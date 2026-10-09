package review

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
)

// Through the Hermes adapter's reading of cron/jobs.json.
func TestAnalyzeSchedules(t *testing.T) {
	h := hermes.New(&instance.Instance{})
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	jobs := `{"jobs":[
	 {"name":"ok","enabled":true,"last_status":"ok","next_run_at":"2026-10-07T12:05:00Z"},
	 {"name":"broken","enabled":true,"last_status":"error","last_error":"Script exited with code 2","failure_streak":3},
	 {"name":"undelivered","enabled":true,"last_status":"ok","last_delivery_error":"channel_not_found"},
	 {"name":"late","enabled":true,"next_run_at":"2026-10-07T11:00:00Z"},
	 {"name":"paused","enabled":false,"last_status":"error"}]}`
	issues := []string{}
	read, err := h.Schedules([]byte(jobs))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range AnalyzeSchedules(read, now) {
		issues = append(issues, i.Job+": "+i.Problem)
	}
	for _, want := range []string{"broken: last run error: Script exited with code 2", "broken: failure streak 3", "undelivered: delivery error: channel_not_found"} {
		if !slices.Contains(issues, want) {
			t.Errorf("missing %q in %v", want, issues)
		}
	}
	if !slices.ContainsFunc(issues, func(s string) bool { return strings.HasPrefix(s, "late: overdue") }) {
		t.Errorf("late job not overdue: %v", issues)
	}
	if slices.ContainsFunc(issues, func(s string) bool { return strings.HasPrefix(s, "ok") || strings.HasPrefix(s, "paused") }) {
		t.Errorf("false positives: %v", issues)
	}
	if _, err := h.Schedules([]byte("not json")); err == nil {
		t.Error("unreadable not reported")
	}
}

func TestSummarizeLogs(t *testing.T) {
	log := strings.Join([]string{
		"2026-10-07 15:20:01,1 WARNING gateway.platforms.api_server: API server rejected invalid API key: peer_ip='192.168.1.4'",
		"2026-10-07 15:20:31,2 WARNING gateway.platforms.api_server: API server rejected invalid API key: peer_ip='192.168.1.5'",
		"2026-10-07 15:21:00,3 WARNING hermes_cli.tools_config: platform 'teams' has no valid toolsets configured",
		"2026-10-07 15:22:00,4 ERROR cron.scheduler: job 5f3c216ed926 failed",
		"2026-10-07 15:22:00,5 INFO all good",
	}, "\n")
	s := SummarizeLogs(log, hermes.New(&instance.Instance{}).LogNoise()...)
	if s.Errors != 1 || s.Warnings != 3 || s.Top[0].Count != 2 || !strings.Contains(s.Top[0].Pattern, "rejected invalid API key") {
		t.Errorf("summary = %+v", s)
	}
	if !strings.Contains(s.Top[len(s.Top)-1].Known, "unused platform toolsets") {
		t.Errorf("known noise not ranked last: %+v", s.Top)
	}
	if !slices.ContainsFunc(s.Top, func(g LogGroup) bool { return strings.Contains(g.Pattern, "job <hex> failed") }) {
		t.Errorf("hex not normalized: %+v", s.Top)
	}
}

func TestFreshness(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if f := Freshness([]string{"2026-10-07T11:58:00Z", "2026-10-07T11:00:00Z"}, 120, now); *f.AgeMinutes != 2 || f.Stale {
		t.Errorf("%+v", f)
	}
	if !Freshness([]string{"2026-10-07T11:00:00Z"}, 120, now).Stale {
		t.Error("old nap not stale")
	}
	if f := Freshness(nil, 900, now); f.Latest != nil || !f.Stale {
		t.Errorf("%+v", f)
	}
}
