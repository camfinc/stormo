package ops

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/aws"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/place"
)

type fake struct {
	deps  *Deps
	calls [][]string
	logs  []string
}

func newFake(t *testing.T, service map[string]int, approve bool, local place.SideState) *fake {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	cli := func(args []string, _ *string) aws.Result {
		f.calls = append(f.calls, args)
		if len(args) > 1 && args[1] == "describe-services" {
			var body any = map[string]any{"services": []any{}, "failures": []any{map[string]string{"reason": "MISSING"}}}
			if service != nil {
				s := map[string]any{"status": "ACTIVE", "pendingCount": 0, "deployments": []any{map[string]string{"status": "PRIMARY", "rolloutState": "COMPLETED"}}}
				for k, v := range service {
					s[k] = v
				}
				body = map[string]any{"services": []any{s}}
			}
			b, _ := json.Marshal(body)
			return aws.Result{Stdout: string(b)}
		}
		return aws.Result{Stdout: "{}"}
	}
	target := deploy.DefaultTarget(inst)
	target.Bucket = "test-bucket-that-does-not-exist"
	f.deps = &Deps{
		Inst: inst, AWS: cli, Target: target,
		Confirm:      func([]string) bool { return approve },
		Log:          func(l string) { f.logs = append(f.logs, l) },
		LocalSide:    func(string) place.SideState { return local },
		SharedTokens: func(*manifest.Agent) (bool, error) { return true, nil }, // full handoff: production Slack app
	}
	return f
}

// updates keeps only the verb-specific arguments of update-service calls.
func (f *fake) updates() [][]string {
	out := [][]string{}
	for _, c := range f.calls {
		if c[1] != "update-service" {
			continue
		}
		i := slices.Index(c, "--service")
		out = append(out, c[i+2:len(c)-2]) // drop "--output json"
	}
	return out
}

func TestPickWhere(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		remote bool
		target string
		env    map[string]string
		want   place.Where
	}{
		{false, "", nil, place.Local}, {true, "", nil, place.Remote}, {false, "remote", nil, place.Remote},
		{false, "", map[string]string{"SWARM_TARGET": "remote"}, place.Remote}, {false, "local", map[string]string{"SWARM_TARGET": "remote"}, place.Local},
	}
	for _, c := range cases {
		if got, err := PickWhere(c.remote, c.target, env(c.env)); err != nil || got != c.want {
			t.Errorf("%+v: %s %v", c, got, err)
		}
	}
	if _, err := PickWhere(false, "prod", env(nil)); err == nil || !strings.Contains(err.Error(), "local or remote") {
		t.Errorf("err = %v", err)
	}
}

func TestRemoteLifecycle(t *testing.T) {
	stopped := newFake(t, map[string]int{"desiredCount": 0, "runningCount": 0}, true, place.Stopped)
	if err := Start(place.Remote, "atlas", stopped.deps); err != nil {
		t.Fatal(err)
	}
	if err := Restart(place.Remote, "atlas", stopped.deps); err != nil {
		t.Fatal(err)
	}
	u := stopped.updates()
	if len(u) != 2 || !slices.Equal(u[0], []string{"--desired-count", "1"}) || !slices.Equal(u[1], []string{"--force-new-deployment", "--desired-count", "1"}) {
		t.Errorf("updates = %v", u)
	}
	found := false
	for _, c := range stopped.calls {
		if c[1] == "update-service" && slices.Contains(c, "acme-stormo-atlas") {
			found = true
		}
	}
	if !found {
		t.Error("service name acme-stormo-atlas not used")
	}

	running := newFake(t, map[string]int{"desiredCount": 1, "runningCount": 1}, true, place.Stopped)
	if err := Start(place.Remote, "atlas", running.deps); err != nil || len(running.updates()) != 0 || !strings.Contains(running.logs[0], "already running") {
		t.Errorf("start on running: %v %v", err, running.logs)
	}
	if err := Stop(place.Remote, "atlas", running.deps); err != nil || !slices.Equal(running.updates()[0], []string{"--desired-count", "0"}) {
		t.Errorf("stop: %v %v", err, running.updates())
	}
}

func TestDeclinedConfirmationChangesNothing(t *testing.T) {
	f := newFake(t, map[string]int{"desiredCount": 1, "runningCount": 1}, false, place.Stopped)
	if err := Stop(place.Remote, "atlas", f.deps); err != nil || len(f.updates()) != 0 || !slices.Equal(f.logs, []string{"atlas: skipped"}) {
		t.Errorf("err=%v updates=%v logs=%v", err, f.updates(), f.logs)
	}
}

func TestECSRefusesWhileRunningLocallyOnTheSameApp(t *testing.T) {
	f := newFake(t, map[string]int{"desiredCount": 0, "runningCount": 0}, true, place.Running)
	if err := Start(place.Remote, "atlas", f.deps); err == nil || !strings.Contains(err.Error(), "handoff atlas --to remote") {
		t.Errorf("start: %v", err)
	}
	if err := Restart(place.Remote, "atlas", f.deps); err == nil || !strings.Contains(err.Error(), "running on local") {
		t.Errorf("restart: %v", err)
	}
	if len(f.updates()) != 0 {
		t.Errorf("updates = %v", f.updates())
	}
}

func TestMissingServiceIsAnErrorNotACreate(t *testing.T) {
	f := newFake(t, nil, true, place.Stopped)
	if err := Start(place.Remote, "atlas", f.deps); err == nil || !strings.Contains(err.Error(), "does not exist yet") || len(f.updates()) != 0 {
		t.Errorf("err = %v", err)
	}
}

func TestRemoteStatus(t *testing.T) {
	f := newFake(t, map[string]int{"desiredCount": 1, "runningCount": 0}, true, place.Stopped)
	rows, err := AgentStatus(place.Remote, f.deps, []string{"atlas"})
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	if r.State != "starting" || r.Detail != "0/1 tasks, rollout COMPLETED" || r.Endpoint != "ecs://acme-ecs/acme-stormo-atlas" {
		t.Errorf("row = %+v", r)
	}
}

func TestRenderStatus(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	out := RenderStatus([]Status{
		{Agent: "atlas", Unit: "sales", Where: "local", State: "running", Health: "healthy", Endpoint: "http://127.0.0.1:18642", LastNap: "2026-10-06T11:55:00Z", PendingLearnings: 2},
		{Agent: "scout", Unit: "sales", Where: "local", State: "stopped", Endpoint: "http://127.0.0.1:18643"},
	}, now)
	want := []string{
		"LOCAL",
		"AGENT  UNIT   STATE    LAST NAP  TO REVIEW            API",
		"atlas  sales  running  5m ago    2 lessons, 0 skills  http://127.0.0.1:18642",
		"scout  sales  stopped  never     -                    http://127.0.0.1:18643",
	}
	if got := strings.Split(out, "\n"); !slices.Equal(got, want) {
		t.Errorf("got\n%s", out)
	}
}
