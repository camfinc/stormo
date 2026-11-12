// Package voice is how the agent speaks (docs/needletail.md §4.9): the voice notes it may answer
// with when its config has tts:. The agent decides per reply (the voice tool's reply); the
// gateway that runs the turn takes the request and posts the reply as audio, with its text when
// the agent asks or when the audio left something out. What is spoken is the reply after the
// before_send handlers and the redactor, made speakable (no markdown, code, tables or URLs), and
// bounded by the longest note. The voice is the config's, else the one the agent picked itself
// (settings-v1.json, proposed for review by the dream).
package voice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/pkg/elevenlabs"
)

// CharsPerSecond is how much text a second of speech carries, near enough for the bound.
const CharsPerSecond = 15

// DefaultMaxSeconds bounds a note when the config does not.
const DefaultMaxSeconds = 120

// Speaker speaks in the agent's voice.
type Speaker struct {
	Client *elevenlabs.Client
	// Voice and VoiceName are the config's (tts.voice); empty: the agent's own pick, if any.
	Voice, VoiceName      string
	Model                 string
	Stability, Similarity *float64
	MaxSeconds            int
	// Home is the agent's home: its pick is kept in SettingsFile there.
	Home string
	Logf func(format string, a ...any)

	mu     sync.Mutex
	picked *Pick
	// seen are the voices the last search showed (id: name), what choose may take.
	seen map[string]string
}

// Load reads the agent's own pick from its home. A boot restores the pick's State copy, not the
// settings file (Learning, which only the dream reads), so Load writes that back from the copy:
// the pick stays in use and in review across restarts.
func (s *Speaker) Load() {
	p, err := ReadSettings(s.Home)
	if err != nil {
		s.logf("voice: %s unreadable: %v", SettingsFile, err)
		return
	}
	if p == nil {
		b, err := os.ReadFile(filepath.Join(s.Home, filepath.FromSlash(StateFile)))
		if err == nil {
			if p, err = ParseSettings(b); err == nil && p != nil {
				if err := writeFile(s.Home, SettingsFile, b); err != nil {
					s.logf("voice: %s not written back: %v", SettingsFile, err)
				}
			}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logf("voice: %s unreadable: %v", StateFile, err)
		}
	}
	s.mu.Lock()
	s.picked = p
	s.mu.Unlock()
}

func (s *Speaker) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// Current is the voice in use: the config's, else the agent's pick; "" when it has none yet.
func (s *Speaker) Current() (id, name string, own bool) {
	if s.Voice != "" {
		return s.Voice, s.VoiceName, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.picked != nil {
		return s.picked.Voice, s.picked.Name, true
	}
	return "", "", false
}

// Limit is the longest note, in seconds.
func (s *Speaker) Limit() int {
	if s.MaxSeconds > 0 {
		return s.MaxSeconds
	}
	return DefaultMaxSeconds
}

// Request is a turn's ask to answer with a voice note.
type Request struct {
	// WithText sends the reply's text with the audio.
	WithText bool
	// Say is what to speak when it differs from the written reply ("": the reply).
	Say string
}

// ErrNoVoice: the agent has no voice yet.
var ErrNoVoice = errors.New("you have no voice yet: search for one that fits you (voice action search) and choose it (action choose)")

// Prepare is what a voice reply speaks for text (the reply as posted: after the handlers and the
// redactor) and whether the text goes with it: the agent's ask, or anyway when the audio leaves
// out what only reads (code, tables, links).
func (s *Speaker) Prepare(text string, q Request) (spoken string, withText bool, err error) {
	if id, _, _ := s.Current(); id == "" {
		return "", false, ErrNoVoice
	}
	source := text
	if strings.TrimSpace(q.Say) != "" {
		source = q.Say
	}
	spoken, left := Speakable(source)
	if spoken == "" {
		return "", false, errors.New("nothing in the reply can be spoken")
	}
	if err := s.Fits(spoken); err != nil {
		return "", false, err
	}
	return spoken, q.WithText || (left && strings.TrimSpace(q.Say) == ""), nil
}

// Fits is whether spoken stays within the longest note.
func (s *Speaker) Fits(spoken string) error {
	if n := utf8.RuneCountInString(spoken); n > s.Limit()*CharsPerSecond {
		return fmt.Errorf("a voice note is at most %d seconds (about %d characters) and this is about %d s: say less (voice reply's say) or answer in text",
			s.Limit(), s.Limit()*CharsPerSecond, n/CharsPerSecond)
	}
	return nil
}

// Speak is spoken (already Prepared) in the agent's voice, as format (elevenlabs.FormatOpus for
// a voice note, FormatMP3 for an audio file).
func (s *Speaker) Speak(ctx context.Context, spoken, format string) (*elevenlabs.Audio, error) {
	id, _, _ := s.Current()
	if id == "" {
		return nil, ErrNoVoice
	}
	a, err := s.Client.Speak(ctx, elevenlabs.Speech{Text: spoken, Voice: id, Model: s.Model, Stability: s.Stability, Similarity: s.Similarity, Format: format})
	if err != nil {
		return nil, err
	}
	s.logf("voice: spoke %d characters (%d billed) as %s", utf8.RuneCountInString(spoken), a.Chars, a.MIME)
	return a, nil
}

// Replies are the turns that asked to answer with a voice note, by conversation.
type Replies struct {
	// Root is a session's conversation (sessions.Store.Root; nil: the session itself), so a
	// request made from a compacted session reaches the turn's gateway.
	Root func(session string) (string, bool)
	mu   sync.Mutex
	m    map[string]Request
}

func (r *Replies) key(session string) string {
	if r.Root != nil {
		if root, ok := r.Root(session); ok {
			return root
		}
	}
	return session
}

// Ask records that session's turn answers with a voice note.
func (r *Replies) Ask(session string, q Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]Request{}
	}
	r.m[r.key(session)] = q
}

