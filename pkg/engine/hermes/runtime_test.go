package hermes

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestActivityFrom(t *testing.T) {
	now := time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)
	health := decode(t, `{"active_agents":1,"gateway_busy":true,"platforms":{"slack":{"state":"connected","needs_attention":false},"telegram":{"state":"fatal error!","needs_attention":true},"Bad Name":{}}}`)
	sessions := decode(t, `{"data":[{"source":"cron","last_active":1791337000,"title":"SECRET-TITLE","preview":"SECRET-PREVIEW client Juan"},{"source":"slack","last_active":1791337630.5,"title":"SECRET-TITLE","preview":"SECRET-PREVIEW"}]}`)
	a := ActivityFrom(health, sessions, now, nil)
	want := &engine.Activity{ActiveAgents: 1, GatewayBusy: true, Source: "slack", RunningJobs: []string{}, LastActive: isoMillis(time.UnixMilli(1791337630500)),
		Platforms: map[string]engine.Platform{"slack": {State: "connected"}, "telegram": {State: "unknown", NeedsAttention: true}}, PolledAt: isoMillis(now)}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("a = %+v", a)
	}
	if b, _ := json.Marshal(a); strings.Contains(string(b), "SECRET") {
		t.Error("leaked")
	}
	if ActivityFrom(nil, sessions, now, nil) != nil {
		t.Error("nil health")
	}
	if a := ActivityFrom(decode(t, `{"active_agents":0}`), decode(t, `{"data":[]}`), now, nil); a.ActiveAgents != 0 || a.GatewayBusy || a.Source != "" {
		t.Errorf("empty = %+v", a)
	}
}

func TestRunningJobs(t *testing.T) {
	// Observed on Hermes 0.21.5: the SLA watchdog (every 2 min) ran 02:50:34-02:52:47.
	job := func(next string, last any) any {
		l, _ := json.Marshal(last)
		return decode(t, fmt.Sprintf(`{"jobs":[{"id":"5f3c216ed926","name":"SLA watchdog <b>","enabled":true,"schedule":{"kind":"interval","minutes":2},"next_run_at":%q,"last_run_at":%s}]}`, next, l))
	}
	at := func(hms string) float64 {
		tm, _ := time.Parse(time.RFC3339, "2026-10-07T"+hms+"Z")
		return float64(tm.UnixMilli())
	}
	memo := map[string]*JobMemo{}
	check := func(got []string, want ...string) {
		t.Helper()
		if want == nil {
			want = []string{}
		}
		if !slices.Equal(got, want) {
			t.Errorf("got %v want %v", got, want)
		}
	}
	// first run after a restart: no last_run_at, so only a move of next_run_at tells
	check(RunningJobs(job("2026-10-07T02:49:34+00:00", nil), memo, at("02:49:00")))
	check(RunningJobs(job("2026-10-07T02:52:34+00:00", nil), memo, at("02:50:41")), "SLA watchdog b")
	check(RunningJobs(job("2026-10-07T02:54:47+00:00", "2026-10-07T02:52:47+00:00"), memo, at("02:52:48")))
	// a core started mid-run: the interval rule alone sees it
	check(RunningJobs(job("2026-10-07T02:56:47+00:00", "2026-10-07T02:52:47+00:00"), map[string]*JobMemo{}, at("02:55:10")), "SLA watchdog b")
	check(RunningJobs(job("2026-10-07T02:56:47+00:00", "2026-10-07T02:52:47+00:00"), map[string]*JobMemo{}, at("02:54:00")))
	if got := LastJobRun(decode(t, `{"jobs":[{"last_run_at":"2026-10-07T02:52:35+00:00"},{"last_run_at":"2026-10-07T02:52:47+00:00"},{"last_run_at":null}]}`)); got != "2026-10-07T02:52:47.000Z" {
		t.Errorf("last = %s", got)
	}
	if LastJobRun(nil) != "" {
		t.Error("last of nil")
	}
	// disabled jobs and junk are ignored
	check(RunningJobs(decode(t, `{"jobs":[{"id":"x","enabled":false,"name":"off"},null,3]}`), map[string]*JobMemo{}, 0))
	check(RunningJobs(nil, map[string]*JobMemo{}, 0))
}
