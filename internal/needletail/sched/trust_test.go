package sched

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/tools"
)

// A job agent.yaml defines delivers to its targets as the repo's (trusted), so the agent cannot
// change what it runs: its prompt, scripts, monitor, no_agent, skills, toolsets and name (by which
// a handler names it) are refused on update, pointing at a job of its own; its timing, targets
// (policy-checked) and pause, resume and run stay the agent's.
func TestRepoJobContentIsTheRepos(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, jobJSON("repo1", `{"kind":"interval","minutes":60}`, `null`),
		strings.Replace(jobJSON("mine", `{"kind":"interval","minutes":60}`, `null`), `"deliver":"local"`, `"deliver":"local","created_at":"2026-10-01T00:00:00Z"`, 1))
	writeScript(t, r.home, "probe.sh", "echo hi\n")
	reg := tools.NewRegistry(r.s.Tool(nil))
	call := func(args map[string]any) (string, bool) {
		b, _ := json.Marshal(args)
		return reg.Call(context.Background(), &tools.Env{}, CronJobTool, string(b))
	}
	for _, change := range []map[string]any{
		{"prompt": "Say hello to everyone."}, {"script": "probe.sh"}, {"monitor": "probe.sh"}, {"no_agent": true, "script": "probe.sh"},
		{"skills": []string{}}, {"enabled_toolsets": []string{"terminal"}}, {"name": "renamed"},
	} {
		args := map[string]any{"action": "update", "job_id": "repo1"}
		for k, v := range change {
			args[k] = v
		}
		if out, failed := call(args); !failed || !strings.Contains(out, "agent.yaml") {
			t.Errorf("repo job update %v: %v %s", change, failed, out)
		}
		args["job_id"] = "mine"
		if change["no_agent"] == nil {
			if out, failed := call(args); failed {
				t.Errorf("own job update %v: %s", change, out)
			}
		}
	}
	if j := r.job("repo1"); j.Str("prompt") != "Report." || j.Str("name") != "repo1" || j.Str("script") != "" {
		t.Errorf("the repo job changed: %s", j.raw["prompt"])
	}
	for _, args := range []map[string]any{
		{"action": "update", "job_id": "repo1", "schedule": "every 2h", "repeat": 3, "deliver": "slack"},
		{"action": "pause", "job_id": "repo1"}, {"action": "resume", "job_id": "repo1"}, {"action": "run", "job_id": "repo1"},
	} {
		if out, failed := call(args); failed {
			t.Errorf("%v refused: %s", args, out)
		}
	}
}

// An explicit deliver of "origin" (or "" on update, which resets to it) is checked as the chat the
// job comes from, not as the home channel: an agent with no home channel can still deliver back
// to the thread it made the job in.
func TestDeliverOriginIsTheJobsOrigin(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0)
	r.s.SetTargetCheck(func(tg Target) error {
		if tg.Channel == "" {
			return errors.New("not a target you may message: no home channel is set")
		}
		return nil
	})
	from := json.RawMessage(`{"platform":"slack","chat_id":"C0DEVCHAN1","thread_id":"1700000002.000100"}`)
	reg := tools.NewRegistry(r.s.Tool(func(string) json.RawMessage { return from }))
	out, failed := reg.Call(context.Background(), &tools.Env{Session: "slack-C0DEVCHAN1-1700000002.000100"}, CronJobTool,
		`{"action":"create","schedule":"1h","prompt":"Report.","deliver":"origin","failure_deliver":"origin"}`)
	if failed {
		t.Fatalf("deliver origin refused: %s", out)
	}
	var made struct {
		Job jobView `json:"job"`
	}
	_ = json.Unmarshal([]byte(out), &made)
	if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"update","job_id":"`+made.Job.JobID+`","deliver":""}`); failed {
		t.Errorf("deliver reset refused: %s", out)
	}
	if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"update","job_id":"`+made.Job.JobID+`","deliver":"slack"}`); !failed {
		t.Errorf("the home channel accepted with none set: %s", out)
	}
}

// A failing script-only job's or monitor's notice carries at most capPrompt's share of the
// output: one Slack post, not dozens.
func TestFailureNoticesAreCapped(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	script := strings.Replace(jobJSON("loud", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`), `"prompt":"Report."`, `"no_agent":true,"script":"loud.sh"`, 1)
	r := bareRig(t, t0, strings.Replace(script, `"deliver":"local"`, `"deliver":"slack"`, 1),
		strings.Replace(gatedJob("mon", "monitor_script", "loud.sh"), `"deliver":"local"`, `"deliver":"slack"`, 1))
	writeScript(t, r.home, "loud.sh", "head -c 200000 /dev/zero | tr '\\0' 'a'; exit 1\n")
	r.tick()
	got := r.sink.taken()
	if len(got) != 2 {
		t.Fatalf("%d deliveries", len(got))
	}
	for _, d := range got {
		if !d.Failure || len(d.Text) > maxScriptPrompt+1024 || !strings.Contains(d.Text, "truncated") {
			t.Errorf("%s: failure %v, %d bytes", d.JobID, d.Failure, len(d.Text))
		}
	}
}
