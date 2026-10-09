package llm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeKeys map[string]string

func (k fakeKeys) AgentFor(h string) (string, bool) {
	a, ok := k[strings.TrimPrefix(h, "Bearer ")]
	return a, ok
}

// Each agent's calls go to its own connection's gateway; the core decides, by agent.
func TestGatewaysRouteByAgent(t *testing.T) {
	keys := fakeKeys{"key-atlas": "atlas", "key-nova": "nova"}
	gw := func(model string) *Gateway {
		dir := t.TempDir()
		return NewGateway(GatewayOptions{Auth: NewChatGPTAuth(AuthOptions{Paths: Paths(dir)}), Keys: keys,
			Models: []ModelInfo{{ID: model}}, Log: func(string) {}})
	}
	route := map[string]string{"atlas": "work", "nova": ""}
	gs := NewGateways("chatgpt", keys, func(a string) string { return route[a] }, []string{"chatgpt", "work"}, []*Gateway{gw("default-model"), gw("work-model")})
	models := func(key string) string {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		if !gs.Handle(w, r) {
			t.Fatal("not handled")
		}
		return w.Body.String()
	}
	if b := models("key-atlas"); !strings.Contains(b, "work-model") {
		t.Errorf("atlas went to %s", b)
	}
	if b := models("key-nova"); !strings.Contains(b, "default-model") {
		t.Errorf("nova went to %s", b)
	}
	route["atlas"] = "gone"
	if b := models("key-atlas"); !strings.Contains(b, "default-model") {
		t.Errorf("an unknown connection must fall back to the default: %s", b)
	}
	s := gs.Status()
	if len(s.Connections) != 2 || s.Connections[0].Name != "chatgpt" || s.Connections[1].Name != "work" || s.Login != LoginMissing {
		t.Errorf("status = %+v", s)
	}
	if r := httptest.NewRequest(http.MethodGet, "/health", nil); gs.Handle(httptest.NewRecorder(), r) {
		t.Error("/health is not the gateway's")
	}
}
