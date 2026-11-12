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

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/local"
)

// chatResult is `stormo chat`'s result: the reply, and the session the conversation is in now (a
// compaction can move it; the next turn may use either).
type chatResult struct {
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Reply   string `json:"reply"`
	// Audio is the reply as a voice note, saved for playing (nil: the agent answered in text).
	Audio *chatAudio `json:"audio,omitempty"`
}

// chatAudio is a voice reply saved under .swarm/voice/; WithText: its text goes with it.
type chatAudio struct {
	Path     string `json:"path"`
	MIME     string `json:"mime"`
	WithText bool   `json:"withText"`
}

// sendChat runs one turn: through the engine's Exchanger when it has one, so a voice reply comes
// back as an MP3 saved under .swarm/voice/.
func sendChat(ctx context.Context, inst *instance.Instance, agent string, conv engine.Conversations, session, msg string) (*chatResult, error) {
	ex, ok := conv.(engine.Exchanger)
	if !ok {
		reply, tip, err := conv.Send(ctx, session, msg)
		if err != nil {
			return nil, err
		}
		return &chatResult{agent, tip, reply, nil}, nil
	}
	r, err := ex.Exchange(ctx, session, engine.Input{Text: msg, AudioFormat: "mp3"})
	if err != nil {
		return nil, err
	}
	out := &chatResult{agent, r.Tip, r.Text, nil}
	if a := r.Audio; a != nil {
		sum := sha256.Sum256(a.Data)
		dir := filepath.Join(inst.Root, ".swarm", "voice")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-%s.mp3", agent, hex.EncodeToString(sum[:8])))
		if err := os.WriteFile(path, a.Data, 0o600); err != nil {
			return nil, err
		}
		out.Audio = &chatAudio{Path: path, MIME: a.MIME, WithText: a.WithText}
	}
	return out, nil
}

type conversationsResult struct {
	Agent         string                `json:"agent"`
	Conversations []engine.Conversation `json:"conversations"`
}

type transcriptResult struct {
	Agent string `json:"agent"`
	engine.Transcript
}

type clearResult struct {
	Agent   string   `json:"agent"`
	Cleared []string `json:"cleared"`
	Removed int      `json:"removed"` // sessions removed, compaction continuations included
}

// chatErr gives a conversation failure the code a caller acts on.
func chatErr(err error) error {
	switch {
	case errors.Is(err, local.ErrNotRunning):
		return withCode("not_running", err)
	case errors.Is(err, engine.ErrBusy):
		return withCode("busy", err)
	case errors.Is(err, engine.ErrNotFound):
		return withCode("not_found", err)
	}
	return err
}

func chatCmd(inst *instance.Instance, sub string, rest []string) error {
	id, err := need(sub, "agent")
	if err != nil {
		return err
	}
	msg := strings.Join(rest, " ")
	if msg == "-" {
		// The message from stdin: a message could be the literal `--json` (docs/api.md).
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			return err
		}
		msg = string(b)
	}
	if msg, err = need(strings.TrimSpace(msg), "message"); err != nil {
		return err
	}
	conv, err := local.Conversations(inst, id)
	if err != nil {
		return err
	}
	r, err := sendChat(context.Background(), inst, id, conv, *fSession, msg)
	if err != nil {
		return chatErr(err)
	}
	result(r, func(w io.Writer) {
		fmt.Fprintln(w, r.Reply)
		if r.Audio != nil {
			fmt.Fprintln(w, "(voice note: "+r.Audio.Path+")")
		}
	})
	return nil
}

func conversationsCmd(inst *instance.Instance, sub string, rest []string) error {
	ctx := context.Background()
	verb := sub
	if verb == "new" || verb == "clear" {
		if len(rest) == 0 {
			return withCode("usage", fmt.Errorf("usage: stormo conversations %s <agent>", verb))
		}
		sub, rest = rest[0], rest[1:]
	} else {
		verb = "list"
	}
	id, err := need(sub, "agent")
	if err != nil {
		return err
	}
	conv, err := local.Conversations(inst, id)
	if err != nil {
		return err
	}
	switch {
	case verb == "new":
		sid, err := conv.New(ctx, "", *fName)
		if err != nil {
			return chatErr(err)
		}
		t := engine.Transcript{ID: sid, Tip: sid, Messages: []engine.Message{}}
		result(transcriptResult{id, t}, func(w io.Writer) { fmt.Fprintln(w, sid) })
		return nil

	case verb == "clear":
		var ids []string
		switch {
		case *fAll && len(rest) == 0:
			// Every conversation started through the API (stormo and the app); a Slack thread's or a
			// schedule's session belongs to that channel and is cleared there.
			list, err := conv.List(ctx, 200)
			if err != nil {
				return chatErr(err)
			}
			for _, c := range list {
				if c.Source == "api_server" {
					ids = append(ids, c.ID)
				}
			}
		case !*fAll && len(rest) == 1:
			ids = rest
		default:
			return withCode("usage", errors.New("usage: stormo conversations clear <agent> <session>|--all"))
		}
		r := clearResult{Agent: id, Cleared: []string{}}
		for _, sid := range ids {
			n, err := conv.Delete(ctx, sid)
			if err != nil {
				return chatErr(err)
			}
			if n > 0 {
				r.Cleared = append(r.Cleared, sid)
				r.Removed += n
			}
		}
		if len(rest) == 1 && len(r.Cleared) == 0 {
			return withCode("not_found", fmt.Errorf("%s: %s: %w", id, rest[0], engine.ErrNotFound))
		}
		result(r, func(w io.Writer) { fmt.Fprintf(w, "cleared %d conversation(s) of %s\n", len(r.Cleared), id) })
		return nil

	case len(rest) == 1:
		t, err := conv.Transcript(ctx, rest[0], 0)
		if err != nil {
			return chatErr(err)
		}
		result(transcriptResult{id, *t}, func(w io.Writer) {
			if t.Compacted {
				fmt.Fprintln(w, "(earlier turns compacted into a summary)")
			}
			for _, m := range t.Messages {
				if m.Kind == engine.MessageTool {
					fmt.Fprintf(w, "  · %s%s\n", m.Tool, strings.Join(m.Tools, ", "))
					continue
				}
				fmt.Fprintf(w, "%s: %s\n\n", m.Role, m.Content)
			}
		})
		return nil

	case len(rest) == 0:
		list, err := conv.List(ctx, 0)
		if err != nil {
			return chatErr(err)
		}
		result(conversationsResult{id, list}, func(w io.Writer) {
			for _, c := range list {
				title := c.Title
				if title == "" {
					title = c.Preview
				}
				fmt.Fprintf(w, "%-34s %-10s %4d  %s\n", c.ID, c.Source, c.Messages, title)
			}
		})
		return nil
	}
	return errUsage
}
