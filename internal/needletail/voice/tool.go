package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/pkg/elevenlabs"
)

// ToolName is the voice tool.
const ToolName = "voice"

// Picking is what the agent's own pick goes by: its language, the persona's words on its voice,
// and the voices the instance's other agents speak with (id: agent).
type Picking struct {
	Language string
	Persona  string
	Taken    map[string]string
	// Neutralise makes third-party text (a voice's name) inert in the model's input.
	Neutralise func(string) string
}

// maxFound is the most voices a search shows.
const maxFound = 15

// Tool is voice: reply (this turn's reply goes as a voice note), search (voices to pick from,
// when the agent has none) and choose (the one it picks: used at once, proposed for review).
func Tool(s *Speaker, r *Replies, p Picking) tools.Tool {
	return tools.Tool{Name: ToolName, Description: toolDescription(s), Schema: json.RawMessage(toolSchema),
		Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (any, error) {
			var a struct {
				Action   string `json:"action"`
				WithText bool   `json:"with_text"`
				Say      string `json:"say"`
				Query    string `json:"query"`
				Gender   string `json:"gender"`
				Age      string `json:"age"`
				Accent   string `json:"accent"`
				Language string `json:"language"`
				ID       string `json:"id"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("bad arguments: %v", err)
			}
			session := ""
			if env != nil {
				session = env.Session
			}
			switch strings.ToLower(strings.TrimSpace(a.Action)) {
			case "", "reply":
				if session == "" {
					return nil, errors.New("voice reply answers a conversation: there is none here")
				}
				if id, _, _ := s.Current(); id == "" {
					return nil, ErrNoVoice
				}
				if say := strings.TrimSpace(a.Say); say != "" {
					spoken, _ := Speakable(say)
					if spoken == "" {
						return nil, errors.New("say has nothing that can be spoken")
					}
					if err := s.Fits(spoken); err != nil {
						return nil, err
					}
				}
				r.Ask(session, Request{WithText: a.WithText, Say: a.Say})
				note := "Your reply will go as a voice note"
				if a.WithText {
					note += ", with its text"
				}
				return map[string]any{"ok": true, "note": note + fmt.Sprintf(". Keep it under %d seconds (about %d characters) of speech. Now write your reply.", s.Limit(), s.Limit()*CharsPerSecond)}, nil
			case "search":
				return search(ctx, s, p, a.Query, a.Gender, a.Age, a.Accent, a.Language)
			case "choose":
				return choose(s, a.ID, a.Reason)
			}
			return nil, fmt.Errorf("unknown action %q: reply, search or choose", a.Action)
		}}
}

// search lists voices the agent could speak with: the account's, then the Voice Library's,
// those another agent of the instance uses last. Only what labels them (no descriptions, which
// their sharers write): the model picks by these and by who it is.
func search(ctx context.Context, s *Speaker, p Picking, query, gender, age, accent, language string) (any, error) {
	if id, name, own := s.Current(); id != "" && !own {
		return nil, fmt.Errorf("your voice is set by your owner (%s): there is nothing to pick", orID(name, id))
	}
	if language == "" {
		language = p.Language
	}
	q := elevenlabs.Query{Search: query, Gender: gender, Age: age, Accent: accent, Language: language, Limit: 40}
	found, err := s.Client.Voices(ctx, q)
	if err != nil {
		return nil, err
	}
	lib, err := s.Client.Library(ctx, q)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	type row struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Gender   string `json:"gender,omitempty"`
		Age      string `json:"age,omitempty"`
		Accent   string `json:"accent,omitempty"`
		Style    string `json:"style,omitempty"`
		UseCase  string `json:"use_case,omitempty"`
		Language string `json:"language,omitempty"`
		UsedBy   string `json:"used_by,omitempty"`
	}
	rows := []row{}
	clean := func(v string, n int) string {
		if p.Neutralise != nil {
			v = p.Neutralise(v)
		}
		v = strings.Join(strings.Fields(v), " ")
		if utf8.RuneCountInString(v) > n {
			v = string([]rune(v)[:n]) + "…"
		}
		return v
	}
	for _, v := range append(found, lib...) {
		if seen[v.ID] {
			continue
		}
		seen[v.ID] = true
		rows = append(rows, row{ID: v.ID, Name: clean(v.Name, 60), Gender: clean(v.Gender, 20), Age: clean(v.Age, 20), Accent: clean(v.Accent, 30),
			Style: clean(v.Descriptive, 30), UseCase: clean(v.UseCase, 30), Language: clean(v.Language, 10), UsedBy: p.Taken[v.ID]})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].UsedBy == "" && rows[j].UsedBy != "" })
	if len(rows) > maxFound {
		rows = rows[:maxFound]
	}
	s.mu.Lock()
	s.seen = map[string]string{}
	for _, r := range rows {
		s.seen[r.ID] = r.Name
	}
	s.mu.Unlock()
	out := map[string]any{"voices": rows, "note": "Pick the one that sounds like you (your persona and your soul), in your language. " +
		"Avoid a voice another agent uses (used_by) unless none fits. Then call voice with action choose, its id and why."}
	if p.Persona != "" {
		out["your_voice"] = p.Persona
	}
	return out, nil
}

// choose takes a voice from the last search as the agent's own: in use at once, kept across
// restarts, and proposed for review (the owner accepts it into agent.yaml or picks another).
func choose(s *Speaker, id, reason string) (any, error) {
	if cur, name, own := s.Current(); cur != "" {
		if own {
			return nil, fmt.Errorf("you chose %s already; it waits for your owner's review", orID(name, cur))
		}
		return nil, fmt.Errorf("your voice is set by your owner (%s)", orID(name, cur))
	}
	s.mu.Lock()
	name, ok := s.seen[id]
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("choose a voice your last search showed (voice action search)")
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if utf8.RuneCountInString(reason) > 300 {
		reason = string([]rune(reason)[:300])
	}
	pick := &Pick{Voice: id, Name: name, Reason: reason}
	if err := writeSettings(s.Home, pick); err != nil {
		return nil, fmt.Errorf("the choice could not be kept: %v", err)
	}
	s.mu.Lock()
	s.picked = pick
	s.mu.Unlock()
	s.logf("voice: picked %s (%s): %s", id, name, reason)
	return map[string]any{"ok": true, "note": fmt.Sprintf("%s is your voice now. Your owner sees the choice in review and may change it.", name)}, nil
}

func orID(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

func toolDescription(s *Speaker) string {
	return fmt.Sprintf("Answer with a voice note, or pick your voice. action reply: this turn's reply goes as a voice note "+
		"(with_text: send the text with it; say: what to speak when it should differ from what you write). Use it when the person "+
		"asks for audio, or when they sent a voice note and your answer is conversational; not for answers to read or copy "+
		"(lists, steps, links, numbers, code), and not when they asked for text. A note is at most %d seconds. "+
		"Send the text too when the reply has names, numbers, dates, links or steps. "+
		"action search: when you have no voice yet, voices you could speak with (filters: query, gender, age, accent, language). "+
		"action choose: the one that fits you, by id from your last search, with why.", s.Limit())
}

const toolSchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["reply","search","choose"],"description":"reply (default): answer this turn with a voice note; search: voices to pick from; choose: take one as yours."},
"with_text":{"type":"boolean","description":"reply: send the written reply too."},
"say":{"type":"string","description":"reply: what to speak, when it should differ from the written reply (a short spoken version)."},
"query":{"type":"string","description":"search: words in a voice's name or labels."},
"gender":{"type":"string","description":"search: female, male or neutral."},
"age":{"type":"string","description":"search: young, middle_aged or old."},
"accent":{"type":"string","description":"search: an accent (e.g. latin american, peninsular, british)."},
"language":{"type":"string","description":"search: an ISO 639-1 code; your own by default."},
"id":{"type":"string","description":"choose: the voice's id from your last search."},
"reason":{"type":"string","description":"choose: why it fits you (your owner reads it)."}
}}`
