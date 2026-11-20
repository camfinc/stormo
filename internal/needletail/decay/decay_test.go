package decay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

const conv = "whatsapp-00aa"

// marked is a store holding a conversation that read a web page an hour ago: the page, the
// agent's marked answer, a swarm message it read, an outsider's message.
func marked(t *testing.T) *sessions.Store {
	t.Helper()
	s, err := sessions.Open(filepath.Join(t.TempDir(), "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(wire.Session{ID: conv, Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	rows := []wire.Message{
		{Role: "user", Content: "noticias recientes?"},
		{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "c1", Name: "web_extract", Arguments: `{"url":"https://example.org"}`}}},
	}
	if _, err := s.Append(conv, rows...); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaint(conv, wire.Taint{Why: "read a web page (web_extract)"}); err != nil {
		t.Fatal(err)
	}
	rows = []wire.Message{
		{Role: "tool", ToolCallID: "c1", Content: "PAGE TEXT", Untrusted: true},
		{Role: "assistant", Content: "Cuatro titulares.", Untrusted: true, ToolCalls: []wire.ToolCall{{ID: "c2", Name: "mcp_core_msg_read", Arguments: `{"id":7}`}}},
		{Role: "tool", ToolCallID: "c2", Content: `{"body":"brief from ops"}`, Untrusted: true},
	}
	if _, err := s.Append(conv, rows...); err != nil {
		t.Fatal(err)
	}
	return s
}

