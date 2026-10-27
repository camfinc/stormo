package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
)

// The learning bridge is engine-neutral: it is tested on a fake engine kind whose runtime has the
// optional engine.LearningRuntime (over Hermes' runtime, which has not), with its own key name.
const fakeLearningKind = "learning-fake"

type fakeLearningEngine struct{ engine.Engine }

func (e fakeLearningEngine) Runtime() engine.Runtime {
	return fakeLearningRuntime{e.Engine.Runtime()}
}

type fakeLearningRuntime struct{ engine.Runtime }

func (fakeLearningRuntime) LearningKeyName() string { return "FAKE_LEARNING_KEY" }
func (fakeLearningRuntime) LearningPath(session string) string {
	return "/v1/sessions/" + url.PathEscape(session) + "/learning"
}
func (fakeLearningRuntime) Conversations(call engine.APICall) engine.Conversations {
	return fakeConversations{call: call}
}

// fakeConversations lists sessions from GET /v1/sessions ({"data": [{id, title}]}).
type fakeConversations struct {
	engine.Conversations
	call engine.APICall
}

func (f fakeConversations) List(ctx context.Context, limit int) ([]engine.Conversation, error) {
	_, body, err := f.call(ctx, http.MethodGet, "/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []engine.Conversation `json:"data"`
	}
	return out.Data, json.Unmarshal(body, &out)
}

func withFakeLearningEngine(t *testing.T) {
	t.Helper()
	get := runtimeLearningEngine
	runtimeLearningEngine = func(kind string, inst *instance.Instance) (engine.Engine, error) {
		if kind != fakeLearningKind {
			return get(kind, inst)
		}
		eng, err := engines.Get("hermes", inst)
		return fakeLearningEngine{eng}, err
	}
	t.Cleanup(func() { runtimeLearningEngine = get })
}

