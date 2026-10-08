package llm_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/core/llm"
)

const (
	key    = "atlas-core-key-0123456789abcdef"
	client = "oaiapp_test123"
)

// ---- a fake OpenAI issuer (discovery, JWKS, token endpoint) and API with a real RSA key --------

var (
	rsaOnce sync.Once
	rsaKey  *rsa.PrivateKey
)

func signingKey(t *testing.T) *rsa.PrivateKey {
	rsaOnce.Do(func() {
		var err error
		if rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			t.Fatal(err)
		}
	})
	return rsaKey
}

type call struct {
	path   string
	body   string
	header http.Header
}

type fake struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []call
	upstream func(w http.ResponseWriter, r *http.Request, body string)
	catalog  any
}

func (f *fake) issuer() string { return f.srv.URL }
func (f *fake) api() string    { return f.srv.URL + "/v1" }
func (f *fake) tokenPath() string {
	return "/api/accounts/oauth/token"
}

func (f *fake) setUpstream(fn func(w http.ResponseWriter, r *http.Request, body string)) {
	f.mu.Lock()
	f.upstream = fn
	f.mu.Unlock()
}

func (f *fake) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call{}, f.calls...)
}

func (f *fake) responsesCalls() []call {
	var out []call
	for _, c := range f.recorded() {
		if c.path == "/v1/responses" {
			out = append(out, c)
		}
	}
	return out
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (f *fake) idToken(claims map[string]any) string {
	now := time.Now().Unix()
	c := map[string]any{"iss": f.issuer(), "aud": client, "sub": "user-1", "email": "owner@example.test", "iat": now, "exp": now + 3600}
	for k, v := range claims {
		c[k] = v
	}
	head := b64(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + b64(c)
	digest := sha256.Sum256([]byte(head))
	sig, err := rsa.SignPKCS1v15(rand.Reader, signingKey(f.t), crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return head + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeSSE(w http.ResponseWriter, events []map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, sseBody(events))
}

func sseBody(events []map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		d, _ := json.Marshal(e)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", e["type"], d)
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var textEvents = []map[string]any{
	{"type": "response.created", "response": map[string]any{"id": "r1"}},
	{"type": "response.output_item.added", "item": map[string]any{"type": "reasoning", "id": "rs_1"}},
	{"type": "response.output_item.done", "item": map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}, "encrypted_content": "ENC"}},
	{"type": "response.output_item.added", "item": map[string]any{"type": "message", "id": "msg_1"}},
	{"type": "response.output_text.delta", "item_id": "msg_1", "delta": "Hello"},
	{"type": "response.output_text.delta", "item_id": "msg_1", "delta": " world"},
	{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13, "input_tokens_details": map[string]any{"cached_tokens": 4}}}},
}

var toolEvents = []map[string]any{
	{"type": "response.output_item.added", "item": map[string]any{"type": "reasoning", "id": "rs_2"}},
	{"type": "response.output_item.done", "item": map[string]any{"type": "reasoning", "id": "rs_2", "summary": []any{}, "encrypted_content": "ENC2"}},
	{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_abc", "namespace": llm.ToolNamespace, "name": "get_order", "arguments": ""}},
	{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": `{"order_id":`},
	{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": `"ABC123"}`},
	{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_abc", "namespace": llm.ToolNamespace, "name": "get_order", "arguments": `{"order_id":"ABC123"}`}},
	{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 20, "output_tokens": 5}}},
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t}
	f.upstream = func(w http.ResponseWriter, r *http.Request, body string) { writeSSE(w, textEvents) }
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeJSON(w, 200, map[string]any{"issuer": f.issuer(), "authorization_endpoint": f.issuer() + "/api/accounts/authorize", "token_endpoint": f.issuer() + f.tokenPath(), "jwks_uri": f.issuer() + "/.well-known/jwks.json"})
			return
		case "/.well-known/jwks.json":
			pub := signingKey(t).PublicKey
			writeJSON(w, 200, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())}}})
			return
		case "/v1/models":
			f.mu.Lock()
			cat := f.catalog
			f.mu.Unlock()
			if cat == nil {
				writeJSON(w, 404, map[string]any{})
			} else {
				writeJSON(w, 200, cat)
			}
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, call{path: r.URL.Path, body: string(b), header: r.Header.Clone()})
		up := f.upstream
		f.mu.Unlock()
		up(w, r, string(b))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func conn(over func(c *llm.Connection)) *llm.Connection {
	c := &llm.Connection{Version: 1, ClientID: client, ExtAgentHostID: "urn:uuid:host", Subject: "user-1", Email: "owner@example.test", IDToken: "x",
		AccessToken: "at-1", RefreshToken: "rt-1", TokenType: "Bearer", ExpiresAt: float64(time.Now().Add(time.Hour).UnixMilli()),
		Scopes: []string{"chatgpt.tokens.use.direct"}, SavedAt: time.Now().UTC().Format(time.RFC3339)}
	if over != nil {
		over(c)
	}
	return c
}

