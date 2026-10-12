package needletail

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

// render → Read → Merge: the repo's definitions win, the live run state and the agent's own jobs
// are kept.
func TestSchedules(t *testing.T) {
	inst, a := atlas(t)
	n := New(&instance.Instance{Names: inst.Names})
	rendered, err := renderJobs(a.Schedules)
	if err != nil {
		t.Fatal(err)
	}
	read, err := n.ReadSchedules(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 1 || read[0].ID != a.Schedules[0].ID || read[0].Cron != "0 9 * * 1-5" || read[0].Enabled || read[0].Deliver != "slack" {
		t.Errorf("read = %+v", read)
	}
	if again, _ := renderJobs(read); string(again) != string(rendered) {
		t.Errorf("render(read(render)) differs:\n%s\n%s", again, rendered)
	}
	live, err := os.ReadFile("testdata/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := n.MergeSchedules(rendered, live)
	if err != nil {
		t.Fatal(err)
	}
	var m struct{ Jobs []map[string]any }
	if err := json.Unmarshal(merged, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Jobs) != 2 {
		t.Fatalf("jobs = %d", len(m.Jobs))
	}
	repo, agent := m.Jobs[0], m.Jobs[1]
	if repo["prompt"] != a.Schedules[0].Prompt || repo["enabled"] != false || repo["schedule"].(map[string]any)["expr"] != "0 9 * * 1-5" {
		t.Errorf("repo definition lost: %v", repo)
	}
	if repo["last_run_at"] != "2026-10-12T09:00:00Z" || repo["failure_streak"] != float64(2) || repo["running"] != true ||
		repo["repeat"].(map[string]any)["completed"] != float64(4) {
		t.Errorf("live state lost: %v", repo)
	}
	if agent["id"] != "agentmade01" || agent["prompt"] != "Ping <me>" {
		t.Errorf("agent job = %v", agent)
	}
	status, err := n.Schedules(merged)
	if err != nil {
		t.Fatal(err)
	}
	if !status[0].Failed || status[0].FailureStreak != 2 || status[0].LastError != "exit 1" || status[1].Failed || status[1].NextRunAt.IsZero() {
		t.Errorf("status = %+v", status)
	}
}
