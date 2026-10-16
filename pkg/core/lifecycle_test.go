package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/link"
)

func TestLifecycleByPlacement(t *testing.T) {
	_, inst := fixture(t)
	inst.Placements = []instance.Placement{{Name: "local", Kind: instance.PlacementLocal}, {Name: "remote", Kind: instance.PlacementECS}, {Name: "vps", Kind: instance.PlacementRunner}}
	const owner = "owner-token-0123456789"
	var mu sync.Mutex
	calls := []string{}
	record := func(s string) { mu.Lock(); calls = append(calls, s); mu.Unlock() }
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: owner, NoMirror: true,
		Roster: func() []BusAgent {
			return []BusAgent{{ID: "atlas", Unit: "sales", Running: true}, {ID: "nova", Unit: "media"}}
		},
		Lifecycle: LifecycleOps{
			Local: func(action, id string, log func(string)) error {
				record("local " + action + " " + id)
				log("compose up")
				return nil
			},
			Remote: func(action, id string, confirm func([]string) bool, log func(string)) error {
				if !confirm([]string{"aws ecs update-service --desired-count 1"}) {
					return nil
				}
				record("ecs " + action + " " + id)
				return nil
			},
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	operator, _, _ := c.DB.AddToken("ops", ScopeOperate, 0)
	reader, _, _ := c.DB.AddToken("viewer", ScopeRead, 0)
	do := func(tok, method, path string, body any) (int, map[string]any) {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+tok)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	if code, _ := do(reader, "POST", "/api/agents/atlas/start", nil); code != 403 {
		t.Errorf("read token starting: %d", code)
	}
	if code, out := do(operator, "POST", "/api/agents/atlas/start", map[string]any{"placement": "local"}); code != 200 || out["placement"] != "local" {
		t.Errorf("local start: %d %v", code, out)
	}
	// Restart of an agent not running here is refused, as review's restart always was.
	if code, _ := do(operator, "POST", "/api/agents/nova/restart", nil); code != 409 {
		t.Errorf("restart of a stopped agent: %d", code)
	}
	// ECS: the plan first, then only with confirm.
	code, out := do(operator, "POST", "/api/agents/atlas/start", map[string]any{"placement": "remote"})
	if code != 428 || !strings.Contains(fmt.Sprint(out["plan"]), "update-service") {
		t.Errorf("ecs without confirm: %d %v", code, out)
	}
	if code, _ := do(operator, "POST", "/api/agents/atlas/start", map[string]any{"placement": "remote", "confirm": true}); code != 200 {
		t.Errorf("ecs with confirm: %d", code)
	}
	// A runner placement: unavailable until a runner serves it, then its orders go there.
	if code, _ := do(operator, "POST", "/api/agents/atlas/stop", map[string]any{"placement": "vps"}); code != 503 {
		t.Errorf("vps without a runner: %d", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.RunRunner(ctx, link.RunnerOptions{CoreURL: base, Token: operator, Placement: "vps", Logf: t.Logf,
		Act: func(_ context.Context, action, agent string, log func(string)) (string, error) {
			record("vps " + action + " " + agent)
			log("ran on the vps")
			if action == "restart" {
				return "", errors.New("docker is not running")
			}
			return "running", nil
		}})
	eventually(t, "the runner", func() bool { return c.Runners.Connected("vps") })
	code, out = do(operator, "POST", "/api/agents/atlas/stop", map[string]any{"placement": "vps"})
	if code != 200 || out["state"] != "running" || !strings.Contains(fmt.Sprint(out["log"]), "ran on the vps") {
		t.Errorf("vps stop: %d %v", code, out)
	}
	if code, out := do(operator, "POST", "/api/agents/atlas/restart", map[string]any{"placement": "vps"}); code != 500 || !strings.Contains(fmt.Sprint(out["error"]), "docker is not running") {
		t.Errorf("vps failure: %d %v", code, out)
	}
	// A runner needs an operate token and a runner placement.
	_, out = do(reader, "GET", "/api/placements", nil)
	if !strings.Contains(fmt.Sprint(out["placements"]), "map[available:true kind:runner name:vps]") {
		t.Errorf("placements: %v", out["placements"])
	}

	mu.Lock()
	got := strings.Join(calls, "; ")
	mu.Unlock()
	if got != "local start atlas; ecs start atlas; vps stop atlas; vps restart atlas" {
		t.Errorf("calls: %s", got)
	}
	log, _ := c.DB.AuditLog(20)
	actions := []string{}
	for _, e := range log {
		if strings.HasPrefix(e.Action, "agent.") {
			actions = append(actions, e.Actor+" "+e.Action+" "+e.Detail)
		}
	}
	if len(actions) != 4 || !strings.Contains(actions[0], "ops agent.restart vps: failed") {
		t.Errorf("audit: %v", actions)
	}
}