type env struct {
	gw    *llm.Gateway
	auth  *llm.ChatGPTAuth
	paths llm.AuthPaths
}

func setup(t *testing.T, f *fake, connection *llm.Connection, noConnection bool, opts func(o *llm.GatewayOptions)) env {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets.local.yaml")
	if err := os.WriteFile(secrets, []byte("local:\n  agents:\n    atlas:\n      SWARM_CORE_KEY: "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := llm.Paths(filepath.Join(dir, "auth"))
	if err := os.MkdirAll(filepath.Join(dir, "auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !noConnection {
		if connection == nil {
			connection = conn(nil)
		}
		b, _ := json.Marshal(connection)
		if err := os.WriteFile(paths.Connection, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	auth := llm.NewChatGPTAuth(llm.AuthOptions{Paths: paths, Client: f.srv.Client(), Issuer: f.issuer()})
	o := llm.GatewayOptions{Auth: auth, Keys: core.NewAgentKeys(secrets), Client: f.srv.Client(), BaseURL: f.api(), Log: func(string) {}}
	if opts != nil {
		opts(&o)
	}
	return env{gw: llm.NewGateway(o), auth: auth, paths: paths}
}

func chatReq(body any, k string) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "http://core/v1/chat/completions", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer "+k)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func chat(gw *llm.Gateway, body any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	gw.Handle(rec, chatReq(body, key))
	return rec
}

var hi = map[string]any{"model": "gpt-6-luna", "messages": []any{map[string]any{"role": "user", "content": "x"}}}

func with(base map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func readSSE(t *testing.T, body string) []any {
	var out []any
	for _, block := range strings.Split(body, "\n\n") {
		d, ok := strings.CutPrefix(block, "data: ")
		if !ok {
			continue
		}
		if d == "[DONE]" {
			out = append(out, "[DONE]")
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(d), &v); err != nil {
			t.Fatalf("bad sse data %q", d)
		}
		out = append(out, v)
	}
	return out
}

func asJSON(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(asJSON(got), asJSON(want)) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		t.Errorf("%s:\n got  %s\n want %s", what, gb, wb)
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return v
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	return decode(t, rec)["error"].(map[string]any)["code"].(string)
}

func request(t *testing.T, js string) *llm.ChatRequest {
	var r llm.ChatRequest
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

// ---- translate -------------------------------------------------------------------------------

func TestTranslateRequest(t *testing.T) {
	body := asJSON(llm.ToResponsesBody(request(t, `{
		"model": "openai/gpt-6-luna",
		"messages": [
			{"role": "system", "content": "You are Atlas."},
			{"role": "user", "content": "find ABC123"},
			{"role": "assistant", "content": null, "tool_calls": [{"id": "call_abc", "type": "function", "function": {"name": "get_order", "arguments": "{\"order_id\":\"ABC123\"}"}}]},
			{"role": "tool", "tool_call_id": "call_abc", "content": "{\"ok\":true}"},
			{"role": "user", "content": [{"type": "text", "text": "and this?"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,AA"}}]}
		],
		"tools": [{"type": "function", "function": {"name": "get_order", "description": "d", "parameters": {"type": "object"}}}],
		"temperature": 0.2,
		"max_tokens": 100
	}`), "atlas", nil)).(map[string]any)
	eq(t, "model", body["model"], "gpt-6-luna")
	eq(t, "instructions", body["instructions"], "You are Atlas.")
	eq(t, "store", body["store"], false)
	eq(t, "stream", body["stream"], true)
	eq(t, "input", body["input"], []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "find ABC123"}}},
		map[string]any{"type": "function_call", "call_id": "call_abc", "namespace": llm.ToolNamespace, "name": "get_order", "arguments": `{"order_id":"ABC123"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_abc", "output": `{"ok":true}`},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "and this?"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA"}}},
	})
	eq(t, "tools", body["tools"], []any{map[string]any{"type": "namespace", "name": llm.ToolNamespace, "description": "Tools provided by the calling agent.",
		"tools": []any{map[string]any{"type": "function", "name": "get_order", "description": "d", "strict": false, "parameters": map[string]any{"type": "object"}}}}})
	eq(t, "tool_choice", body["tool_choice"], "auto")
	for _, k := range []string{"temperature", "max_tokens", "max_output_tokens", "top_p", "metadata"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s must not be sent", k)
		}
	}
	for _, i := range body["input"].([]any) {
		if i.(map[string]any)["role"] == "system" {
			t.Error("system role item sent")
		}
	}
	if n := len(body["prompt_cache_key"].(string)); n > 64 {
		t.Errorf("prompt_cache_key is %d chars", n)
	}
}

func TestTranslateReplaysReasoning(t *testing.T) {
	cache := llm.NewReasoningCache()
	cache.Set([]string{"call_abc"}, []any{map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "ENC2"}})
	body := asJSON(llm.ToResponsesBody(request(t, `{"model": "gpt-6-luna", "messages": [{"role": "assistant", "content": "", "tool_calls": [{"id": "call_abc", "function": {"name": "f", "arguments": "{}"}}]}]}`), "atlas", cache)).(map[string]any)
	input := body["input"].([]any)
	var types []any
	for _, i := range input {
		types = append(types, i.(map[string]any)["type"])
	}
	eq(t, "types", types, []any{"reasoning", "function_call"})
	if _, ok := input[0].(map[string]any)["id"]; ok {
		t.Error("replayed reasoning must not carry its id")
	}
}

func TestClampEffort(t *testing.T) {
	for in, want := range map[string]string{"max": "max", "ultra": "max", "minimal": "low", "High": "high", "turbo": ""} {
		if got := llm.ClampEffort(in, nil); got != want {
			t.Errorf("ClampEffort(%q) = %q, want %q", in, got, want)
		}
	}
	none := asJSON(llm.ToResponsesBody(request(t, `{"model": "gpt-6-luna", "messages": [], "reasoning_effort": "none"}`), "a", nil)).(map[string]any)
	eq(t, "none", none["reasoning"], map[string]any{"effort": "none"})
	max := asJSON(llm.ToResponsesBody(request(t, `{"model": "gpt-6-luna", "messages": [], "reasoning": {"effort": "max"}}`), "a", nil)).(map[string]any)
	eq(t, "max", max["reasoning"], map[string]any{"effort": "max", "summary": "auto"})
}

// ---- gateway ---------------------------------------------------------------------------------

func TestUnknownKey401(t *testing.T) {
	f := newFake(t)
	e := setup(t, f, nil, false, nil)
	rec := httptest.NewRecorder()
	e.gw.Handle(rec, chatReq(hi, "nope-nope-nope-nope"))
	if rec.Code != 401 || len(f.recorded()) != 0 {
		t.Fatalf("status %d, calls %d", rec.Code, len(f.recorded()))
	}
}

func modelsReq() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://core/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

func TestModelsCatalog(t *testing.T) {
	f := newFake(t)
	before := setup(t, f, nil, true, nil)
	rec := httptest.NewRecorder()
	before.gw.Handle(rec, modelsReq())
	data := decode(t, rec)["data"].([]any)
	eq(t, "defaults", []any{data[0].(map[string]any)["id"], data[0].(map[string]any)["context_length"]}, []any{"gpt-6-luna", 272000})

	f.catalog = map[string]any{"models": []any{
		map[string]any{"slug": "gpt-6-luna", "display_name": "GPT-6 Luna", "visibility": "list", "context_window": 400000},
		map[string]any{"slug": "hidden", "display_name": "H", "visibility": "hide"},
		map[string]any{"slug": "gpt-6-sol", "display_name": "GPT-6 Sol", "visibility": "list"},
	}}
	e := setup(t, f, nil, false, nil)
	rec = httptest.NewRecorder()
	e.gw.Handle(rec, modelsReq())
	var got [][]any
	for _, m := range decode(t, rec)["data"].([]any) {
		got = append(got, []any{m.(map[string]any)["id"], m.(map[string]any)["context_length"]})
	}
	eq(t, "catalog", got, [][]any{{"gpt-6-luna", 400000}, {"gpt-6-sol", 272000}})
	if rec := chat(e.gw, with(hi, "model", "hidden")); rec.Code != 404 {
		t.Errorf("hidden model: %d", rec.Code)
	}
}

func choicesOf(c any) []any {
	m, ok := c.(map[string]any)
	if !ok {
		return nil
	}
	ch, _ := m["choices"].([]any)
	return ch
}

func TestStreamsText(t *testing.T) {
	f := newFake(t)
	e := setup(t, f, nil, false, nil)
	rec := chat(e.gw, with(hi, "stream", true))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	chunks := readSSE(t, rec.Body.String())
	if chunks[len(chunks)-1] != "[DONE]" {
		t.Fatal("no [DONE]")
	}
	var text strings.Builder
	var finish, usage any
	for _, c := range chunks {
		for _, x := range choicesOf(c) {
			d, _ := x.(map[string]any)["delta"].(map[string]any)
			if s, ok := d["content"].(string); ok {
				text.WriteString(s)
			}
			if fr := x.(map[string]any)["finish_reason"]; fr != nil && finish == nil {
				finish = fr
			}
		}
		if m, ok := c.(map[string]any); ok && m["usage"] != nil && usage == nil {
			usage = m["usage"]
		}
	}
	eq(t, "text", text.String(), "Hello world")
	eq(t, "role", choicesOf(chunks[0])[0].(map[string]any)["delta"].(map[string]any)["role"], "assistant")
	eq(t, "finish", finish, "stop")
	u := usage.(map[string]any)
	eq(t, "usage", []any{u["prompt_tokens"], u["completion_tokens"], u["prompt_tokens_details"].(map[string]any)["cached_tokens"]}, []any{10, 3, 4})
	h := f.responsesCalls()[0].header
	eq(t, "headers", []string{h.Get("Authorization"), h.Get("Content-Type"), h.Get("Accept")}, []string{"Bearer at-1", "application/json", "text/event-stream"})
	us := e.gw.Status().Usage["atlas"]
	eq(t, "usage counters", []int{us.Requests, us.OK, us.InputTokens, us.OutputTokens}, []int{1, 1, 10, 3})
}

func TestStreamsToolCallAndReplays(t *testing.T) {
	f := newFake(t)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) { writeSSE(w, toolEvents) })
	e := setup(t, f, nil, false, nil)
	rec := chat(e.gw, with(hi, "stream", true, "tools", []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_order"}}}))
	var deltas []any
	var finish any
	for _, c := range readSSE(t, rec.Body.String()) {
		for _, x := range choicesOf(c) {
			d, _ := x.(map[string]any)["delta"].(map[string]any)
			if tc, ok := d["tool_calls"].([]any); ok {
				deltas = append(deltas, tc...)
			}
			if fr := x.(map[string]any)["finish_reason"]; fr != nil && finish == nil {
				finish = fr
			}
		}
	}
	eq(t, "first delta", deltas[0], map[string]any{"index": 0, "id": "call_abc", "type": "function", "function": map[string]any{"name": "get_order", "arguments": ""}})
	var args strings.Builder
	for _, d := range deltas {
		args.WriteString(d.(map[string]any)["function"].(map[string]any)["arguments"].(string))
	}
	eq(t, "args", args.String(), `{"order_id":"ABC123"}`)
	eq(t, "finish", finish, "tool_calls")

	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) { writeSSE(w, textEvents) })
	chat(e.gw, map[string]any{"model": "gpt-6-luna", "messages": []any{
		map[string]any{"role": "user", "content": "x"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_abc", "type": "function", "function": map[string]any{"name": "get_order", "arguments": `{"order_id":"ABC123"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_abc", "content": "{}"},
	}})
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.responsesCalls()[1].body), &sent)
	input := sent["input"].([]any)
	var types []any
	for _, i := range input {
		types = append(types, i.(map[string]any)["type"])
	}
	eq(t, "types", types, []any{"message", "reasoning", "function_call", "function_call_output"})
	eq(t, "replayed", input[1].(map[string]any)["encrypted_content"], "ENC2")
	eq(t, "namespace", input[2].(map[string]any)["namespace"], llm.ToolNamespace)
}

func TestNonStreaming(t *testing.T) {
	f := newFake(t)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) { writeSSE(w, toolEvents) })
	e := setup(t, f, nil, false, nil)
	body := decode(t, chat(e.gw, hi))
	eq(t, "object", body["object"], "chat.completion")
	choice := body["choices"].([]any)[0].(map[string]any)
	eq(t, "finish", choice["finish_reason"], "tool_calls")
	eq(t, "call", choice["message"].(map[string]any)["tool_calls"].([]any)[0], map[string]any{"id": "call_abc", "type": "function", "function": map[string]any{"name": "get_order", "arguments": `{"order_id":"ABC123"}`}})
	eq(t, "total", body["usage"].(map[string]any)["total_tokens"], 25)
}