// Take is session's request, removed: the gateway takes it once, when it posts the turn's reply.
func (r *Replies) Take(session string) (Request, bool) {
	if r == nil {
		return Request{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.key(session)
	q, ok := r.m[k]
	delete(r.m, k)
	return q, ok
}

// Drop forgets session's request (a turn that ends without a reply).
func (r *Replies) Drop(session string) { _, _ = r.Take(session) }

var (
	fenceRe      = regexp.MustCompile("(?s)```.*?(```|$)")
	slackLinkRe  = regexp.MustCompile(`<(https?://[^|>]+)\|([^>]+)>`)
	slackBareRe  = regexp.MustCompile(`<(https?://[^>]+)>`)
	mdLinkRe     = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)]+)\)`)
	urlRe        = regexp.MustCompile(`https?://\S+`)
	headingRe    = regexp.MustCompile(`^#{1,6}\s+`)
	bulletRe     = regexp.MustCompile(`^(?:[-*•]|\d+[.)])\s+`)
	emphasisRe   = regexp.MustCompile(`\*\*|__|~~|\*`)
	inlineCodeRe = regexp.MustCompile("`([^`]*)`")
	spacesRe     = regexp.MustCompile(`[ \t]+`)
)

// Speakable is text as it can be spoken: code blocks, tables and URLs left out (left reports
// that something was), links read as their text, headings, list markers and emphasis dropped.
func Speakable(text string) (spoken string, left bool) {
	if fenceRe.MatchString(text) {
		text, left = fenceRe.ReplaceAllString(text, " "), true
	}
	if slackLinkRe.MatchString(text) || mdLinkRe.MatchString(text) {
		left = true
	}
	text = slackLinkRe.ReplaceAllString(text, "$2")
	text = mdLinkRe.ReplaceAllString(text, "$1")
	if slackBareRe.MatchString(text) || urlRe.MatchString(text) {
		left = true
	}
	text = slackBareRe.ReplaceAllString(text, "")
	text = urlRe.ReplaceAllString(text, "")
	text = inlineCodeRe.ReplaceAllString(text, "$1")
	out := []string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "|") {
			left = true // a table reads, it does not speak
			continue
		}
		line = headingRe.ReplaceAllString(line, "")
		line = bulletRe.ReplaceAllString(line, "")
		line = emphasisRe.ReplaceAllString(line, "")
		line = strings.TrimSpace(spacesRe.ReplaceAllString(line, " "))
		if line == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n")), left
}

// SettingsFile is the agent's runtime settings in its home: the voice it picked. The runtime
// writes it (the voice tool's choose); the dream proposes what it holds for review, and accepting
// writes it to agent.yaml voice:. Napped as Learning, so a boot does not restore it: StateFile is
// its copy that does.
const SettingsFile = "state/needletail/settings-v1.json"

// StateFile is SettingsFile's copy kept as State (restored at boot), the same format.
const StateFile = "state/needletail/voice-v1.json"

// Pick is a voice the agent chose for itself, and why.
type Pick struct {
	Voice  string `json:"id"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ReadSettings is the pick SettingsFile under home holds (nil: none).
func ReadSettings(home string) (*Pick, error) {
	b, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(SettingsFile)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseSettings(b)
}

// writeSettings keeps p in SettingsFile and its StateFile copy under home.
func writeSettings(home string, p *Pick) error {
	body, err := MarshalSettings(p)
	if err != nil {
		return err
	}
	if err := writeFile(home, StateFile, body); err != nil {
		return err
	}
	return writeFile(home, SettingsFile, body)
}

func writeFile(home, rel string, body []byte) error {
	path := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteAtomic(path, body, 0o600)
}
