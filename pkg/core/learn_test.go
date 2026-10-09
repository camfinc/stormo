package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/loop"
)

type learnFixture struct {
	l      *Learner
	db     *DB
	inst   *instance.Instance
	mu     sync.Mutex
	roster map[string]*BusAgent
	naps   []string
	sleeps []time.Duration
	napErr error
	dream  map[string]error
	now    time.Time
	// busyFor: an agent stays busy for this many polls.
	busyFor map[string]int
}

func newLearn(t *testing.T) *learnFixture {
	t.Helper()
	_, inst := fixture(t)
	f := &learnFixture{db: testDB(t), inst: inst, now: time.Date(2026, 10, 9, 3, 0, 30, 0, time.UTC), dream: map[string]error{}, busyFor: map[string]int{},
		roster: map[string]*BusAgent{"atlas": {ID: "atlas", Unit: "sales", Running: true}, "nova": {ID: "nova", Unit: "media"}}}
	inst.CoreLearning = instance.CoreLearning{At: "03:00", Timezone: "UTC", StaggerMinutes: 2, QuietWaitMinutes: 30}
	f.l = NewLearner(LearnerOptions{Inst: inst, DB: f.db, DigestDir: t.TempDir(), Now: func() time.Time { return f.now },
		Roster: func() []BusAgent {
			f.mu.Lock()
			defer f.mu.Unlock()
			out := []BusAgent{}
			for _, a := range f.roster {
				out = append(out, *a)
			}
			return out
		},
		Deps: LearnDeps{
			NapNow: func(id string) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.naps = append(f.naps, id)
				return f.napErr
			},
			Dream: func(id string) (*loop.DreamReport, error) {
				if err := f.dream[id]; err != nil {
					return nil, err
				}
				return &loop.DreamReport{Agent: id, Naps: []string{"n1", "n2"}, NewLearnings: 3, Resighted: 1,
					SkillProposals: []loop.SkillProposalReport{{}}, CronProposal: id == "atlas"}, nil
			},
			Pending: func(id string) (int, int) { return 4, 1 },
			Prune: func(id string) (*loop.PruneReport, error) {
				return &loop.PruneReport{Agent: id, Naps: 5, Bytes: 3 << 20}, nil
			},
			Sleep: func(d time.Duration, _ <-chan struct{}) bool {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.sleeps = append(f.sleeps, d)
				for id, n := range f.busyFor {
					if n > 0 {
						f.busyFor[id] = n - 1
						f.roster[id].Busy = n-1 > 0
					}
				}
				return true
			},
		}})
	return f
}

func TestManualCycleNapsDreamsAndDigests(t *testing.T) {
	f := newLearn(t)
	c, err := f.l.RunCycle("manual", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "done" || c.Trigger != "manual" || len(c.Runs) != 2 {
		t.Fatalf("cycle %+v", c)
	}
	if strings.Join(f.naps, ",") != "atlas" {
		t.Errorf("only running agents are napped: %v", f.naps)
	}
	a, n := c.Runs[0], c.Runs[1]
	if a.Agent != "atlas" || !a.Napped || a.Naps != 2 || a.NewLearnings != 3 || a.SkillProposals != 1 || !a.CronProposal || a.PendingLearnings != 4 || a.PendingSkills != 1 {
		t.Errorf("atlas %+v", a)
	}
	if a.PrunedNaps != 5 || a.PrunedBytes != 3<<20 {
		t.Errorf("pruned %+v", a)
	}
	if n.Agent != "nova" || n.Napped || !strings.Contains(n.NapNote, "not running") || n.NewLearnings != 3 {
		t.Errorf("nova is dreamed on its stored naps: %+v", n)
	}
	if len(f.sleeps) != 0 {
		t.Errorf("a manual cycle is not staggered: %v", f.sleeps)
	}
	b, err := os.ReadFile(c.Digest)
	if err != nil {
		t.Fatal(err)
	}
	d := string(b)
	for _, want := range []string{"Learning cycle 1 (manual)", "| atlas | yes | 2 | 3 | 1 | 1 | yes | 4 lessons, 1 skills | 5 (3.0 MB) |", "stormo learn list atlas", "stormo learn skills nova", "nothing was committed"} {
		if !strings.Contains(d, want) {
			t.Errorf("digest lacks %q:\n%s", want, d)
		}
	}
	if info, _ := os.Stat(c.Digest); info.Mode().Perm() != 0o600 {
		t.Errorf("digest mode %v", info.Mode().Perm())
	}
}

func TestBusyAgentsAndFailures(t *testing.T) {
	f := newLearn(t)
	// Busy for two polls, then quiet: waited for, then napped.
	f.roster["atlas"].Busy, f.busyFor["atlas"] = true, 2
	c, _ := f.l.RunCycle("manual", []string{"atlas"}, false)
	if !c.Runs[0].Napped || len(f.sleeps) != 2 || f.sleeps[0] != 30*time.Second {
		t.Errorf("waited %v, run %+v", f.sleeps, c.Runs[0])
	}
	// Busy past the quiet window: dreamed without a nap.
	f.sleeps, f.naps = nil, nil
	f.roster["atlas"].Busy, f.busyFor["atlas"] = true, 1000
	c, _ = f.l.RunCycle("manual", []string{"atlas"}, false)
	if c.Runs[0].Napped || !strings.Contains(c.Runs[0].NapNote, "busy for 30m0s") || len(f.naps) != 0 || len(f.sleeps) != 60 {
		t.Errorf("busy: %+v naps %v sleeps %d", c.Runs[0], f.naps, len(f.sleeps))
	}
	// force naps it anyway.
	f.sleeps = nil
	c, _ = f.l.RunCycle("manual", []string{"atlas"}, true)
	if !c.Runs[0].Napped || len(f.sleeps) != 0 {
		t.Errorf("forced: %+v", c.Runs[0])
	}
	// A failed nap still dreams; a failed dream is recorded.
	f.roster["atlas"].Busy, f.busyFor["atlas"] = false, 0
	f.napErr = errors.New("nap sidecar not running")
	f.dream["atlas"] = errors.New("store unreadable")
	c, _ = f.l.RunCycle("manual", []string{"atlas"}, false)
	if r := c.Runs[0]; r.Napped || r.NapNote != "nap failed: nap sidecar not running" || r.Error != "store unreadable" || r.NewLearnings != 0 {
		t.Errorf("failures %+v", r)
	}
	b, _ := os.ReadFile(c.Digest)
	if !strings.Contains(string(b), "dream failed: store unreadable") {
		t.Errorf("digest:\n%s", b)
	}
	if _, err := f.l.RunCycle("manual", []string{"zed"}, false); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("unknown agent: %v", err)
	}
}

