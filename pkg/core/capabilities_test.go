package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/link"
)

func TestCapabilitiesBroker(t *testing.T) {
	_, inst := fixture(t)
	release := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.WriteHeader(401)
			return
		}
		var body struct {
			Job   int64           `json:"job"`
			Agent string          `json:"agent"`
			Input json.RawMessage `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		<-release
		fmt.Fprintf(w, `{"rendered":%s,"by":%q}`, body.Input, body.Agent)
	}))
	defer endpoint.Close()
	if err := os.WriteFile(filepath.Join(inst.Root, "secrets.local.yaml"), []byte("shared:\n  MEDIA_TOKEN: tok-123\n  ECHO_KEY: echo-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inst.Placements = []instance.Placement{{Name: "local", Kind: instance.PlacementLocal}, {Name: "remote", Kind: instance.PlacementECS}, {Name: "gpu", Kind: instance.PlacementRunner}}
	inst.Capabilities = []instance.Capability{
		{Name: "media.bake", Kind: "http", URL: endpoint.URL, Headers: map[string]string{"Authorization": "Bearer ${MEDIA_TOKEN}"}, Concurrency: 1, TimeoutMinutes: 1, CostUSD: 0.4},
		{Name: "text.echo", Kind: "container", Image: "echo:1", Secrets: []string{"ECHO_KEY"}, Placement: "local", Concurrency: 2, TimeoutMinutes: 1},
		{Name: "gpu.render", Kind: "container", Image: "render:1", Placement: "gpu", Concurrency: 1, TimeoutMinutes: 1, CostUSD: 2},
	}
	var containerEnv map[string]string
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true,
		Roster: func() []BusAgent { return []BusAgent{{ID: "atlas", Unit: "sales"}, {ID: "nova", Unit: "media"}} },
		CapabilityGrants: func(a string) []string {
			if a == "atlas" {
				return []string{"media.bake", "text.echo", "gpu.render"}
			}
			return nil
		},
		Container: func(_ context.Context, image string, _ []string, env map[string]string, input []byte) ([]byte, error) {
			containerEnv = env
			return []byte(`{"echo":` + string(input) + `}`), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools := map[string]MCPTool{}
	for _, tl := range CapabilityTools(c.Broker) {
		tools[tl.Name] = tl
	}
	call := func(agent, tool string, args any) (any, error) {
		b, _ := json.Marshal(args)
		return tools[tool].Run(ToolCall{Agent: agent, Args: b})
	}
	status := func(id int64) *JobView {
		t.Helper()
		v, err := call("atlas", "cap_status", map[string]any{"job": id})
		if err != nil {
			t.Fatal(err)
		}
		return v.(*JobView)
	}

	// What an agent sees: its grants only.
	list, _ := call("atlas", "cap_list", map[string]any{})
	if !strings.Contains(fmt.Sprint(list), "media.bake") {
		t.Errorf("atlas list: %v", list)
	}
	if list, _ := call("nova", "cap_list", map[string]any{}); strings.Contains(fmt.Sprint(list), "media") {
		t.Errorf("nova list: %v", list)
	}
	if _, err := call("nova", "cap_start", map[string]any{"capability": "media.bake", "input": map[string]any{}}); err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Errorf("nova without a grant: %v", err)
	}

	// HTTP, one at a time: the second waits for the first.
	j1, err := call("atlas", "cap_start", map[string]any{"capability": "media.bake", "input": map[string]any{"prompt": "a lighthouse", "email": "ana@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	j2, _ := call("atlas", "cap_start", map[string]any{"capability": "media.bake", "input": map[string]any{"prompt": "a harbour"}})
	id1, id2 := j1.(*JobView).ID, j2.(*JobView).ID
	eventually(t, "the first job running", func() bool { return status(id1).Status == "running" })
	if s := status(id2).Status; s != "queued" {
		t.Errorf("second job while the first runs: %s", s)
	}
	close(release)
	eventually(t, "both done", func() bool { return status(id1).Status == "done" && status(id2).Status == "done" })
	if out := string(status(id1).Output); !strings.Contains(out, "a lighthouse") || !strings.Contains(out, `"by":"atlas"`) {
		t.Errorf("output: %s", out)
	}
	if _, err := call("nova", "cap_status", map[string]any{"job": id1}); err == nil {
		t.Error("nova reads atlas's job")
	}

	// A local container gets its secrets through the environment.
	j3, _ := call("atlas", "cap_start", map[string]any{"capability": "text.echo", "input": map[string]any{"x": 1}})
	eventually(t, "the container job", func() bool { return status(j3.(*JobView).ID).Status == "done" })
	if containerEnv["ECHO_KEY"] != "echo-secret" {
		t.Errorf("container env: %v", containerEnv)
	}

	// A runner's container: failed while no runner serves it, then run there.
	j4, _ := call("atlas", "cap_start", map[string]any{"capability": "gpu.render", "input": map[string]any{"scene": 1}})
	eventually(t, "no runner", func() bool { return status(j4.(*JobView).ID).Status == "failed" })
	if e := status(j4.(*JobView).ID).Error; !strings.Contains(e, "no runner serves gpu") {
		t.Errorf("error: %s", e)
	}
	operator, _, _ := c.DB.AddToken("gpu-box", ScopeOperate, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go link.RunRunner(ctx, link.RunnerOptions{CoreURL: fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port), Token: operator, Placement: "gpu", Logf: t.Logf,
		Act: func(_ context.Context, o link.Order, _ func(string)) (link.OrderResult, error) {
			return link.OrderResult{Output: json.RawMessage(`{"frames":24,"image":"` + o.Job.Image + `"}`)}, nil
		}})
	eventually(t, "the gpu runner", func() bool { return c.Runners.Connected("gpu") })
	j5, _ := call("atlas", "cap_start", map[string]any{"capability": "gpu.render", "input": map[string]any{"scene": 2}})
	eventually(t, "the runner's job", func() bool { return status(j5.(*JobView).ID).Status == "done" })
	if out := string(status(j5.(*JobView).ID).Output); !strings.Contains(out, `"image":"render:1"`) {
		t.Errorf("runner output: %s", out)
	}

	// The owner's view: grants, spend this month (failed jobs cost nothing), previews scrubbed.
	v, err := c.Broker.View([]string{"atlas", "nova"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"agent":"atlas","capability":"media.bake","jobs":2,"failed":0,"usd":0.8`) ||
		!strings.Contains(string(b), `"agent":"atlas","capability":"gpu.render","jobs":2,"failed":1,"usd":2`) || strings.Contains(string(b), "tok-123") {
		t.Errorf("view: %s", b)
	}
	jobs, _ := c.Broker.Jobs(10)
	jb, _ := json.Marshal(jobs)
	if strings.Contains(string(jb), "ana@example.com") || strings.Contains(string(jb), `"output"`) {
		t.Errorf("owner jobs leak input or output: %s", jb)
	}
}