func TestInStreamPlanCodes(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		ours   string
	}{
		{"subscription_sharing_usage_limit_exceeded", 429, "usage_limit_reached"},
		{"subscription_sharing_usage_unavailable", 503, "plan_usage_unavailable"},
		{"subscription_sharing_user_not_eligible", 403, "plan_user_not_eligible"},
		{"subscription_sharing_route_not_supported", 403, "plan_route_not_supported"},
		{"subscription_sharing_invalid_user", 401, "plan_invalid_user"},
		{"subscription_sharing_unsupported_capability", 400, "plan_unsupported_capability"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			f := newFake(t)
			f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
				writeSSE(w, []map[string]any{{"type": "response.created"}, {"type": "response.failed", "response": map[string]any{"error": map[string]any{"code": tc.code, "message": "plan says no"}}}})
			})
			e := setup(t, f, nil, false, nil)
			rec := chat(e.gw, with(hi, "stream", true))
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d", rec.Code, tc.status)
			}
			eq(t, "code", errCode(t, rec), tc.ours)
			if tc.status == 503 && rec.Header().Get("Retry-After") != "30" {
				t.Errorf("retry-after %q", rec.Header().Get("Retry-After"))
			}
			if strings.HasSuffix(tc.code, "invalid_user") && e.auth.State() != llm.LoginRelogin {
				t.Errorf("state %s", e.auth.State())
			}
		})
	}
}

