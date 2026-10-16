package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestTokensScopesAndAudit(t *testing.T) {
	_, inst := fixture(t)
	inst.CoreLoopbackTrust = false
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

	// Without loopback trust nothing but /health is open, even on loopback.
	if code, _ := do("GET", "/health", "", nil); code != 200 {
		t.Errorf("/health: %d", code)
	}
	for _, p := range []string{"/api/fleet", "/api/core", "/api/instance", "/api/tokens"} {
		if code, _ := do("GET", p, "", nil); code != 401 {
			t.Errorf("%s without a token: %d", p, code)
		}
	}
	// An agent key is no person: it gets nothing outside its own routes.
	if code, _ := do("GET", "/api/fleet", atlasKey, nil); code != 401 {
		t.Errorf("fleet with an agent key: %d", code)
	}

	mint := func(name, scope string) string {
		t.Helper()
		code, out := do("POST", "/api/tokens", owner, map[string]any{"name": name, "scope": scope, "days": 30})
		if code != 201 {
			t.Fatalf("mint %s: %d %v", name, code, out)
		}
		return out["token"].(string)
	}
	reader, operator := mint("laptop", "read"), mint("ops", "operate")
	if code, _ := do("POST", "/api/tokens", owner, map[string]any{"name": "laptop", "scope": "read"}); code != 400 {
		t.Errorf("duplicate name: %d", code)
	}

	if code, _ := do("GET", "/api/fleet", reader, nil); code != 200 {
		t.Errorf("read token on fleet: %d", code)
	}
	if code, _ := do("POST", "/api/learn", reader, map[string]any{}); code != 403 {
		t.Errorf("read token acting: %d", code)
	}
	if code, _ := do("GET", "/api/tokens", operator, nil); code != 403 {
		t.Errorf("operate token on tokens: %d", code)
	}
	if code, _ := do("GET", "/api/tokens", "stc_000000000000_"+string(bytes.Repeat([]byte("0"), 48)), nil); code != 401 {
		t.Errorf("made-up token: %d", code)
	}

	// Revoked: stops at once.
	if code, _ := do("DELETE", "/api/tokens/laptop", owner, nil); code != 200 {
		t.Errorf("revoke: %d", code)
	}
	if code, _ := do("GET", "/api/fleet", reader, nil); code != 401 {
		t.Errorf("revoked token: %d", code)
	}
	code, out := do("GET", "/api/tokens", owner, nil)
	if code != 200 || len(out["tokens"].([]any)) != 1 {
		t.Errorf("live tokens: %d %v", code, out)
	}

	// Expired: refused.
	value, _, err := c.DB.AddToken("old", ScopeRead, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if code, _ := do("GET", "/api/fleet", value, nil); code != 401 {
		t.Errorf("expired token: %d", code)
	}

	// The audit log names who did what.
	_, out = do("GET", "/api/audit", owner, nil)
	entries := out["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("audit: %v", entries)
	}
	last := entries[0].(map[string]any)
	if last["actor"] != "owner" || last["action"] != "token.revoke" || last["target"] != "laptop" {
		t.Errorf("newest audit entry: %v", last)
	}
}

func TestLoopbackTrustKeepsLocalViewsOpen(t *testing.T) {
	_, inst := fixture(t)
	if !inst.CoreLoopbackTrust {
		t.Fatal("core.loopback_trust defaults to on")
	}
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	for p, want := range map[string]int{"/api/fleet": 200, "/api/instance": 200, "/api/messages": 401, "/api/tokens": 401, "/api/audit": 401} {
		r, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("%s: %d, want %d", p, r.StatusCode, want)
		}
	}
	// Through a proxy (forwarded), loopback is no longer trusted.
	req, _ := http.NewRequest("GET", base+"/api/fleet", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Errorf("forwarded request: %d", r.StatusCode)
	}
}