// screenOf answers every part with the given probabilities (questions not named: 0.05) and
// records what it was asked.
func screenOf(p map[string]float64, asked *[]json.RawMessage) Screen {
	return func(_ context.Context, capability string, input json.RawMessage, _ time.Duration) (json.RawMessage, error) {
		if capability != "decision.ask" {
			return nil, fmt.Errorf("capability %q", capability)
		}
		*asked = append(*asked, input)
		var in struct {
			States map[string]json.RawMessage `json:"states"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err
		}
		res := map[string]any{}
		for id := range in.States {
			ans := map[string]any{}
			for _, q := range Questions {
				v, ok := p[q.Name]
				if !ok {
					v = 0.05
				}
				ans[q.Name] = map[string]any{"type": "noul", "noul": v}
			}
			res[id] = map[string]any{"answers": ans}
		}
		return json.Marshal(map[string]any{"results": res, "decided": len(res)})
	}
}

func decayOf(s *sessions.Store, sc Screen, audit *[]map[string]any) *Decay {
	return &Decay{Store: s, Cfg: wire.EndorseConfig{Screen: "decision.ask"}, Screen: sc,
		Audit: func(e map[string]any) { *audit = append(*audit, e) },
		Now:   func() time.Time { return time.Now().Add(time.Hour) }}
}

// A conversation quiet of new marked data for After: the outsiders' words go out of context, the
// rest is screened, and a pass endorses it by the screen, audited.
func TestDecayEndorsesWhatPassesTheScreen(t *testing.T) {
	s := marked(t)
	var asked []json.RawMessage
	var audit []map[string]any
	d := decayOf(s, screenOf(nil, &asked), &audit)
	if !d.Due(conv) {
		t.Fatal("not due an hour after the read")
	}
	out, err := d.Run(context.Background(), conv, time.Second)
	if err != nil || !out.Endorsed || out.Held != "" || out.Evicted != 5 {
		t.Fatalf("run: %+v %v", out, err)
	}
	if s.Untrusted(conv) {
		t.Fatal("still marked")
	}
	if h, _ := s.Get(conv); h.EndorsedBy != "screen:decision.ask" {
		t.Fatalf("endorsed by %q", h.EndorsedBy)
	}
	if len(asked) != 1 {
		t.Fatalf("screened %d times", len(asked))
	}
	in := string(asked[0])
	for _, want := range []string{"Cuatro titulares.", "brief from ops", "mcp_core_msg_read", "noticias recientes?"} {
		if !strings.Contains(in, want) {
			t.Errorf("the screen was not given %q", want)
		}
	}
	if strings.Contains(in, "PAGE TEXT") {
		t.Error("the page went to the screen: it is out of context, never judged")
	}
	if len(audit) != 1 || audit[0]["event"] != "trusted" || audit[0]["by"] != "screen:decision.ask" || audit[0]["conversation"] != conv {
		t.Fatalf("audit: %v", audit)
	}
	if d.Due(conv) {
		t.Fatal("due again once endorsed")
	}
}

// A question at its threshold holds the conversation marked (the outsiders' words still go out of
// context), audited with why, and it is tried again only after After.
func TestDecayHoldsWhatTheScreenFlags(t *testing.T) {
	s := marked(t)
	var asked []json.RawMessage
	var audit []map[string]any
	d := decayOf(s, screenOf(map[string]float64{"exfiltrate": 0.91, "contact": 0.79}, &asked), &audit)
	out, err := d.Run(context.Background(), conv, time.Second)
	if err != nil || out.Endorsed || !strings.Contains(out.Held, "exfiltrate 0.91") || strings.Contains(out.Held, "contact") {
		t.Fatalf("run: %+v %v", out, err)
	}
	if !s.Untrusted(conv) || s.Evicted(conv) != 5 {
		t.Fatal("held: marked, with its outsiders' words out of context")
	}
	if len(audit) != 1 || audit[0]["event"] != "screen_held" {
		t.Fatalf("audit: %v", audit)
	}
	if d.Due(conv) {
		t.Fatal("tried again before After")
	}
	// a lower threshold of its own (approvals.endorse.thresholds) flags contact too
	d2 := decayOf(s, screenOf(map[string]float64{"contact": 0.79}, &asked), &audit)
	d2.Cfg.Thresholds = map[string]float64{"contact": 0.6}
	if out, _ := d2.Run(context.Background(), conv, time.Second); out.Endorsed || !strings.Contains(out.Held, "contact 0.79") {
		t.Fatalf("threshold override: %+v", out)
	}
}

// No decision is never a pass: a part without answers, a refused question, an unreadable answer.
// A screen that cannot be reached is an error, tried again soon (not after After).
func TestDecayNeedsADecision(t *testing.T) {
	all := func(answer string) string { // every question answered the same way
		var parts []string
		for _, q := range Questions {
			parts = append(parts, fmt.Sprintf("%q:%s", q.Name, answer))
		}
		return `{"results":{"1":{"answers":{` + strings.Join(parts, ",") + `}}}}`
	}
	cases := map[string]string{
		"no part":          `{"results":{}}`,
		"refused":          `{"results":{"1":{"answers":{"override":{"type":"refus"}}}}}`,
		"an error":         `{"results":{"1":{"error":"model unavailable"}}}`,
		"junk":             `not json`,
		"refused with a 0": all(`{"type":"refus","noul":0}`), // Codex review #4
		"a negative":       all(`{"type":"noul","noul":-1}`),
		"over 1":           all(`{"type":"noul","noul":1.5}`),
		"not a number":     all(`{"type":"noul","noul":"0.1"}`),
		"untyped":          all(`{"noul":0.1}`),
		"null":             all(`{"type":"noul","noul":null}`), // Codex review, second pass
	}
	for name, answer := range cases {
		s := marked(t)
		var audit []map[string]any
		d := decayOf(s, func(context.Context, string, json.RawMessage, time.Duration) (json.RawMessage, error) {
			return json.RawMessage(answer), nil
		}, &audit)
		if out, err := d.Run(context.Background(), conv, time.Second); err != nil || out.Endorsed || out.Held == "" {
			t.Errorf("%s: %+v %v", name, out, err)
		}
	}
	s := marked(t)
	var audit []map[string]any
	d := decayOf(s, func(context.Context, string, json.RawMessage, time.Duration) (json.RawMessage, error) {
		return nil, errors.New("job running")
	}, &audit)
	if _, err := d.Run(context.Background(), conv, time.Second); err == nil || !s.Untrusted(conv) {
		t.Fatalf("unreachable screen: %v", err)
	}
	if d.Due(conv) {
		t.Fatal("tried again at once")
	}
	d.Now = func() time.Time { return time.Now().Add(time.Hour + 3*time.Minute) }
	if !d.Due(conv) {
		t.Fatal("not tried again a few minutes later")
	}
}

// Only outsiders' words marked the conversation: once they are out of context nothing marked is
// left to judge, and it is endorsed without a screen. Too much marked text holds it unscreened.
func TestDecayWithoutAScreen(t *testing.T) {
	s, err := sessions.Open(filepath.Join(t.TempDir(), "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(wire.Session{ID: conv, Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(conv, wire.Message{Role: "user", Content: "[unverified] a guest's words", Untrusted: true}, wire.Message{Role: "assistant", Content: "Hola."}); err != nil {
		t.Fatal(err)
	}
	var audit []map[string]any
	d := decayOf(s, func(context.Context, string, json.RawMessage, time.Duration) (json.RawMessage, error) {
		t.Fatal("screened with nothing to judge")
		return nil, nil
	}, &audit)
	if out, err := d.Run(context.Background(), conv, time.Second); err != nil || !out.Endorsed {
		t.Fatalf("nothing to judge: %+v %v", out, err)
	}

	s = marked(t)
	if _, err := s.Append(conv, wire.Message{Role: "assistant", Content: strings.Repeat("palabra ", 600), Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	d = decayOf(s, func(context.Context, string, json.RawMessage, time.Duration) (json.RawMessage, error) {
		t.Fatal("screened past max_bytes")
		return nil, nil
	}, &audit)
	d.Cfg.MaxBytes = 4096
	if out, err := d.Run(context.Background(), conv, time.Second); err != nil || out.Endorsed || !strings.Contains(out.Held, "more than one screen judges") {
		t.Fatalf("past max_bytes: %+v %v", out, err)
	}
}

// Marked data that comes in while the screen runs refuses the endorsement (it never saw it).
func TestDecayRefusesWhatCameInMeanwhile(t *testing.T) {
	s := marked(t)
	var audit []map[string]any
	var asked []json.RawMessage
	pass := screenOf(nil, &asked)
	d := decayOf(s, func(ctx context.Context, c string, in json.RawMessage, w time.Duration) (json.RawMessage, error) {
		if err := s.AddTaint(conv, wire.Taint{Why: "read a web page (web_extract)"}); err != nil {
			return nil, err
		}
		return pass(ctx, c, in, w)
	}, &audit)
	out, err := d.Run(context.Background(), conv, time.Second)
	if err != nil || out.Endorsed || !s.Untrusted(conv) {
		t.Fatalf("run: %+v %v", out, err)
	}
}

// Long marked text is judged whole, in parts under the decision model's accurate size, each with
// the people's words; a pass needs every part.
func TestDecayJudgesLongTextInParts(t *testing.T) {
	s := marked(t)
	long := strings.Repeat("Resumen de la investigación ñ. ", 2500) // ~78 KB
	if _, err := s.Append(conv, wire.Message{Role: "assistant", Content: long + "TAIL-MARKER", Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	var asked []json.RawMessage
	var audit []map[string]any
	d := decayOf(s, screenOf(nil, &asked), &audit)
	if out, err := d.Run(context.Background(), conv, time.Second); err != nil || !out.Endorsed {
		t.Fatalf("run: %+v %v", out, err)
	}
	var in struct {
		States map[string]struct {
			People []string `json:"people"`
			Judge  []Item   `json:"judge"`
		} `json:"states"`
	}
	if err := json.Unmarshal(asked[0], &in); err != nil {
		t.Fatal(err)
	}
	if len(in.States) < 4 {
		t.Fatalf("%d parts", len(in.States))
	}
	whole := ""
	for i := 1; i <= len(in.States); i++ {
		p := in.States[fmt.Sprint(i)]
		n := 0
		for _, j := range p.Judge {
			n += len(j.Text)
			whole += j.Text
		}
		if n > partBytes || len(p.People) != 1 {
			t.Fatalf("part %d: %d bytes, people %v", i, n, p.People)
		}
	}
	if !strings.Contains(whole, "TAIL-MARKER") || !strings.Contains(whole, long[:100]) {
		t.Fatal("the long text was not judged whole")
	}

	// one part flagged holds it all
	s = marked(t)
	if _, err := s.Append(conv, wire.Message{Role: "assistant", Content: long, Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	d = decayOf(s, func(_ context.Context, _ string, input json.RawMessage, _ time.Duration) (json.RawMessage, error) {
		calls++
		var in struct {
			States map[string]json.RawMessage `json:"states"`
		}
		_ = json.Unmarshal(input, &in)
		res := map[string]any{}
		for id := range in.States {
			ans := map[string]any{}
			for _, q := range Questions {
				v := 0.05
				if id == "2" && q.Name == "override" {
					v = 0.93
				}
				ans[q.Name] = map[string]any{"type": "noul", "noul": v}
			}
			res[id] = map[string]any{"answers": ans}
		}
		return json.Marshal(map[string]any{"results": res})
	}, &audit)
	if out, _ := d.Run(context.Background(), conv, time.Second); out.Endorsed || calls != 1 {
		t.Fatalf("a flagged part: %+v (%d calls)", out, calls)
	}
}

// The screen's questions are the keys approvals.endorse.thresholds takes (wire's list), each with a
// threshold strictly between 0 and 1.
func TestQuestionsMatchTheConfig(t *testing.T) {
	var names []string
	for _, q := range Questions {
		names = append(names, q.Name)
		if q.Threshold <= 0 || q.Threshold >= 1 || q.Instructions == "" || q.True == "" || q.False == "" {
			t.Errorf("%s: %+v", q.Name, q)
		}
	}
	if !slices.Equal(names, wire.EndorseQuestions) {
		t.Fatalf("%v != wire.EndorseQuestions %v", names, wire.EndorseQuestions)
	}
}

func TestPiecesNeverSplitARune(t *testing.T) {
	s := strings.Repeat("añ", 10)
	for _, p := range pieces(s, 3) {
		if len(p) > 3 || !strings.HasPrefix(s[strings.Index(s, p):], p) || !utfValid(p) {
			t.Fatalf("piece %q", p)
		}
	}
	if strings.Join(pieces(s, 3), "") != s {
		t.Fatal("pieces lose text")
	}
}

func utfValid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// Codex review #1: a conversation a gateway marked for words outside its rows is held for a person,
// unscreened; #3: a gateway's mark or a row that comes in while the screen runs refuses its pass.
func TestDecayHoldsWhatItCannotSee(t *testing.T) {
	s := marked(t)
	if err := s.MarkOutside(conv); err != nil {
		t.Fatal(err)
	}
	var audit []map[string]any
	d := decayOf(s, func(context.Context, string, json.RawMessage, time.Duration) (json.RawMessage, error) {
		t.Fatal("screened what it cannot see")
		return nil, nil
	}, &audit)
	d.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if out, err := d.Run(context.Background(), conv, time.Second); err != nil || out.Endorsed || !strings.Contains(out.Held, "not among its rows") {
		t.Fatalf("marked outside: %+v %v", out, err)
	}

	for name, during := range map[string]func(*sessions.Store) error{
		"a gateway's mark": func(s *sessions.Store) error { return s.MarkUntrusted(conv) },
		"a handler's row": func(s *sessions.Store) error {
			_, err := s.Append(conv, wire.Message{Role: "assistant", Content: "a handler's note"})
			return err
		},
	} {
		s := marked(t)
		var asked []json.RawMessage
		pass := screenOf(nil, &asked)
		d := decayOf(s, func(ctx context.Context, c string, in json.RawMessage, w time.Duration) (json.RawMessage, error) {
			if err := during(s); err != nil {
				return nil, err
			}
			return pass(ctx, c, in, w)
		}, &audit)
		if out, err := d.Run(context.Background(), conv, time.Second); err != nil || out.Endorsed || !s.Untrusted(conv) {
			t.Errorf("%s during the screen: %+v %v", name, out, err)
		}
	}
}