func TestPlanLimitHold(t *testing.T) {
	f := newFake(t)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		writeSSE(w, []map[string]any{{"type": "response.failed", "response": map[string]any{"error": map[string]any{"code": "subscription_sharing_usage_limit_exceeded", "message": "limit"}}}})
	})
	e := setup(t, f, nil, false, func(o *llm.GatewayOptions) { o.PlanHoldSeconds = 1200 })
	rec := chat(e.gw, hi)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1200" {
		t.Fatalf("status %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	er := decode(t, rec)["error"].(map[string]any)
	eq(t, "type", er["type"], "usage_limit_reached")
	eq(t, "resets", er["resets_in_seconds"], 1200)
	if msg := er["message"].(string); !strings.Contains(msg, "usage limit has been reached") || !strings.Contains(msg, "resets_in_seconds: 1200") {
		t.Errorf("message %q", msg)
	}
	if rec := chat(e.gw, hi); rec.Code != 429 {
		t.Errorf("second call %d", rec.Code)
	}
	if n := len(f.responsesCalls()); n != 1 {
		t.Errorf("upstream calls %d", n)
	}
	if e.gw.Status().PlanLimitedUntil == nil {
		t.Error("status must show the hold")
	}
}

func TestHTTPLevelStatuses(t *testing.T) {
	for _, tc := range [][2]int{{429, 429}, {403, 403}, {503, 503}, {500, 502}} {
		f := newFake(t)
		f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
			writeJSON(w, tc[0], map[string]any{"error": map[string]any{"code": "x", "message": "m"}})
		})
		if rec := chat(setup(t, f, nil, false, nil).gw, hi); rec.Code != tc[1] {
			t.Errorf("upstream %d → %d, want %d", tc[0], rec.Code, tc[1])
		}
	}
}

