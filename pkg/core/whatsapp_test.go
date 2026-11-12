package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWhatsAppAccessAndConfiguration(t *testing.T) {
	_, inst := fixture(t)
	const owner = "owner-token-0123456789"
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: owner, NoMirror: true,
		Roster: func() []BusAgent { return []BusAgent{{ID: "atlas", Unit: "sales"}, {ID: "nova", Unit: "support"}} }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	request := func(handle func(http.ResponseWriter, *http.Request, string), method, path, token string, want int) {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		if handle == nil {
			c.Server.Handler.ServeHTTP(w, r)
		} else {
			handle(w, r, "atlas")
		}
		if w.Code != want {
			t.Fatalf("%s %s: got %d: %s", method, path, w.Code, w.Body.String())
		}
	}
	check := func(method, token string, want int) {
		t.Helper()
		request(c.handleWhatsApp, method, "/api/agents/atlas/whatsapp", token, want)
	}
	groups := func(method, token string, want int) {
		t.Helper()
		request(c.handleWhatsAppGroups, method, "/api/agents/atlas/whatsapp/groups", token, want)
	}
	_, out := rpc(t, c.MCP.Handle, atlasKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whatsapp_send","arguments":{"chat":"+393331234567","text":"hi"}}}`)
	if res, _ := out["result"].(map[string]any); res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "no WhatsApp channel") {
		t.Fatalf("send without a channel: %v", out)
	}
	check("GET", "", 403)
	check("POST", atlasKey, 403)
	check("GET", owner, 409)
	groups("GET", atlasKey, 403)
	groups("GET", owner, 409)

	path := filepath.Join(inst.Root, "agents", "atlas", "agent.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "kind: slack", "kind: whatsapp"))
	write(t, path, string(raw))
	check("GET", owner, 200)
	groups("POST", owner, 405)
	request(nil, "POST", "/api/agents/atlas/whatsapp/groups", owner, 405) // routed to the groups handler
	groups("GET", owner, 409)                                             // not linked

	raw = []byte(strings.ReplaceAll(string(raw), "kind: whatsapp", "kind: whatsapp\n    allowed_groups: [120363012345678901@g.us]\n    local: {allowed_groups: [120363000000000001@g.us]}"))
	write(t, path, string(raw))
	if p := c.whatsappPolicy("atlas"); len(p.Groups) != 1 || p.Groups[0] != "120363000000000001@g.us" {
		t.Fatalf("local groups not applied: %+v", p)
	}

	// whatsapp_send: as the agent, over the MCP handler (no initialize needed).
	send := func(args, want string) {
		t.Helper()
		_, out := rpc(t, c.MCP.Handle, atlasKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whatsapp_send","arguments":`+args+`}}`)
		res, _ := out["result"].(map[string]any)
		text := fmt.Sprint(res["content"])
		if res["isError"] != true || !strings.Contains(text, want) {
			t.Fatalf("%s: want an error with %q, got %v", args, want, out)
		}
	}
	send(`{"chat":"120363000000000001@g.us","text":"hi"}`, "not linked on this core")
	send(`{"chat":"120363012345678901@g.us","text":"hi"}`, "not one of your allowed_groups") // only local: applies
	send(`{"chat":"+393339999999","text":"hi"}`, "not one of your allowed_users")
	send(`{"chat":"120363000000000001@g.us","text":" "}`, "1 byte to 16 KB")
	send(`{"chat":"120363000000000001@g.us","text":"hi","as":"nova"}`, "bad arguments")
	send(`{"chat":"120363000000000001@g.us"}`, "or send an image")
	send(`{"chat":"120363000000000001@g.us","image":"/shared/support/x.png"}`, "is not mounted for you") // another unit's layer: refused before WhatsApp
	send(`{"chat":"120363000000000001@g.us","image":"/shared/group/missing.png"}`, "does not exist")
	write(t, filepath.Join(inst.Root, "workdir", "group", "pic.png"), "\x89PNG\r\n\x1a\n")
	send(`{"chat":"120363000000000001@g.us","image":"/shared/group/pic.png","text":"caption"}`, "not linked on this core")

	if p := c.whatsappPolicy("missing"); p.Enabled || p.All || len(p.Users) != 0 {
		t.Fatal("missing agent grants access")
	}
	check("DELETE", owner, 200)
	check("PUT", owner, 405)
}
