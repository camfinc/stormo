package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	adapter "github.com/camfinc/stormo/pkg/engine/needletail"
	"github.com/camfinc/stormo/pkg/instance"
)

const (
	coreKey  = "core-key-0123456789abcdef"
	cloudKey = "sk-cloud-0123456789abcdef"
	apiKey   = "api-key-0123456789abcdef"
	crmToken = "acme_abcdefgh1234"
)

// openAIStub is an OpenAI-compatible endpoint: it runs one terminal call, then answers with the
// tool's output. It records the keys it saw.
type openAIStub struct {
	mu   sync.Mutex
	keys []string
}

func (s *openAIStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.keys = append(s.keys, r.Header.Get("Authorization"))
	s.mu.Unlock()
	var req struct {
		Messages []struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	last := req.Messages[len(req.Messages)-1]
	w.Header().Set("Content-Type", "text/event-stream")
	if last.Role == "user" {
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Checking. ","tool_calls":[{"index":0,"id":"call_q1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"echo token=$CRM_API_TOKEN key=${OPENROUTER_API_KEY:-unset} core=${SWARM_CORE_KEY:+set}\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
	} else {
		out, _ := json.Marshal("Saw: " + *last.Content)
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]}`+"\n\n", out)
	}
	fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":17,"total_tokens":117}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// home is a copy of the adapter's golden home with needletail.yaml pointed at core (primary) and
// cloud (fallback), as Compile renders it for an agent on the core.
func home(t *testing.T, core, cloud string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "..", "pkg", "engine", "needletail", "testdata", "home"))); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`version: 1
agent: atlas
model:
    primary:
        base_url: %s
        key_env: SWARM_CORE_KEY
        model: gpt-6-luna
    fallbacks:
        - base_url: %s
          key_env: OPENROUTER_API_KEY
          model: openai/gpt-6-luna
limits: {turns: 4}
core:
    mcp_url: http://127.0.0.1:1/mcp
    ingest_url: http://127.0.0.1:1/ingest
    key_env: SWARM_CORE_KEY
memory:
    memory_char_limit: 2200
    user_char_limit: 1375
luau:
    grants: [fs.read:home, fs.write:home, core:msg_*]
`, core, cloud)
	if err := os.WriteFile(filepath.Join(dir, "needletail.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func setenv(t *testing.T, home string) {
	t.Setenv("AGENT_HOME", home)
	t.Setenv("HOME", os.Getenv("HOME"))
	t.Setenv("SWARM_CORE_KEY", coreKey)
	t.Setenv("OPENROUTER_API_KEY", cloudKey)
	t.Setenv("API_SERVER_KEY", apiKey)
	t.Setenv("CRM_API_TOKEN", crmToken)
	t.Setenv("SWARM_UNIT", "sales")
	t.Setenv("SWARM_SHARED_DIR", filepath.Join(home, "shared-not-mounted"))
}

// oneshot falls back from a dead core to the cloud endpoint, runs the terminal, and prints the
// stream-json the adapter's bench parser reads.
func TestOneshotBench(t *testing.T) {
	cloud := &openAIStub{}
	srv := httptest.NewServer(cloud)
	defer srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	h := home(t, dead.URL+"/v1", srv.URL+"/v1")
	setenv(t, h)

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"needletail", "oneshot", "--format", "stream-json", "-q", "Is the CRM token there?"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	run := adapter.New(&instance.Instance{}).ParseBench(stdout.String(), code)
	if !slices.Equal(run.Tools, []string{"terminal"}) || run.Tokens == nil || *run.Tokens != 234 || run.Error != "" || run.ExitCode != 0 {
		t.Errorf("bench run = %+v\n%s", run, stdout.String())
	}
	// the token reached the script (skills read it from the env) but not the output; the cloud key
	// was taken out of the terminal's env; the core key was passed through
	if !strings.Contains(run.Text, "token=[REDACTED]") || !strings.Contains(run.Text, "key=unset") || !strings.Contains(run.Text, "core=set") {
		t.Errorf("text = %q", run.Text)
	}
	out := stdout.String() + stderr.String()
	for _, secret := range []string{crmToken, cloudKey, coreKey, apiKey} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %s leaked", secret)
		}
	}
	if len(cloud.keys) != 2 || cloud.keys[0] != "Bearer "+cloudKey {
		t.Errorf("cloud saw %v", cloud.keys)
	}
	// the session stays in the home, redacted
	files, _ := filepath.Glob(filepath.Join(h, "sessions", "oneshot-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("sessions = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), crmToken) || !strings.Contains(string(raw), `"end_reason":"oneshot"`) {
		t.Errorf("session file = %s", raw)
	}

	// a failure is still one result line, with exit code 1
	stdout.Reset()
	dead2 := httptest.NewServer(http.NotFoundHandler())
	dead2.Close()
	h2 := home(t, dead.URL+"/v1", dead2.URL+"/v1")
	setenv(t, h2)
	code = Run(context.Background(), []string{"oneshot", "--format", "stream-json", "-q", "hi"}, &stdout, &stderr)
	run = adapter.New(&instance.Instance{}).ParseBench(stdout.String(), code)
	if code != 1 || run.ExitCode != 1 || run.Error == "" {
		t.Errorf("failed run = %d %+v", code, run)
	}
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// serve answers the healthcheck, and stormo's chat client (the adapter) holds a conversation.
func TestServe(t *testing.T) {
	core := &openAIStub{}
	srv := httptest.NewServer(core)
	defer srv.Close()
	h := home(t, srv.URL+"/v1", srv.URL+"/v1")
	setenv(t, h)
	port := freePort(t)
	t.Setenv("NEEDLETAIL_API_PORT", strconv.Itoa(port))

	var stderr bytes.Buffer
	var mu sync.Mutex
	logs := &lockedWriter{w: &stderr, mu: &mu}
	if code := Run(context.Background(), []string{"healthcheck"}, io.Discard, io.Discard); code != 1 {
		t.Errorf("healthcheck before serve = %d", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- Run(ctx, []string{"serve"}, io.Discard, logs) }()
	healthy := false
	for i := 0; i < 100 && !healthy; i++ {
		healthy = Run(context.Background(), []string{"healthcheck"}, io.Discard, io.Discard) == 0
		if !healthy {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !healthy {
		cancel()
		t.Fatalf("never healthy: %s", stderr.String())
	}
	call := func(ctx context.Context, method, path string, body any) (int, []byte, error) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), rd)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		return res.StatusCode, raw, err
	}
	conv := adapter.NewRuntime().Conversations(engine.APICall(call))
	reply, tip, err := conv.Send(context.Background(), "chat-1", "hello")
	if err != nil || tip != "chat-1" || !strings.HasPrefix(reply, "Saw: ") || strings.Contains(reply, crmToken) {
		t.Errorf("chat = %q %q %v", reply, tip, err)
	}
	if core.keys[0] != "Bearer "+coreKey {
		t.Errorf("core saw %v", core.keys)
	}
	tr, err := conv.Transcript(context.Background(), "chat-1", 0)
	if err != nil || len(tr.Messages) != 4 || tr.Messages[2].Tool != "terminal" {
		t.Errorf("transcript = %+v %v", tr, err)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("serve exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range []string{crmToken, cloudKey, coreKey, apiKey} {
		if strings.Contains(stderr.String(), s) {
			t.Errorf("log leaked %s", s)
		}
	}
	if !strings.Contains(stderr.String(), "serving agent atlas") {
		t.Errorf("logs = %s", stderr.String())
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestUsage(t *testing.T) {
	var out bytes.Buffer
	if Run(context.Background(), nil, &out, &out) != 2 || Run(context.Background(), []string{"bogus"}, &out, &out) != 2 {
		t.Error("usage exit")
	}
	out.Reset()
	if Run(context.Background(), []string{"needletail", "version"}, &out, &out) != 0 || !strings.HasPrefix(out.String(), "needletail ") {
		t.Errorf("version = %q", out.String())
	}
	if Run(context.Background(), []string{"oneshot", "--format", "xml", "-q", "x"}, &out, &out) != 2 {
		t.Error("bad format accepted")
	}
}

// The terminal gets an explicit env without the runtime's own secrets, even if one came back into
// this process' env after boot scrubbed it.
func TestTerminalEnvClean(t *testing.T) {
	h := home(t, "http://127.0.0.1:1/v1", "http://127.0.0.1:1/v1")
	setenv(t, h)
	rt, err := boot("", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", cloudKey)
	env := strings.Join(rt.agent.Env.Environ(), "\n")
	if strings.Contains(env, cloudKey) || strings.Contains(env, apiKey) || !strings.Contains(env, "CRM_API_TOKEN="+crmToken) || !strings.Contains(env, "SWARM_CORE_KEY=") {
		t.Errorf("terminal env:\n%s", env)
	}
}
