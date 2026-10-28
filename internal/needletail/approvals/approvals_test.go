package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

type fakeNotifier struct {
	mu      sync.Mutex
	asked   []string
	updated []string
	told    []string
	fail    bool
	// block, when set, holds Update until it is closed
	block chan struct{}
}

func (f *fakeNotifier) Ask(_ context.Context, r *Request, who string) (Posted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return Posted{}, errors.New("slack down")
	}
	f.asked = append(f.asked, who+":"+r.ID)
	return Posted{Channel: "D0" + who, TS: "1.000" + r.ID}, nil
}

func (f *fakeNotifier) Update(_ context.Context, r *Request, p Posted) error {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, r.ID+":"+r.Status)
	return nil
}

func (f *fakeNotifier) Tell(_ context.Context, r *Request) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.told = append(f.told, r.ID+":"+r.Status)
	return nil
}

type world struct {
	dir   string
	now   time.Time
	n     *fakeNotifier
	runs  []string
	trust []string
	svc   *Service
}

func newWorld(t *testing.T) *world {
	w := &world{dir: t.TempDir(), now: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC), n: &fakeNotifier{}}
	w.svc = w.service()
	return w
}

func (w *world) service() *Service {
	s := New(Options{StateDir: filepath.Join(w.dir, "state"), TmpDir: filepath.Join(w.dir, "tmp"), Approvers: []string{"U0OWNER001", "U0AUTHOR01"},
		Now: func() time.Time { return w.now },
		Execute: map[string]Executor{KindTool: func(ctx context.Context, r *Request) (string, error) {
			if messaging.Endorsed(ctx) != r.ID || messaging.UntrustedWhy(ctx) != "" {
				return "", errors.New("not endorsed")
			}
			w.runs = append(w.runs, r.Action.Tool+string(r.Action.Args))
			return `{"ok":true}`, nil
		}},
		Trust: func(r *Request, by string) error { w.trust = append(w.trust, r.Conversation+":"+by); return nil },
	})
	s.SetNotifier(w.n)
	return s
}

func ask(conv string) Ask {
	return Ask{Action: Action{Kind: KindTool, Tool: "send_message", Args: json.RawMessage(`{"target": "slack:C0OTHER001", "message": "hi"}`)},
		Conversation: conv, Session: conv, Origin: Origin{Channel: "C0THREAD01", Thread: "1.1"},
		Card: Card{Title: "send a message", Payload: "hi", PayloadKind: "text"}, Why: "tainted", Trust: true,
		Taint: []wire.Taint{{Why: "unverified", Users: []string{"U0AUTHOR01"}}}}
}

