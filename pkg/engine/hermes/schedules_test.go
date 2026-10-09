package hermes

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

func TestMergeSchedules(t *testing.T) {
	got, err := New(&instance.Instance{}).MergeSchedules([]byte(`{"jobs":[{"id":"a","prompt":"repo"}]}`), []byte(`{"jobs":[{"id":"a","prompt":"stale"},{"id":"b","prompt":"agent-made <x>"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"jobs\": [\n    {\n      \"id\": \"a\",\n      \"prompt\": \"repo\"\n    },\n    {\n      \"id\": \"b\",\n      \"prompt\": \"agent-made <x>\"\n    }\n  ]\n}"
	if string(got) != want {
		t.Errorf("got %s", got)
	}
}

// A committed Hermes jobs file read as schedules and rendered back keeps every definition key.
func TestSchedulesRoundTrip(t *testing.T) {
	h := New(&instance.Instance{})
	committed := []byte(`{"jobs":[
	 {"id":"a1","name":"Digest","prompt":"Post the digest <b>","skills":["ops/digest"],"skill":"ops/digest","model":null,"provider":null,
	  "base_url":null,"script":"digest.py","no_agent":true,"monitor_script":null,"monitor_url":null,"monitor_state":null,"context_from":null,
	  "schedule":{"kind":"interval","minutes":120,"display":"every 120m"},"schedule_display":"every 120m","repeat":{"times":null,"completed":7},
	  "enabled":true,"state":"scheduled","paused_at":null,"paused_reason":null,"created_at":"2026-05-01T00:00:00Z",
	  "next_run_at":"2026-10-09T12:00:00Z","last_run_at":"2026-10-09T10:00:00Z","last_status":"ok","last_error":null,
	  "last_delivery_error":null,"deliver":"slack","origin":null,"enabled_toolsets":["web","terminal"],"workdir":null,"failure_deliver":"slack"},
	 {"id":"b2","name":"Weekly","prompt":"Plan the week","skills":[],"skill":null,"model":"google/x","provider":"openrouter",
	  "base_url":null,"script":null,"no_agent":false,"monitor_script":"watch.py","monitor_url":null,"monitor_state":null,"context_from":null,
	  "schedule":{"kind":"cron","expr":"0 9 * * 1","display":"0 9 * * 1"},"schedule_display":"0 9 * * 1","repeat":{"times":null,"completed":0},
	  "enabled":false,"state":"paused","paused_at":null,"paused_reason":"review first","created_at":null,
	  "next_run_at":null,"last_run_at":null,"last_status":null,"last_error":null,
	  "last_delivery_error":null,"deliver":"slack","origin":null,"enabled_toolsets":["web"],"workdir":null,"failure_deliver":null}]}`)
	schedules, err := h.ReadSchedules(committed)
	if err != nil {
		t.Fatal(err)
	}
	if s := schedules[0]; s.Every != "120m" || s.Agent || !s.Enabled || s.Script != "digest.py" || s.OnFailure != "slack" {
		t.Errorf("first = %+v", s)
	}
	if s := schedules[1]; s.Cron != "0 9 * * 1" || !s.Agent || s.Enabled || s.Note != "review first" || s.Monitor != "watch.py" || s.Model != "google/x" {
		t.Errorf("second = %+v", s)
	}
	rendered, err := renderJobs(schedules)
	if err != nil {
		t.Fatal(err)
	}
	var was, now struct{ Jobs []map[string]any }
	_ = json.Unmarshal(committed, &was)
	if err := json.Unmarshal(rendered, &now); err != nil {
		t.Fatalf("%v\n%s", err, rendered)
	}
	for i := range was.Jobs {
		for k := range definitionKeys {
			if !reflect.DeepEqual(was.Jobs[i][k], now.Jobs[i][k]) {
				t.Errorf("job %d %s: %v became %v", i, k, was.Jobs[i][k], now.Jobs[i][k])
			}
		}
		for _, k := range []string{"created_at", "next_run_at", "last_run_at", "last_status"} {
			if now.Jobs[i][k] != nil {
				t.Errorf("job %d %s rendered as %v; run state is the scheduler's", i, k, now.Jobs[i][k])
			}
		}
	}
	if again, _ := renderJobs(schedules); string(again) != string(rendered) {
		t.Error("rendering is not deterministic")
	}
	// Merge: the repo's definition, the live run state.
	merged, err := h.MergeSchedules(rendered, committed)
	if err != nil {
		t.Fatal(err)
	}
	var m struct{ Jobs []map[string]any }
	_ = json.Unmarshal(merged, &m)
	if m.Jobs[0]["last_run_at"] != "2026-10-09T10:00:00Z" || m.Jobs[0]["repeat"].(map[string]any)["completed"] != float64(7) || m.Jobs[0]["prompt"] != "Post the digest <b>" {
		t.Errorf("merged = %v", m.Jobs[0])
	}
}
