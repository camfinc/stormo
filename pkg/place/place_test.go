package place

import (
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/aws"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/instance"
)

func g(other SideState, shared, allow bool) bool {
	return GuardStart("atlas", Local, other, shared, allow).OK
}

func TestProductionSlackAppStartsOnlyWhereNotRunning(t *testing.T) {
	cases := []struct {
		other         SideState
		allow, wantOK bool
	}{
		{Running, false, false},
		{Running, true, false}, // the flag never overrides a known running side
		{Stopped, false, true},
		{Missing, false, true},
		{Unknown, false, false},
		{Unknown, true, true},
	}
	for _, c := range cases {
		if got := g(c.other, true, c.allow); got != c.wantOK {
			t.Errorf("other=%s allow=%v: got %v", c.other, c.allow, got)
		}
	}
	if r := GuardStart("atlas", Remote, Running, true, false).Reason; !strings.Contains(r, "stormo handoff atlas --to remote") {
		t.Fatal(r)
	}
}

func TestDevAppRunsAlongside(t *testing.T) {
	for _, o := range []SideState{Running, Stopped, Missing, Unknown} {
		if !g(o, false, false) {
			t.Errorf("%s: dev app blocked", o)
		}
	}
}

func TestRemoteSide(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	tg := deploy.DefaultTarget(inst)
	fake := func(out string, code int) aws.CLI {
		return func(args []string, _ *string) aws.Result {
			if !strings.Contains(strings.Join(args, " "), "--services acme-stormo-atlas") {
				t.Fatalf("args %v", args)
			}
			return aws.Result{Code: code, Stdout: out, Stderr: "boom"}
		}
	}
	for _, c := range []struct {
		out  string
		code int
		want SideState
	}{
		{`{"services":[{"status":"ACTIVE","desiredCount":1,"runningCount":0,"pendingCount":0}]}`, 0, Running},
		{`{"services":[{"status":"ACTIVE","desiredCount":0,"runningCount":0,"pendingCount":0}]}`, 0, Stopped},
		{`{"services":[{"status":"INACTIVE","desiredCount":0}]}`, 0, Missing},
		{`{"services":[],"failures":[{"reason":"MISSING"}]}`, 0, Missing},
		{``, 255, Unknown},
	} {
		if got := RemoteSide(inst, "atlas", fake(c.out, c.code), tg); got != c.want {
			t.Errorf("%s: got %s want %s", c.out, got, c.want)
		}
	}
}

func TestStateFromComposePs(t *testing.T) {
	lines := `{"Service":"agent","State":"running","Health":"starting"}
{"Service":"nap","State":"exited"}`
	s := StateFromComposePs(lines)
	if s.State != "starting" || s.Health != "starting" || s.Detail != "nap sidecar exited" {
		t.Fatalf("%+v", s)
	}
	if s := StateFromComposePs(`[{"Service":"agent","State":"running","Health":"healthy"},{"Service":"nap","State":"running"}]`); s.State != "running" || s.Detail != "" {
		t.Fatalf("%+v", s)
	}
	if s := StateFromComposePs(""); s.State != "stopped" {
		t.Fatalf("%+v", s)
	}
}