func TestContentType(t *testing.T) {
	f := newFake(t)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>")
	})
	if rec := chat(setup(t, f, nil, false, nil).gw, hi); rec.Code != 502 {
		t.Errorf("html: %d", rec.Code)
	}
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		w.Header()["Content-Type"] = nil // no sniffing: a stream without Content-Type
		_, _ = io.WriteString(w, sseBody(textEvents))
	})
	if rec := chat(setup(t, f, nil, false, nil).gw, hi); rec.Code != 200 {
		t.Errorf("untyped stream: %d %s", rec.Code, rec.Body.String())
	}
}

func TestNotSignedIn(t *testing.T) {
	f := newFake(t)
	e := setup(t, f, nil, true, nil)
	rec := chat(e.gw, hi)
	if rec.Code != 401 || errCode(t, rec) != "core_login_missing" || len(f.recorded()) != 0 {
		t.Fatalf("status %d body %s calls %d", rec.Code, rec.Body.String(), len(f.recorded()))
	}
}

func TestUpstream401RefreshesOnce(t *testing.T) {
	f := newFake(t)
	var n atomic.Int32
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		if r.URL.Path == f.tokenPath() {
			writeJSON(w, 200, map[string]any{"access_token": "at-2", "refresh_token": "rt-2", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		if n.Add(1) == 1 {
			writeJSON(w, 401, map[string]any{})
			return
		}
		writeSSE(w, textEvents)
	})
	e := setup(t, f, nil, false, nil)
	if rec := chat(e.gw, hi); rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var saved map[string]any
	b, _ := os.ReadFile(e.paths.Connection)
	_ = json.Unmarshal(b, &saved)
	eq(t, "rotated", saved["refresh_token"], "rt-2")
	if info, _ := os.Stat(e.paths.Connection); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	var form url.Values
	var paths []string
	for _, c := range f.recorded() {
		paths = append(paths, c.path)
		if c.path == f.tokenPath() {
			form, _ = url.ParseQuery(c.body)
		}
	}
	eq(t, "form", form, url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {"rt-1"}, "resource": {"https://api.openai.com/v1"}})
	eq(t, "calls", paths, []string{"/v1/responses", f.tokenPath(), "/v1/responses"})
}