func TestRuntimeLearningOwnerBridge(t *testing.T) {
	withFakeLearningEngine(t)
	_, inst := fixture(t)
	const normal = "ordinary-runtime-key-0123456789"
	const dedicated = "owner-learning-secret-0123456789"
	const owner = "core-owner-secret-0123456789"
	write(t, filepath.Join(inst.Root, "agents/atlas/agent.yaml"), "format: 1\nid: atlas\nname: Atlas\nunit: sales\nrole: test\nengine: {kind: "+fakeLearningKind+", version: 0.1.0}\nmodel: {name: test}\nsecrets: [API_SERVER_KEY, FAKE_LEARNING_KEY]\n")
	write(t, filepath.Join(inst.Root, "secrets.local.yaml"), "agents:\n  atlas:\n    API_SERVER_KEY: "+normal+"\n    FAKE_LEARNING_KEY: "+dedicated+"\n")
	calls, posts := 0, 0
	upstreamError := false
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/v1/sessions" {
			if r.Header.Get("Authorization") != "Bearer "+normal {
				t.Error("session request did not use conversation credential")
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "session-a", "title": "Review " + dedicated}}})
			return
		}
		if r.URL.Path != "/v1/sessions/session-a/learning" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+dedicated {
			t.Error("feedback did not use dedicated server-side credential")
		}
		if r.Method == http.MethodPost {
			posts++
			var req learning.RuntimeReviewRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				t.Error("invalid bridge body")
			}
			if !req.Valid() {
				t.Errorf("invalid operation %+v", req)
			}
		}
		if upstreamError {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": dedicated + normal + owner})
			return
		}
		json.NewEncoder(w).Encode(learning.RuntimeReviewData{Events: []learning.OutcomeEvent{}, Candidates: []learning.ReflectionCandidate{{ID: "candidate", Text: dedicated + " " + normal + " " + owner}}, Status: learning.RuntimeReviewStatus{ReflectionEnabled: true, CallsUsed: 3, CallsLimit: 100, EventsUsed: 5, EventsLimit: 4096, ExportedIDs: []string{"candidate"}, FeedbackAllowed: true, ExportAllowed: true}})
	}))
	defer runtime.Close()
	c := &Core{inst: inst, owner: owner, DB: testDB(t), keys: testKeys(t)}
	online := true
	roster := func() []BusAgent { return []BusAgent{{ID: "atlas", Running: online, Endpoint: runtime.URL}} }
	request := func(method, query, token, body string) (int, RuntimeLearningView, string) {
		t.Helper()
		r := httptest.NewRequest(method, "/api/agents/atlas/learning"+query, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		c.handleRuntimeLearning(w, r, "atlas", roster)
		var out RuntimeLearningView
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out, w.Body.String()
	}
	for _, token := range []string{"", atlasKey, normal, dedicated} {
		code, _, _ := request("GET", "", token, "")
		if code != 401 {
			t.Fatalf("credential %q got %d", token, code)
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized requests reached runtime")
	}
	code, out, raw := request("GET", "", owner, "")
	if code != 200 || out.State != "ready" || len(out.Sessions) != 1 || out.Data != nil {
		t.Fatalf("session listing: %d %+v", code, out)
	}
	if strings.Contains(raw, dedicated) {
		t.Fatal("session title leaked credential")
	}
	_, out, raw = request("GET", "?session=session-a", owner, "")
	if out.Data == nil || out.Data.Status.CallsUsed != 3 || !out.Data.Status.ReflectionEnabled {
		t.Fatalf("missing budget/status %+v", out)
	}
	for _, secret := range []string{normal, dedicated, owner} {
		if strings.Contains(raw, secret) {
			t.Fatal("response leaked credential")
		}
	}
	feedback := `{"eventId":"observed","outcome":"succeeded","origin":"check-one","summary":"Checked result"}`
	_, out, _ = request("POST", "?session=session-a", owner, feedback)
	if out.State != "ready" || posts != 1 {
		t.Fatal("feedback bridge failed")
	}
	_, out, _ = request("POST", "?session=session-a", owner, `{"exportId":"candidate"}`)
	if out.State != "ready" || posts != 2 {
		t.Fatal("export bridge failed")
	}
	before := calls
	for _, body := range []string{`{"exportId":"candidate","key":"injected"}`, `{"exportId":"candidate","eventId":"observed"}`, `{"eventId":"observed","outcome":"succeeded","origin":"check"} {}`} {
		code, _, _ := request("POST", "?session=session-a", owner, body)
		if code != 400 {
			t.Fatalf("ambiguous payload got %d", code)
		}
	}
	code, _, _ = request("GET", "?session=session-a&key=injected", owner, "")
	if code != 400 || calls != before {
		t.Fatal("client credential input was not refused before runtime")
	}
	upstreamError = true
	_, out, raw = request("POST", "?session=session-a", owner, feedback)
	if out.State != "error" || strings.Contains(raw, dedicated) || strings.Contains(raw, normal) || strings.Contains(raw, owner) {
		t.Fatalf("unsafe runtime error %s", raw)
	}
	upstreamError = false
	online = false
	_, out, _ = request("GET", "", owner, "")
	if out.State != "unavailable" {
		t.Fatalf("offline state %+v", out)
	}
	online = true
	write(t, filepath.Join(inst.Root, "secrets.local.yaml"), "agents:\n  atlas:\n    API_SERVER_KEY: "+normal+"\n")
	_, out, _ = request("GET", "", owner, "")
	if out.State != "missing_credential" {
		t.Fatalf("missing key state %+v", out)
	}
	write(t, filepath.Join(inst.Root, "agents/atlas/agent.yaml"), "format: 1\nid: atlas\nname: Atlas\nunit: sales\nrole: test\nengine: {kind: hermes, version: 0.21.5}\nmodel: {name: test}\n")
	_, out, _ = request("GET", "", owner, "")
	if out.State != "unsupported" {
		t.Fatalf("unsupported engine state %+v", out)
	}
}

func TestLearningRedactionPreservesCountersAndOverlappingSecrets(t *testing.T) {
	c := &Core{owner: "core-owner-credential"}
	view := RuntimeLearningView{State: "ready", Data: &learning.RuntimeReviewData{Candidates: []learning.ReflectionCandidate{{Text: "long-secret-value and long-secret"}}, Status: learning.RuntimeReviewStatus{EventsUsed: 3, CallsUsed: 3}}}
	view.Data.Background = &learning.BackgroundReviewData{CallsUsed: 3, Candidates: []learning.ReflectionCandidate{{Text: "long-secret-value and long-secret"}}}
	safe := c.redactLearningView(view, map[string]string{"short": "long-secret", "long": "long-secret-value", "number": "3"})
	if safe.Data == nil || safe.Data.Status.EventsUsed != 3 || safe.Data.Status.CallsUsed != 3 {
		t.Fatal("redaction changed numeric budget metadata")
	}
	if safe.Data.Background.CallsUsed != 3 || safe.Data.Background.Candidates[0].Text != "[SECRET] and [SECRET]" {
		t.Fatal("background redaction failed")
	}
	if safe.Data.Candidates[0].Text != "[SECRET] and [SECRET]" {
		t.Fatalf("overlapping secret suffix exposed: %s", safe.Data.Candidates[0].Text)
	}
}
