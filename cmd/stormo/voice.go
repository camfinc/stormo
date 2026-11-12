package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/elevenlabs"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
)

// voiceShowResult is an agent's voice as agent.yaml sets it and as it picked one for itself
// (a setting proposal waiting for review).
type voiceShowResult struct {
	Agent string          `json:"agent"`
	Voice *manifest.Voice `json:"voice"`
	// Proposed is the voice the agent picked for itself, waiting for review (nil: none).
	Proposed *proposedVoice `json:"proposed"`
	// KeySet: the voice connection's key has a value for this agent.
	KeySet bool `json:"keySet"`
}

type proposedVoice struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// voiceTryResult is a sample of the agent's voice saved for playing.
type voiceTryResult struct {
	Agent string `json:"agent"`
	Voice string `json:"voice"`
	Text  string `json:"text"`
	Path  string `json:"path"`
	Chars int    `json:"chars"`
}

// pickPrompt asks an agent to pick its own voice (the voice tool's search and choose).
const pickPrompt = "Pick your own voice now. Call voice with action search (in your language; try a few filters), " +
	"listen to who you are (your soul and your persona's voice), and choose the one that fits you with action choose, with why. " +
	"Avoid a voice another agent uses. Then reply in one line: the voice's name and why."

func voiceCmd(inst *instance.Instance, sub string, rest []string) error {
	verb := sub
	id, err := need(strings.Join(rest, " "), "agent")
	if err != nil {
		return err
	}
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return err
	}
	switch verb {
	case "show":
		r, err := voiceShow(inst, a)
		if err != nil {
			return err
		}
		result(r, func(w io.Writer) {
			switch {
			case r.Voice == nil:
				fmt.Fprintf(w, "%s has no voice (agent.yaml voice:)\n", id)
			case r.Voice.ID != "":
				fmt.Fprintf(w, "%s speaks as %s (%s) through %s\n", id, orName(r.Voice.Name, r.Voice.ID), r.Voice.ID, r.Voice.Provider)
			case r.Proposed != nil:
				fmt.Fprintf(w, "%s picked %s (%s), waiting for review\n", id, orName(r.Proposed.Name, r.Proposed.ID), r.Proposed.ID)
			default:
				fmt.Fprintf(w, "%s has no voice chosen yet: it picks one (stormo voice pick %s) or you choose\n", id, id)
			}
		})
		return nil
	case "try":
		r, err := voiceTry(inst, a, *fText)
		if err != nil {
			return withCode("unavailable", err)
		}
		result(r, func(w io.Writer) { fmt.Fprintln(w, r.Path) })
		return nil
	case "pick":
		if a.Voice == nil || a.Voice.Speak == manifest.SpeakNever {
			return withCode("invalid", fmt.Errorf("%s does not speak: agent.yaml voice: with speak: auto first", id))
		}
		if a.Voice.ID != "" {
			return withCode("invalid", fmt.Errorf("%s speaks as %s already: clear voice.id to let it pick", id, orName(a.Voice.Name, a.Voice.ID)))
		}
		conv, err := local.Conversations(inst, id)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		sid, err := conv.New(ctx, "", "Picking a voice")
		if err != nil {
			return chatErr(err)
		}
		reply, tip, err := conv.Send(ctx, sid, pickPrompt)
		if err != nil {
			return chatErr(err)
		}
		result(chatResult{id, tip, reply}, func(w io.Writer) { fmt.Fprintln(w, reply) })
		return nil
	}
	return errUsage
}

func orName(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

func voiceShow(inst *instance.Instance, a *manifest.Agent) (*voiceShowResult, error) {
	r := &voiceShowResult{Agent: a.ID, Voice: a.Voice}
	if a.Voice == nil {
		return r, nil
	}
	props, err := loop.ListSettingProposals(inst.Root, a.ID)
	if err != nil {
		return nil, err
	}
	for _, p := range props {
		switch p.Key {
		case "voice.id":
			if r.Proposed == nil {
				r.Proposed = &proposedVoice{}
			}
			r.Proposed.ID = p.Value
		case "voice.name":
			if r.Proposed == nil {
				r.Proposed = &proposedVoice{}
			}
			r.Proposed.Name = p.Value
		}
	}
	if r.Proposed != nil && r.Proposed.ID == "" {
		r.Proposed = nil
	}
	if c, ok := inst.Connection(a.Voice.Provider); ok {
		key, err := connectionKey(inst, c, a.ID)
		if err != nil {
			return nil, err
		}
		r.KeySet = key != ""
	}
	return r, nil
}

// sampleLine is what a sample says when none is given: the agent introducing itself, in its
// language where one is set (Spanish or English; others get English).
func sampleLine(a *manifest.Agent) string {
	if a.Voice != nil && a.Voice.Language == "es" {
		return fmt.Sprintf("Hola, soy %s. Así sueno cuando te respondo con una nota de voz.", a.Name)
	}
	return fmt.Sprintf("Hi, I'm %s. This is how I sound when I answer with a voice note.", a.Name)
}

// voiceTry speaks a short line in the agent's voice (agent.yaml's, else the one it picked) and
// saves it under .swarm/voice/ for playing. It costs the provider's characters.
func voiceTry(inst *instance.Instance, a *manifest.Agent, text string) (*voiceTryResult, error) {
	if a.Voice == nil {
		return nil, fmt.Errorf("%s has no voice (agent.yaml voice:)", a.ID)
	}
	show, err := voiceShow(inst, a)
	if err != nil {
		return nil, err
	}
	voiceID := a.Voice.ID
	if voiceID == "" && show.Proposed != nil {
		voiceID = show.Proposed.ID
	}
	if voiceID == "" {
		return nil, errors.New("no voice chosen yet: choose one, or let the agent pick (stormo voice pick)")
	}
	if text = strings.TrimSpace(text); text == "" {
		text = sampleLine(a)
	}
	if len([]rune(text)) > 300 {
		return nil, errors.New("a sample is at most 300 characters")
	}
	client, err := voiceClient(inst, a.Voice.Provider, a.ID)
	if err != nil {
		return nil, err
	}
	s := elevenlabs.Speech{Text: text, Voice: voiceID, Model: a.Voice.Model, Format: elevenlabs.FormatMP3}
	if st := a.Voice.Settings; st != nil {
		s.Stability, s.Similarity = st.Stability, st.Similarity
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	audio, err := client.Speak(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Voice.Provider, err)
	}
	sum := sha256.Sum256([]byte(voiceID + "\n" + a.Voice.Model + "\n" + text))
	dir := filepath.Join(inst.Root, ".swarm", "voice")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.mp3", a.ID, hex.EncodeToString(sum[:6])))
	if err := os.WriteFile(path, audio.Data, 0o600); err != nil {
		return nil, err
	}
	return &voiceTryResult{Agent: a.ID, Voice: voiceID, Text: text, Path: path, Chars: audio.Chars}, nil
}
