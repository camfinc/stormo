package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		Container: func(_ context.Context, image string, _ []string, env map[string]string, input []byte, _ string) ([]byte, error) {
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

// serviceDocker plays docker for service capabilities whose container is an httptest server.
func serviceDocker(addr string) func(context.Context, []string, ...string) ([]byte, error) {
	var mu sync.Mutex
	running := false
	return func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "run":
			running = true
		case "port":
			return []byte(addr + "\n"), nil
		case "inspect":
			return []byte(fmt.Sprint(running) + "\n"), nil
		case "stop", "rm":
			running = false
		}
		return nil, nil
	}
}

func TestCapabilityServicesFilesAndNotices(t *testing.T) {
	_, inst := fixture(t)
	wroot := t.TempDir()
	jobs := t.TempDir()
	hold := make(chan struct{})
	// The worker: upper-cases its input files into out/, or waits when asked to be slow.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		var body struct {
			Job   int64 `json:"job"`
			Input struct {
				Slow bool `json:"slow"`
			} `json:"input"`
			Files []string `json:"files"`
			Out   string   `json:"out"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Input.Slow {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		host := func(p string) string { return filepath.Join(jobs, "text-upper", strings.TrimPrefix(p, "/jobs/")) }
		for _, f := range body.Files {
			b, err := os.ReadFile(host(f))
			if err != nil {
				w.WriteHeader(500)
				fmt.Fprint(w, err)
				return
			}
			os.MkdirAll(filepath.Join(host(body.Out), "sub"), 0o755)
			os.WriteFile(filepath.Join(host(body.Out), "sub", filepath.Base(f)), []byte(strings.ToUpper(string(b))), 0o644)
		}
		fmt.Fprintf(w, `{"job":%d,"files":%d}`, body.Job, len(body.Files))
	}))
	defer worker.Close()
	services := link.NewServices("acme")
	services.Docker = serviceDocker(strings.TrimPrefix(worker.URL, "http://"))
	inst.Capabilities = []instance.Capability{{Name: "text.upper", Kind: "service", Image: "upper:1", Placement: "local", Port: 8080, Health: "/health",
		IdleMinutes: 10, BootMinutes: 1, Concurrency: 1, TimeoutMinutes: 1, Files: "both", Docs: "Upper-cases files.",
		InputSchema: map[string]any{"type": "object"}, Examples: []map[string]any{{"slow": false}}}}
	os.MkdirAll(filepath.Join(wroot, "sales", "notes"), 0o755)
	os.WriteFile(filepath.Join(wroot, "sales", "notes", "a.txt"), []byte("hello"), 0o644)
	os.MkdirAll(filepath.Join(wroot, "media"), 0o755)
	os.WriteFile(filepath.Join(wroot, "media", "secret.txt"), []byte("x"), 0o644)
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true,
		WorkdirRoot: wroot,
		Roster: func() []BusAgent {
			return []BusAgent{{ID: "atlas", Unit: "sales"}, {ID: "nova", Unit: "media"}}
		},
		CapabilityGrants: func(string) []string { return []string{"text.upper"} },
		Services:         services, CapabilityImage: func(string) error { return nil }, CapabilityJobs: jobs})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools := map[string]MCPTool{}
	for _, tl := range CapabilityTools(c.Broker) {
		tools[tl.Name] = tl
	}
	call := func(tool string, args any) (any, error) {
		b, _ := json.Marshal(args)
		return tools[tool].Run(ToolCall{Agent: "atlas", Args: b})
	}

	// Exploring never wakes the worker.
	d, err := call("cap_describe", map[string]any{"capability": "text.upper"})
	if err != nil {
		t.Fatal(err)
	}
	if m := d.(map[string]any); m["docs"] != "Upper-cases files." || m["worker"] != "asleep" || !strings.Contains(m["how"].(string), "/shared/sales/capabilities/text-upper/job-<n>") {
		t.Errorf("describe: %v", m)
	}
	if services.State("text.upper").State != "asleep" {
		t.Error("describe woke it")
	}

	// Files only from the agent's own layers.
	if _, err := call("cap_start", map[string]any{"capability": "text.upper", "files": []string{"/shared/media/secret.txt"}}); err == nil || !strings.Contains(err.Error(), "not mounted for you") {
		t.Errorf("another unit's file: %v", err)
	}
	if _, err := call("cap_start", map[string]any{"capability": "text.upper", "out": "/shared/media/out"}); err == nil {
		t.Error("out in another unit's layer")
	}

	// cap_run: boots it, hands the file over, delivers what it wrote, answers in time.
	v, err := call("cap_run", map[string]any{"capability": "text.upper", "files": []string{"/shared/sales/notes/a.txt"}, "wait_seconds": 10})
	if err != nil {
		t.Fatal(err)
	}
	j := v.(*JobView)
	want := fmt.Sprintf("/shared/sales/capabilities/text-upper/job-%d/sub/a.txt", j.ID)
	if j.Status != "done" || len(j.Files) != 1 || j.Files[0] != want || !strings.Contains(string(j.Output), `"files":1`) {
		t.Fatalf("run: %+v", j)
	}
	if b, _ := os.ReadFile(filepath.Join(wroot, strings.TrimPrefix(want, "/shared/"))); string(b) != "HELLO" {
		t.Errorf("delivered: %q", b)
	}
	if info, _ := c.Workdir.Info("atlas", want); info.LastWriter != "atlas" || info.How != "capability:text.upper" {
		t.Errorf("attribution: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(jobs, "text-upper", fmt.Sprint(j.ID))); !os.IsNotExist(err) {
		t.Error("the job folder outlived the job")
	}
	if services.State("text.upper").State != "awake" {
		t.Error("not awake after a job")
	}
	if inbox, _ := c.Bus.Inbox("atlas", true, 10); len(inbox) != 0 {
		t.Errorf("cap_run that ended in time left a notice: %+v", inbox)
	}

	// cap_start: a notice from the core when it is done, its files attached, into a folder it names.
	v, _ = call("cap_start", map[string]any{"capability": "text.upper", "files": []string{"/shared/sales/notes/a.txt"}, "out": "/shared/group/upper"})
	j = v.(*JobView)
	eventually(t, "the notice", func() bool {
		inbox, _ := c.Bus.Inbox("atlas", true, 10)
		return len(inbox) == 1
	})
	inbox, _ := c.Bus.Inbox("atlas", true, 10)
	if m := inbox[0]; m.From != CoreSender || !strings.Contains(m.Subject, fmt.Sprintf("text.upper job %d done", j.ID)) || len(m.Attach) != 1 || m.Attach[0] != "/shared/group/upper/sub/a.txt" {
		t.Errorf("notice: %+v", m)
	}
	if wakeInput([]string{CoreSender}, "x", 1, 1)[:13] != "[CORE NOTICE]" {
		t.Error("core notices wake as notices")
	}

	// A slow one keeps running past cap_run's wait and gets its notice; the next waits in the queue
	// and is cancelled there; the running one is cancelled too.
	v, _ = call("cap_run", map[string]any{"capability": "text.upper", "input": map[string]any{"slow": true}, "wait_seconds": 1})
	slow := v.(*JobView)
	if slow.Status != "running" || !slow.Notify || slow.Note == "" {
		t.Errorf("slow: %+v", slow)
	}
	v, _ = call("cap_start", map[string]any{"capability": "text.upper", "input": map[string]any{"slow": true}})
	next := v.(*JobView)
	if v, _ := call("cap_cancel", map[string]any{"job": next.ID}); v.(*JobView).Status != "failed" || v.(*JobView).Error != "cancelled" {
		t.Errorf("cancel queued: %+v", v)
	}
	if _, err := c.Broker.Sleep("text.upper"); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("sleep while a job runs: %v", err)
	}
	call("cap_cancel", map[string]any{"job": slow.ID})
	eventually(t, "the running job cancelled", func() bool {
		j, _ := c.Broker.Job("atlas", slow.ID)
		return j.Status == "failed" && j.Error == "cancelled"
	})
	if jl, _ := call("cap_jobs", map[string]any{}); len(jl.(map[string]any)["jobs"].([]JobView)) != 4 {
		t.Errorf("jobs: %v", jl)
	}

	// The owner: the worker's state in the view, and sleep through the API.
	vw, _ := c.Broker.View([]string{"atlas"})
	b, _ := json.Marshal(vw)
	if !strings.Contains(string(b), `"worker":"awake"`) {
		t.Errorf("view: %s", b)
	}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/capabilities/text.upper/sleep", c.Addr.Port), nil)
	req.Header.Set("Authorization", "Bearer owner-token-0123456789")
	eventually(t, "sleep once idle", func() bool {
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
	if services.State("text.upper").State != "asleep" {
		t.Error("still awake")
	}
	close(hold)
}

// A job can wait for files its agent uploads itself (a file it was sent, never in the shared
// space): they stream into the job's folder, the last one queues it, and a job whose files never
// come fails and leaves nothing behind.
func TestCapabilityUploads(t *testing.T) {
	_, inst := fixture(t)
	wroot, jobs := t.TempDir(), t.TempDir()
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		var body struct {
			Files []string `json:"files"`
			Out   string   `json:"out"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		host := func(p string) string { return filepath.Join(jobs, "media-cover", strings.TrimPrefix(p, "/jobs/")) }
		names := []string{}
		for _, f := range body.Files {
			b, _ := os.ReadFile(host(f))
			names = append(names, fmt.Sprintf("%s=%s", filepath.Base(f), b))
		}
		os.WriteFile(filepath.Join(host(body.Out), "cover.jpg"), []byte(strings.Join(names, ",")), 0o644)
		fmt.Fprintf(w, `{"saw":%q}`, strings.Join(names, ","))
	}))
	defer worker.Close()
	services := link.NewServices("acme")
	services.Docker = serviceDocker(strings.TrimPrefix(worker.URL, "http://"))
	inst.Capabilities = []instance.Capability{
		{Name: "media.cover", Kind: "service", Image: "m:1", Placement: "local", Port: 8080, Health: "/health", IdleMinutes: 10, BootMinutes: 1, Concurrency: 1, TimeoutMinutes: 1, Files: "both"},
		{Name: "web.read", Kind: "service", Image: "w:1", Placement: "local", Port: 8080, Health: "/health", IdleMinutes: 10, BootMinutes: 1, Concurrency: 1, TimeoutMinutes: 1, Files: "out"},
	}
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true,
		WorkdirRoot: wroot, Roster: func() []BusAgent { return []BusAgent{{ID: "atlas", Unit: "sales"}, {ID: "nova", Unit: "media"}} },
		CapabilityGrants: func(string) []string { return []string{"media.cover", "web.read"} },
		Services:         services, CapabilityImage: func(string) error { return nil }, CapabilityJobs: jobs,
		CapabilityNow: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	upload := func(key string, id int64, name, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s?name=%s", c.Addr.Port, UploadPath(id), url.QueryEscape(name)), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	tools := map[string]MCPTool{}
	for _, tl := range CapabilityTools(c.Broker) {
		tools[tl.Name] = tl
	}
	call := func(tool string, args any) (any, error) {
		b, _ := json.Marshal(args)
		return tools[tool].Run(ToolCall{Agent: "atlas", Args: b})
	}

	v, err := call("cap_start", map[string]any{"capability": "media.cover", "files_pending": 2})
	if err != nil {
		t.Fatal(err)
	}
	j := v.(*JobView)
	if j.Status != "waiting" || j.Pending != 2 || j.Upload != UploadPath(j.ID) {
		t.Fatalf("start: %+v", j)
	}
	for _, refused := range []map[string]any{
		{"capability": "web.read", "files_pending": 1},     // takes no files
		{"capability": "media.cover", "files_pending": 11}, // too many
	} {
		if _, err := call("cap_start", refused); err == nil {
			t.Errorf("%v accepted", refused)
		}
	}
	if _, err := call("cap_run", map[string]any{"capability": "media.cover", "files_pending": 1}); err == nil {
		t.Error("cap_run waits for no uploads")
	}
	// someone else's job, a path for a name, no key
	if code, _ := upload(novaKey, j.ID, "a.mp4", "x"); code != 409 {
		t.Errorf("nova uploaded to atlas's job: %d", code)
	}
	if code, _ := upload(atlasKey, j.ID, "../escape.mp4", "x"); code != 409 {
		t.Errorf("a path as a name: %d", code)
	}
	if code, _ := upload("", j.ID, "a.mp4", "x"); code != 401 {
		t.Errorf("no key: %d", code)
	}
	if code, body := upload(atlasKey, j.ID, "clip.mp4", "video-bytes"); code != 200 || !strings.Contains(body, `"status":"waiting"`) {
		t.Fatalf("first upload: %d %s", code, body)
	}
	if code, _ := upload(atlasKey, j.ID, "clip.mp4", "again"); code != 409 {
		t.Errorf("a name twice: %d", code)
	}
	if code, body := upload(atlasKey, j.ID, "voice.m4a", "audio"); code != 200 || !strings.Contains(body, `"status":"queued"`) {
		t.Fatalf("last upload: %d %s", code, body)
	}
	if code, _ := upload(atlasKey, j.ID, "late.mp4", "x"); code != 409 {
		t.Errorf("an upload to a queued job: %d", code)
	}
	eventually(t, "the job", func() bool { j, _ := c.Broker.Job("atlas", j.ID); return j.Status == "done" })
	done, _ := c.Broker.Job("atlas", j.ID)
	if !strings.Contains(string(done.Output), "clip.mp4=video-bytes,voice.m4a=audio") || len(done.Files) != 1 || len(done.Uploads) != 2 {
		t.Errorf("done: %+v %s", done, done.Output)
	}
	if _, err := os.Stat(filepath.Join(jobs, "media-cover", fmt.Sprint(j.ID))); !os.IsNotExist(err) {
		t.Error("the uploads outlived the job")
	}
	// the uploads never touched the shared space: only the cover the job wrote is there
	filepath.WalkDir(wroot, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".mp4") || strings.HasSuffix(p, ".m4a")) {
			t.Errorf("an upload in the shared space: %s", p)
		}
		return nil
	})

	// files that never come: the job fails after its wait and its folder goes
	v, _ = call("cap_start", map[string]any{"capability": "media.cover", "files_pending": 1})
	late := v.(*JobView)
	if code, _ := upload(atlasKey, late.ID, "half.mp4", "x"); code != 200 {
		t.Fatal("upload")
	}
	v, _ = call("cap_start", map[string]any{"capability": "media.cover", "files_pending": 2})
	late = v.(*JobView)
	upload(atlasKey, late.ID, "one.mp4", "x")
	clock.Add(int64(pendingWait + time.Minute))
	c.Broker.poke()
	eventually(t, "the late job failed", func() bool { j, _ := c.Broker.Job("atlas", late.ID); return j.Status == "failed" })
	if j, _ := c.Broker.Job("atlas", late.ID); !strings.Contains(j.Error, "did not arrive") {
		t.Errorf("late: %+v", j)
	}
	if _, err := os.Stat(filepath.Join(jobs, "media-cover", fmt.Sprint(late.ID))); !os.IsNotExist(err) {
		t.Error("a late job's uploads stayed")
	}
}
