package core

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWhatsAppAccessAndConfiguration(t *testing.T) {
	_, inst := fixture(t)
	const owner = "owner-token-0123456789"
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: owner, NoMirror: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	check := func(method, token string, want int) {
		t.Helper()
		r := httptest.NewRequest(method, "/api/agents/atlas/whatsapp", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		c.handleWhatsApp(w, r, "atlas")
		if w.Code != want {
			t.Fatalf("%s: got %d: %s", method, w.Code, w.Body.String())
		}
	}
	check("GET", "", 403)
	check("POST", atlasKey, 403)
	check("GET", owner, 409)

	path := filepath.Join(inst.Root, "agents", "atlas", "agent.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "kind: slack", "kind: whatsapp"))
	write(t, path, string(raw))
	check("GET", owner, 200)

	if p := c.whatsappPolicy("missing"); p.Enabled || p.All || len(p.Users) != 0 {
		t.Fatal("missing agent grants access")
	}
	check("DELETE", owner, 200)
	check("PUT", owner, 405)
}
