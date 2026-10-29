package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/camfinc/stormo/pkg/slack"
)

func TestSlackDirectoryForTheOwner(t *testing.T) {
	_, inst := fixture(t)
	inst.CoreLoopbackTrust = false
	write(t, filepath.Join(inst.Root, "secrets.local.yaml"), "agents:\n  atlas:\n    SLACK_BOT_TOKEN: xoxb-atlas\n")
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Header.Get("Authorization") != "Bearer xoxb-atlas" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		switch r.URL.Path {
		case "/auth.test":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team": "Acme", "team_id": "T1", "user": "atlas", "user_id": "UBOT"})
		case "/users.list":
			lists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "members": []any{map[string]any{"id": "U1", "name": "ana", "profile": map[string]any{"display_name": "Ana"}}}})
		case "/conversations.list":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": []any{map[string]any{"id": "C1", "name": "sales", "is_member": true}}})
		case "/conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "D9"}})
		}
	}))
	defer srv.Close()
	saved := slack.APIBase
	slack.APIBase = srv.URL + "/"
	defer func() { slack.APIBase = saved }()

	const owner = "owner-token-0123456789"
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: owner, NoMirror: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	do := func(method, path, tok string, body any) (int, map[string]any) {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	// People's names are the owner's: no token, or an agent's key, gets nothing.
	for _, tok := range []string{"", atlasKey} {
		if code, _ := do("GET", "/api/agents/atlas/slack", tok, nil); code != 401 {
			t.Errorf("token %q: %d", tok, code)
		}
	}
	code, out := do("GET", "/api/agents/atlas/slack", owner, nil)
	if code != 200 || out["team"].(map[string]any)["name"] != "Acme" || len(out["people"].([]any)) != 1 || len(out["channels"].([]any)) != 1 {
		t.Fatalf("directory: %d %v", code, out)
	}
	// Cached: a second read does not list again; refresh=1 does.
	do("GET", "/api/agents/atlas/slack", owner, nil)
	if lists.Load() != 1 {
		t.Errorf("users.list called %d times, want 1 (cache)", lists.Load())
	}
	do("GET", "/api/agents/atlas/slack?refresh=1", owner, nil)
	if lists.Load() != 2 {
		t.Errorf("refresh: users.list called %d times", lists.Load())
	}
	// The DM with a person, added to the cached directory.
	code, out = do("POST", "/api/agents/atlas/slack/dm", owner, map[string]string{"user": "U0ANA1"})
	if code != 200 || out["channel"] != "D9" {
		t.Errorf("dm: %d %v", code, out)
	}
	if _, out = do("GET", "/api/agents/atlas/slack", owner, nil); len(out["dms"].([]any)) != 1 {
		t.Errorf("dms after open: %v", out["dms"])
	}
	if code, _ := do("POST", "/api/agents/atlas/slack/dm", owner, map[string]string{"user": "not-an-id"}); code != 400 {
		t.Errorf("bad user: %d", code)
	}
	// nova has no Slack bot token; unknown agents are 404 too.
	if code, out := do("GET", "/api/agents/nova/slack", owner, nil); code != 404 || errCode(out) != "no_slack" {
		t.Errorf("nova: %d %v", code, out)
	}
	if code, _ := do("GET", "/api/agents/ghost/slack", owner, nil); code != 404 {
		t.Errorf("ghost: %d", code)
	}
	// A token Slack refuses is the client's to fix: 502 with Slack's code.
	write(t, filepath.Join(inst.Root, "secrets.local.yaml"), "agents:\n  atlas:\n    SLACK_BOT_TOKEN: pbpaste | oops\n")
	if code, out := do("GET", "/api/agents/atlas/slack?refresh=1", owner, nil); code != 502 || errCode(out) != "slack_invalid_auth" {
		t.Errorf("bad token: %d %v", code, out)
	}
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}