func TestUpstreamNeverAnswers(t *testing.T) {
	f := newFake(t)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) { <-r.Context().Done() })
	e := setup(t, f, nil, false, func(o *llm.GatewayOptions) {
		o.HeadersTimeout = 50 * time.Millisecond
		o.Concurrency = 1
	})
	rec := chat(e.gw, hi)
	if rec.Code != 504 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if s := e.gw.Status(); s.Inflight != 0 || len(s.Active) != 0 {
		t.Errorf("slot not released: %+v", s)
	}
}

// ---- chatgpt auth ----------------------------------------------------------------------------

func expiringSoon(c *llm.Connection) {
	c.ExpiresAt = float64(time.Now().Add(30 * time.Second).UnixMilli())
}

func kindOf(err error) string {
	var ae *llm.AuthError
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return ""
}

func TestConcurrentRefreshOneCall(t *testing.T) {
	f := newFake(t)
	var tokenCalls atomic.Int32
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		tokenCalls.Add(1)
		time.Sleep(20 * time.Millisecond)
		writeJSON(w, 200, map[string]any{"access_token": "at-3", "refresh_token": "rt-3", "token_type": "Bearer", "expires_in": 3600})
	})
	e := setup(t, f, conn(expiringSoon), false, nil)
	var wg sync.WaitGroup
	got := make([]string, 3)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = e.auth.AccessToken(context.Background())
		}()
	}
	wg.Wait()
	if tokenCalls.Load() != 1 {
		t.Errorf("token calls %d", tokenCalls.Load())
	}
	eq(t, "tokens", got, []string{"at-3", "at-3", "at-3"})
}

