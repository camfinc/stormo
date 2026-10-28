package slack

import (
	"strings"
	"testing"
	"time"
)

// Item 11 (Claude L6): a click that cannot be taken now (approvals not wired yet, or the runtime
// shutting down) is answered: the clicker is told to try again, never left with a silent button.
func TestClickAnsweredWhenNotTaken(t *testing.T) {
	r := newRig(t, nil)
	r.fake.Click("c1", alice, dm, "1700000000.000100", actionApprove, "ap_1|"+strings.Repeat("a", 64))
	waitEphemeral := func(n int) {
		t.Helper()
		end := time.Now().Add(5 * time.Second)
		for time.Now().Before(end) {
			if len(r.fake.Ephemerals()) >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%d ephemeral answers, want %d", len(r.fake.Ephemerals()), n)
	}
	waitEphemeral(1)
	if e := r.fake.Ephemerals()[0]; e.User != alice || !strings.Contains(e.Text, "try again") {
		t.Errorf("before approvals are wired: %+v", e)
	}
	r.g.mu.Lock()
	r.g.closing = true
	r.g.mu.Unlock()
	r.fake.Click("c2", alice, dm, "1700000000.000100", actionApprove, "ap_1|"+strings.Repeat("a", 64))
	waitEphemeral(2)
	if e := r.fake.Ephemerals()[1]; !strings.Contains(e.Text, "try again") {
		t.Errorf("while shutting down: %+v", e)
	}
	r.g.mu.Lock()
	r.g.closing = false
	r.g.mu.Unlock()
}