func TestApproveRunsExactlyOnce(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	r, err := w.svc.Request(ctx, ask("slack-C0THREAD01-1.1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.n.asked) != 1 || !strings.HasPrefix(w.n.asked[0], "U0OWNER001:") {
		t.Fatalf("the card must reach the owner only (never the taint's author): %v", w.n.asked)
	}
	// the taint's author, even listed as an approver, cannot decide; nor can a stranger
	if _, err := w.svc.Decide(ctx, Decision{ID: r.ID, Hash: r.Hash, By: "U0AUTHOR01", Approve: true}); !errors.Is(err, ErrAuthor) {
		t.Fatalf("author approved: %v", err)
	}
	if _, err := w.svc.Decide(ctx, Decision{ID: r.ID, Hash: r.Hash, By: "U0STRANGER", Approve: true}); !errors.Is(err, ErrApprover) {
		t.Fatalf("stranger approved: %v", err)
	}
	// a card for another payload (hash mismatch) is refused
	if _, err := w.svc.Decide(ctx, Decision{ID: r.ID, Hash: strings.Repeat("0", 64), By: "U0OWNER001", Approve: true}); !errors.Is(err, ErrHash) {
		t.Fatalf("hash mismatch approved: %v", err)
	}
	if len(w.runs) != 0 {
		t.Fatal("ran before approval")
	}
	got, err := w.svc.Decide(ctx, Decision{ID: r.ID, Hash: r.Hash, By: "U0OWNER001", Approve: true, Trust: true})
	if err != nil || got.Status != StatusDone || len(w.runs) != 1 || w.runs[0] != `send_message{"target": "slack:C0OTHER001", "message": "hi"}` {
		t.Fatalf("approve: %+v %v runs %v", got, err, w.runs)
	}
	if len(w.trust) != 1 || w.trust[0] != "slack-C0THREAD01-1.1:U0OWNER001" {
		t.Fatalf("trust thread: %v", w.trust)
	}
	// a replay (a second click, the same button again) never runs it twice
	if _, err := w.svc.Decide(ctx, Decision{ID: r.ID, Hash: r.Hash, By: "U0OWNER001", Approve: true}); !errors.Is(err, ErrDecided) || len(w.runs) != 1 {
		t.Fatalf("replay: %v runs %v", err, w.runs)
	}
	if len(w.n.updated) != 1 || len(w.n.told) != 1 {
		t.Fatalf("card update %v, told %v", w.n.updated, w.n.told)
	}
	audit, _ := os.ReadFile(filepath.Join(w.dir, "state", "approvals-audit.jsonl"))
	for _, ev := range []string{`"requested"`, `"refused_author"`, `"refused_approver"`, `"refused_hash"`, `"approved"`, `"trusted"`, `"done"`} {
		if !strings.Contains(string(audit), ev) {
			t.Errorf("audit lacks %s:\n%s", ev, audit)
		}
	}
	if strings.Contains(string(audit), "hi\"") || strings.Contains(string(audit), "C0OTHER001") {
		t.Errorf("the audit holds payload text:\n%s", audit)
	}
	rows, done := w.svc.Notes("slack-C0THREAD01-1.1")
	if len(rows) != 1 || rows[0].Kind != wire.KindApproval || !strings.Contains(rows[0].Content, "approved by U0OWNER001") {
		t.Fatalf("notes %+v", rows)
	}
	done()
	if rows, _ := w.svc.Notes("slack-C0THREAD01-1.1"); len(rows) != 0 {
		t.Fatal("a note is given twice")
	}
}

func TestDenyTimeoutAndRestart(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	a, _ := w.svc.Request(ctx, ask("conv-a"))
	b, _ := w.svc.Request(ctx, ask("conv-b"))
	c, _ := w.svc.Request(ctx, ask("conv-c"))
	if got, err := w.svc.Decide(ctx, Decision{ID: a.ID, Hash: a.Hash, By: "U0OWNER001"}); err != nil || got.Status != StatusDenied {
		t.Fatalf("deny: %+v %v", got, err)
	}
	// b times out: denied
	w.now = w.now.Add(31 * time.Minute)
	w.svc.Expire(ctx)
	if got, _ := w.svc.Get(b.ID); got.Status != StatusExpired {
		t.Fatalf("timeout: %+v", got)
	}
	if _, err := w.svc.Decide(ctx, Decision{ID: b.ID, Hash: b.Hash, By: "U0OWNER001", Approve: true}); !errors.Is(err, ErrDecided) {
		t.Fatalf("an expired request approved: %v", err)
	}
	// a restart keeps a pending request (c was made 31 min ago: past due, denied at boot) and
	// a fresh one
	d, _ := w.svc.Request(ctx, ask("conv-d"))
	w.now = w.now.Add(time.Minute)
	w.svc = w.service()
	if err := w.svc.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.svc.Get(c.ID); got.Status != StatusExpired {
		t.Fatalf("past due at boot: %+v", got)
	}
	got, err := w.svc.Decide(ctx, Decision{ID: d.ID, Hash: d.Hash, By: "U0OWNER001", Approve: true})
	if err != nil || got.Status != StatusDone || len(w.runs) != 1 {
		t.Fatalf("pending across a restart: %+v %v", got, err)
	}
	if len(w.runs) != 1 {
		t.Fatalf("denied or expired ran: %v", w.runs)
	}
}

func TestCaps(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for i := range DefaultPerConversation {
		if _, err := w.svc.Request(ctx, ask("conv")); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := w.svc.Request(ctx, ask("conv")); !errors.Is(err, ErrCap) {
		t.Fatalf("per conversation: %v", err)
	}
	for i := DefaultPerConversation; i < DefaultPerDay; i++ {
		if _, err := w.svc.Request(ctx, ask("conv-"+string(rune('a'+i)))); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := w.svc.Request(ctx, ask("another")); !errors.Is(err, ErrCap) {
		t.Fatalf("per day: %v", err)
	}
	w.now = w.now.Add(25 * time.Hour)
	if _, err := w.svc.Request(ctx, ask("another")); err != nil {
		t.Fatalf("a day later: %v", err)
	}
}

func TestUnavailableAndUnshowable(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.svc.SetNotifier(nil)
	if _, err := w.svc.Request(ctx, ask("c")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no gateway: %v", err)
	}
	w.svc.SetNotifier(w.n)
	big := ask("c")
	big.Card.Payload = strings.Repeat("x", MaxPayload+1)
	if _, err := w.svc.Request(ctx, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	w.n.fail = true
	if _, err := w.svc.Request(ctx, ask("c")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("card not posted: %v", err)
	}
	// only taint authors as approvers: no one may decide
	w.n.fail = false
	only := ask("c")
	only.Taint = []wire.Taint{{Users: []string{"U0OWNER001", "U0AUTHOR01"}}}
	if _, err := w.svc.Request(ctx, only); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no possible approver: %v", err)
	}
}

func TestHashCoversThePayload(t *testing.T) {
	a := Action{Kind: KindTool, Tool: "write_file", Args: json.RawMessage(`{"path":"a","content":"x"}`)}
	b := a
	b.Args = json.RawMessage(`{"path":"a","content":"y"}`)
	if a.Hash("c") == b.Hash("c") || a.Hash("c") == a.Hash("d") {
		t.Fatal("the hash does not cover args and conversation")
	}
	spaced := a
	spaced.Args = json.RawMessage("{ \"path\": \"a\", \"content\": \"x\" }")
	if a.Hash("c") != spaced.Hash("c") {
		t.Fatal("whitespace changes the hash")
	}
}