func TestScheduleDueStaggerAndRecovery(t *testing.T) {
	f := newLearn(t)
	at := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now  time.Time
		want bool
	}{
		{at.Add(-time.Minute), false},
		{at, true},
		{at.Add(5 * time.Hour), true},
		{at.Add(catchUp + time.Minute), false},
	} {
		if got := f.l.Due(c.now); got != c.want {
			t.Errorf("due at %s = %v", c.now.Format("15:04"), got)
		}
	}
	if next := f.l.Next(at.Add(-time.Hour)); next != "2026-10-09T03:00:00.000Z" {
		t.Errorf("next before %s", next)
	}
	if next := f.l.Next(at.Add(time.Hour)); next != "2026-10-10T03:00:00.000Z" {
		t.Errorf("next after %s", next)
	}
	f.now = at.Add(time.Minute)
	c, err := f.l.RunCycle("schedule", nil, false)
	if err != nil || c.Status != "done" {
		t.Fatal(c, err)
	}
	if len(f.sleeps) != 1 || f.sleeps[0] != 2*time.Minute {
		t.Errorf("a scheduled cycle staggers agents: %v", f.sleeps)
	}
	if f.l.Due(at.Add(2 * time.Hour)) {
		t.Error("one scheduled cycle a day")
	}
	if !f.l.Due(at.Add(24 * time.Hour)) {
		t.Error("due again tomorrow")
	}
	f.inst.CoreLearning.At = ""
	if f.l.Due(at.Add(24*time.Hour)) || f.l.Next(at) != "" {
		t.Error("no schedule, no cycle")
	}

	// A cycle left running by a core that stopped is interrupted on the next start.
	if _, err := f.db.Exec(`INSERT INTO learn_cycles(trigger, started, status) VALUES('manual', 1, 'running')`); err != nil {
		t.Fatal(err)
	}
	l2 := NewLearner(LearnerOptions{Inst: f.inst, DB: f.db, Deps: f.l.o.Deps})
	cs, _ := l2.Cycles(10)
	if cs[0].Status != "interrupted" {
		t.Errorf("left running: %+v", cs[0])
	}
}

func TestOneCycleAtATime(t *testing.T) {
	f := newLearn(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	f.l.o.Deps.NapNow = func(string) error { close(entered); <-release; return nil }
	id, err := f.l.Start("manual", []string{"atlas"}, false)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := f.l.Start("manual", nil, false); !errors.Is(err, ErrCycleRunning) {
		t.Errorf("second cycle: %v", err)
	}
	v, _ := f.l.View(5)
	if v.Running == nil || *v.Running != id || v.At != "03:00" || v.Timezone != "UTC" {
		t.Errorf("view %+v", v)
	}
	close(release)
	for i := 0; i < 200; i++ {
		if v, _ := f.l.View(5); v.Running == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the cycle never finished")
}

func TestLearnRoutes(t *testing.T) {
	_, inst := fixture(t)
	naps := make(chan string, 4)
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true,
		Roster: func() []BusAgent { return []BusAgent{{ID: "atlas", Unit: "sales", Running: true}} },
		LearnDeps: LearnDeps{
			NapNow:  func(id string) error { naps <- id; return nil },
			Dream:   func(id string) (*loop.DreamReport, error) { return &loop.DreamReport{Agent: id, NewLearnings: 2}, nil },
			Pending: func(string) (int, int) { return 2, 0 },
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	do := func(method, path, auth, body string) (int, string) {
		req, _ := http.NewRequest(method, base+path, bytes.NewReader([]byte(body)))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	for _, who := range []string{"", atlasKey} {
		if code, _ := do("POST", "/api/learn", who, `{}`); code != 401 {
			t.Errorf("learn as %q: %d", who, code)
		}
	}
	if code, body := do("POST", "/api/learn", "owner-token-0123456789", `{"agents":["atlas"]}`); code != 202 || !strings.Contains(body, `"cycle":1`) {
		t.Fatalf("learn %d %s", code, body)
	}
	if got := <-naps; got != "atlas" {
		t.Errorf("napped %s", got)
	}
	if code, body := do("POST", "/api/learn", "owner-token-0123456789", `{"agents":["zed"]}`); code != 400 && code != 409 {
		t.Errorf("bad agent %d %s", code, body)
	}
	for i := 0; i < 200; i++ {
		_, body := do("GET", "/api/learning", "", "")
		if strings.Contains(body, `"status":"done"`) {
			if !strings.Contains(body, `"newLearnings":2`) {
				t.Errorf("learning %s", body)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the cycle never finished")
}