func TestReusedRefreshToken(t *testing.T) {
	f := newFake(t)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		writeJSON(w, 400, map[string]any{"error": "refresh_token_reused"})
	})
	e := setup(t, f, conn(expiringSoon), false, nil)
	if _, err := e.auth.AccessToken(context.Background()); kindOf(err) != "relogin" {
		t.Fatalf("err %v", err)
	}
	if e.auth.State() != llm.LoginRelogin {
		t.Fatalf("state %s", e.auth.State())
	}
	time.Sleep(10 * time.Millisecond)
	b, _ := json.Marshal(conn(func(c *llm.Connection) { c.AccessToken, c.RefreshToken = "at-new", "rt-new" }))
	if err := os.WriteFile(e.paths.Connection, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if e.auth.State() != llm.LoginOK {
		t.Fatalf("state after new sign-in %s", e.auth.State())
	}
	if tok, err := e.auth.AccessToken(context.Background()); tok != "at-new" || err != nil {
		t.Fatalf("token %q %v", tok, err)
	}
}

func TestRefreshedDifferentAccount(t *testing.T) {
	f := newFake(t)
	other := f.idToken(map[string]any{"sub": "someone-else"})
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		writeJSON(w, 200, map[string]any{"access_token": "at-4", "refresh_token": "rt-4", "id_token": other, "token_type": "Bearer", "expires_in": 3600})
	})
	e := setup(t, f, conn(expiringSoon), false, nil)
	if _, err := e.auth.AccessToken(context.Background()); kindOf(err) != "relogin" {
		t.Fatalf("err %v", err)
	}
	if e.auth.State() != llm.LoginRelogin {
		t.Fatalf("state %s", e.auth.State())
	}
}

func callback(authorize string, extra url.Values) error {
	a, err := url.Parse(authorize)
	if err != nil {
		return err
	}
	cb, err := url.Parse(a.Query().Get("redirect_uri"))
	if err != nil {
		return err
	}
	q := url.Values{"state": {a.Query().Get("state")}}
	for k, v := range extra {
		q[k] = v
	}
	cb.RawQuery = q.Encode()
	res, err := http.Get(cb.String())
	if err == nil {
		res.Body.Close()
	}
	return err
}

func TestBrowserSignIn(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var authorize *url.URL
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		if r.URL.Path != f.tokenPath() {
			http.Error(w, "?", 404)
			return
		}
		mu.Lock()
		q := authorize.Query()
		mu.Unlock()
		form, _ := url.ParseQuery(body)
		ok := form.Get("grant_type") == "authorization_code" && form.Get("client_id") == client && form.Get("resource") == "https://api.openai.com/v1" && form.Get("redirect_uri") == q.Get("redirect_uri")
		// The PKCE verifier hashes to the challenge sent to the browser.
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
			writeJSON(w, 400, map[string]any{"error": "bad_exchange"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"access_token": "at-signin", "refresh_token": "rt-signin", "token_type": "Bearer", "expires_in": 3600, "earliest_refresh_at": 1_900_000_000,
			"scope":    "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
			"id_token": f.idToken(map[string]any{"nonce": q.Get("nonce")}),
		})
	})
	paths := llm.Paths(filepath.Join(t.TempDir(), "auth"))
	var out []string
	port := 0
	c, err := llm.SignIn(context.Background(), llm.SignInOptions{
		Paths: paths, Client: f.srv.Client(), Issuer: f.issuer(), AppName: "Acme Swarm", Port: &port, Timeout: 10 * time.Second,
		Print: func(l string) { out = append(out, l) },
		OpenBrowser: func(u string) error {
			mu.Lock()
			authorize, _ = url.Parse(u)
			mu.Unlock()
			return callback(u, url.Values{"code": {"the-code"}, "client_id": {client}, "scope": {"x"}})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	q := authorize.Query()
	eq(t, "endpoint", authorize.Scheme+"://"+authorize.Host+authorize.Path, f.issuer()+"/api/accounts/authorize")
	eq(t, "client_id", q.Get("client_id"), "dynamic_agent_client")
	eq(t, "scope", q.Get("scope"), "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct")
	eq(t, "resource", q.Get("resource"), "https://api.openai.com/v1")
	eq(t, "method", q.Get("code_challenge_method"), "S256")
	eq(t, "app", q.Get("agent_name_hint"), "Acme Swarm")
	if !regexp.MustCompile(`^urn:uuid:[0-9a-f-]{36}$`).MatchString(q.Get("ext_agent_host_id")) {
		t.Errorf("host id %q", q.Get("ext_agent_host_id"))
	}
	if cb, _ := url.Parse(q.Get("redirect_uri")); cb.Path != "/auth/callback" {
		t.Errorf("redirect %q", q.Get("redirect_uri"))
	}
	eq(t, "conn", []any{c.ClientID, c.Subject, c.Email, c.AccessToken, *c.EarliestRefreshAt}, []any{client, "user-1", "owner@example.test", "at-signin", 1_900_000_000_000.0})
	if info, _ := os.Stat(paths.Connection); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
	var reg map[string]any
	b, _ := os.ReadFile(paths.Registration)
	_ = json.Unmarshal(b, &reg)
	eq(t, "registration", reg, map[string]any{"ext_agent_host_id": q.Get("ext_agent_host_id"), "client_id": client})
	if !strings.Contains(strings.Join(out, "\n"), "Continue with ChatGPT") {
		t.Errorf("printed %q", out)
	}
	// The saved connection reads back as a signed-in auth (the file is the TS core's format).
	if s := llm.NewChatGPTAuth(llm.AuthOptions{Paths: paths}).State(); s != llm.LoginOK {
		t.Errorf("state %s", s)
	}
}

func TestSignInWrongNonce(t *testing.T) {
	f := newFake(t)
	f.setUpstream(func(w http.ResponseWriter, r *http.Request, body string) {
		writeJSON(w, 200, map[string]any{"access_token": "a", "refresh_token": "r", "token_type": "Bearer", "expires_in": 3600,
			"scope": "openid offline_access chatgpt.tokens.use.direct", "id_token": f.idToken(map[string]any{"nonce": "not-it"})})
	})
	paths := llm.Paths(filepath.Join(t.TempDir(), "auth"))
	port := 0
	_, err := llm.SignIn(context.Background(), llm.SignInOptions{
		Paths: paths, Client: f.srv.Client(), Issuer: f.issuer(), AppName: "Acme Swarm", Port: &port, Timeout: 10 * time.Second,
		OpenBrowser: func(u string) error { return callback(u, url.Values{"code": {"c"}, "client_id": {client}}) },
	})
	if err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(paths.Connection); !os.IsNotExist(err) {
		t.Error("connection saved")
	}
}

// A connection file written earlier (its exact field names and number formats) loads.
func TestReadsExistingConnectionFile(t *testing.T) {
	dir := t.TempDir()
	paths := llm.Paths(dir)
	existing := fmt.Sprintf(`{
  "version": 1,
  "client_id": "oaiapp_x",
  "ext_agent_host_id": "urn:uuid:00000000-0000-4000-8000-000000000000",
  "subject": "user-1",
  "email": "owner@example.test",
  "id_token": "a.b.c",
  "access_token": "at-old",
  "refresh_token": "rt-old",
  "token_type": "Bearer",
  "expires_at": %d,
  "earliest_refresh_at": 1900000000000,
  "scopes": ["chatgpt.tokens.use.direct"],
  "saved_at": "2026-10-07T12:00:00.000Z"
}
`, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(paths.Connection, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	a := llm.NewChatGPTAuth(llm.AuthOptions{Paths: paths})
	if tok, err := a.AccessToken(context.Background()); tok != "at-old" || err != nil {
		t.Fatalf("token %q %v", tok, err)
	}
	if acc := a.Account(); acc == nil || acc.Email != "owner@example.test" || acc.ClientID != "oaiapp_x" {
		t.Fatalf("account %+v", acc)
	}
}
