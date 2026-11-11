package core

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/link"
)

// chatFake plays an agent's engine API for device chat.
type chatFake struct {
	mu    sync.Mutex
	hold  chan struct{}
	sent  []string
	newID int
}

func (f *chatFake) List(context.Context, int) ([]engine.Conversation, error) {
	return []engine.Conversation{{ID: "c1", Title: "Quotes", Preview: strings.Repeat("p", 300), Messages: 4, LastActive: "2026-11-10T20:00:00Z"}}, nil
}

func (f *chatFake) Transcript(_ context.Context, id string, _ int) (*engine.Transcript, error) {
	if id != "c1" {
		return nil, engine.ErrNotFound
	}
	return &engine.Transcript{ID: "c1", Messages: []engine.Message{
		{ID: "1", Role: "user", Kind: engine.MessageText, Content: "hello"},
		{ID: "2", Role: "assistant", Kind: engine.MessageTool, Content: "web_search(...)", Tools: []string{"web_search"}},
		{ID: "3", Role: "tool", Kind: engine.MessageTool, Content: "results"},
		{ID: "4", Role: "assistant", Kind: engine.MessageText, Content: strings.Repeat("a", 5000)},
	}}, nil
}

func (f *chatFake) New(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newID++
	return fmt.Sprintf("new%d", f.newID), nil
}

func (f *chatFake) Send(ctx context.Context, id, msg string) (string, string, error) {
	f.mu.Lock()
	f.sent = append(f.sent, id+":"+msg)
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	return "reply to " + msg, id, nil
}

func (f *chatFake) Delete(context.Context, string) (int, error) { return 0, nil }

// gatewayDocker plays docker for the device gateway: it records the environment of each boot.
type gatewayDocker struct {
	mu      sync.Mutex
	addr    string
	running bool
	env     []string
	runs    []string
}

func (g *gatewayDocker) run(_ context.Context, env []string, args ...string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch args[0] {
	case "run":
		g.running, g.env = true, env
		g.runs = append(g.runs, strings.Join(args, " "))
	case "port":
		return []byte(g.addr + "\n"), nil
	case "inspect":
		return []byte(fmt.Sprint(g.running) + "\n"), nil
	case "stop", "rm":
		g.running = false
	}
	return nil, nil
}

func (g *gatewayDocker) key() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.env {
		if v, ok := strings.CutPrefix(e, "STORMO_INGRESS_KEY="); ok {
			return v
		}
	}
	return ""
}

func gatewayCapability() instance.Capability {
	return instance.Capability{Name: "devices.gateway", Kind: "service", Image: "gw:1", Placement: "local", Port: 8080, Health: "/device/v1/health",
		IdleMinutes: 10, BootMinutes: 1, Concurrency: 1, TimeoutMinutes: 15, Files: "none", AlwaysOn: true, Ingress: true, ReadOnly: true,
		Publish: "127.0.0.1:18650"}
}

func deviceJWK(k *ecdsa.PrivateKey) JWK {
	b, _ := k.PublicKey.Bytes()
	return JWK{Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(b[1:33]), Y: base64.RawURLEncoding.EncodeToString(b[33:])}
}

func TestLinkedDevices(t *testing.T) {
	_, inst := fixture(t)
	inst.CoreLoopbackTrust = false
	inst.Capabilities = []instance.Capability{gatewayCapability()}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer health.Close()
	dk := &gatewayDocker{addr: strings.TrimPrefix(health.URL, "http://")}
	services := link.NewServices("acme")
	services.Docker = dk.run
	conv := &chatFake{}
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true,
		Roster: func() []BusAgent {
			return []BusAgent{{ID: "atlas", Unit: "sales", Running: true}, {ID: "nova", Unit: "media"}}
		},
		CapabilityGrants: func(string) []string { return []string{"devices.gateway"} },
		Services:         services, CapabilityImage: func(string) error { return nil }, CapabilityKeepEvery: 50 * time.Millisecond,
		Conversations: func(string) (engine.Conversations, error) { return conv, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	do := func(method, path, auth string, body any, hdr ...string) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
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
	const owner = "owner-token-0123456789"
	errCode := func(m map[string]any) string {
		e, _ := m["error"].(map[string]any)
		s, _ := e["code"].(string)
		return s
	}

	// The core boots the always-on gateway by itself and hands it a key and its own address.
	var key string
	for i := 0; i < 100 && key == ""; i++ {
		time.Sleep(20 * time.Millisecond)
		key = dk.key()
	}
	if !strings.HasPrefix(key, "sti_") {
		t.Fatalf("no ingress key in the gateway's environment: %v", dk.env)
	}
	dk.mu.Lock()
	run, env := dk.runs[0], strings.Join(dk.env, " ")
	dk.mu.Unlock()
	for _, want := range []string{"-p 127.0.0.1:18650:8080", "--read-only", "--cap-drop ALL"} {
		if !strings.Contains(run, want) {
			t.Errorf("gateway run lacks %q: %s", want, run)
		}
	}
	if strings.Contains(run, key) || strings.Contains(run, ":/cache") || !strings.Contains(env, fmt.Sprintf("STORMO_CORE_URL=http://host.docker.internal:%d", c.Addr.Port)) {
		t.Errorf("key in argv, a cache volume, or no core URL: %s / %s", run, env)
	}
	// Kept running: no second boot while it is up.
	time.Sleep(200 * time.Millisecond)
	if dk.mu.Lock(); len(dk.runs) != 1 {
		t.Errorf("rebooted while running: %d boots", len(dk.runs))
	}
	dk.mu.Unlock()

	// The ingress key reaches /api/ingress/* and nothing else; nobody else reaches those.
	for _, p := range []string{"/api/fleet", "/api/devices", "/api/tokens", "/api/capabilities", "/health"} {
		if st, _ := do("GET", p, key, nil); st != 401 {
			t.Errorf("ingress key on %s: %d", p, st)
		}
	}
	for _, who := range []string{"", owner, atlasKey} {
		if st, _ := do("POST", "/api/ingress/pairings", who, map[string]any{}); st != 401 && st != 403 {
			t.Errorf("%q on /api/ingress: %d", who, st)
		}
	}

	// A device asks to pair.
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwk := deviceJWK(k)
	st, p := do("POST", "/api/ingress/pairings", key, map[string]any{"name": "Watch", "model": "Pixel Watch 3", "os": "Wear OS 6", "appVersion": "0.1.0",
		"publicKey": jwk, "attestation": []string{base64.StdEncoding.EncodeToString([]byte("cert"))}, "ip": "203.0.113.7", "country": "IT"})
	if st != 201 || p["fingerprint"] != KeyFingerprint(jwk) || len(NormalizeUserCode(p["userCode"].(string))) != 8 {
		t.Fatalf("pairing: %d %v", st, p)
	}
	code, pairing := p["userCode"].(string), p["pairing"].(string)
	if st, _ := do("POST", "/api/ingress/pairings", key, map[string]any{"name": "W", "model": "M", "publicKey": JWK{Kty: "EC", Crv: "P-256", X: jwk.X, Y: jwk.X}}); st != 400 {
		t.Errorf("a key off the curve: %d", st)
	}
	if st, s := do("GET", "/api/ingress/pairings/"+pairing, key, nil); st != 200 || s["state"] != "pending" {
		t.Errorf("pending: %d %v", st, s)
	}
	// The owner sees it waiting, never its code, and links it by typing the code.
	st, list := do("GET", "/api/devices", owner, nil)
	if st != 200 || len(list["pairings"].([]any)) != 1 || strings.Contains(fmt.Sprint(list), NormalizeUserCode(code)) {
		t.Fatalf("devices: %d %v", st, list)
	}
	if st, _ := do("POST", "/api/devices/link", owner, map[string]any{"userCode": "BBBB-BBBB", "preview": true}); st != 404 {
		t.Errorf("wrong code: %d", st)
	}
	st, pv := do("POST", "/api/devices/link", owner, map[string]any{"userCode": strings.ToLower(strings.ReplaceAll(code, "-", " ")), "preview": true})
	if pr, _ := pv["pairing"].(map[string]any); st != 200 || pr["model"] != "Pixel Watch 3" || pr["ip"] != "203.0.113.7" || pr["fingerprint"] != KeyFingerprint(jwk) {
		t.Fatalf("preview: %d %v", st, pv)
	}
	if st, _ := do("POST", "/api/devices/link", owner, map[string]any{"userCode": code, "grants": map[string]any{"chat": []string{"nobody"}}}); st != 400 {
		t.Errorf("chat with an unknown agent: %d", st)
	}
	readToken, _, _ := c.DB.AddToken("reader", ScopeRead, 0)
	if st, _ := do("POST", "/api/devices/link", readToken, map[string]any{"userCode": code, "grants": map[string]any{"status": true}}); st != 403 {
		t.Errorf("linking with a read token: %d", st)
	}
	st, ln := do("POST", "/api/devices/link", owner, map[string]any{"userCode": code, "name": "My watch", "grants": map[string]any{"status": true, "review": true, "chat": []string{"atlas"}}})
	dv, _ := ln["device"].(map[string]any)
	if st != 201 || dv["name"] != "My watch" || dv["attestation"] != 1.0 {
		t.Fatalf("link: %d %v", st, ln)
	}
	dev := dv["id"].(string)
	if st, s := do("GET", "/api/ingress/pairings/"+pairing, key, nil); st != 200 || s["state"] != "approved" || s["deviceId"] != dev {
		t.Fatalf("approved: %d %v", st, s)
	}
	if _, s := do("GET", "/api/ingress/pairings/"+pairing, key, nil); s["state"] != "consumed" {
		t.Errorf("a pairing is read once: %v", s)
	}
	if st, d := do("GET", "/api/ingress/devices/"+dev, key, nil); st != 200 || !strings.Contains(fmt.Sprint(d["publicKey"]), jwk.X) {
		t.Errorf("device key: %d %v", st, d)
	}

	// On behalf of the device: its grants, checked here.
	as := func(method, path string, body any) (int, map[string]any) {
		return do(method, path, key, body, "X-Stormo-Device", dev, "X-Stormo-Client-Ip", "198.51.100.9")
	}
	if st, out := do("GET", "/api/ingress/me", key, nil); st != 401 || errCode(out) != "device_unknown" {
		t.Errorf("no device: %d %v", st, out)
	}
	if st, me := as("GET", "/api/ingress/me", nil); st != 200 || me["instance"] != "Acme Swarm" {
		t.Errorf("me: %d %v", st, me)
	}
	if st, f := as("GET", "/api/ingress/fleet", nil); st != 200 || f["agents"] == nil {
		t.Errorf("fleet: %d %v", st, f)
	}
	st, rv := as("GET", "/api/ingress/review", nil)
	items, _ := rv["items"].([]any)
	byID := map[string]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		byID[m["kind"].(string)+"/"+m["id"].(string)] = m
	}
	if st != 200 || byID["lesson/l1"]["appOnly"] != false || byID["lesson/l2"] != nil || byID["skill/quote-check"]["appOnly"] != true {
		t.Fatalf("review: %d %v", st, rv)
	}
	skillV := byID["skill/quote-check"]["version"].(string)
	if st, out := as("POST", "/api/ingress/review", map[string]string{"agent": "atlas", "kind": "skill", "id": "quote-check", "version": skillV, "decision": "accept"}); st != 400 || errCode(out) != "app_only" {
		t.Errorf("a skill from a device: %d %v", st, out)
	}
	if st, out := as("POST", "/api/ingress/review", map[string]string{"agent": "atlas", "kind": "skill", "id": "quote-check", "version": "x", "decision": "accept"}); st != 400 || errCode(out) != "bad_request" {
		t.Errorf("a malformed version: %d %v", st, out)
	}
	v1 := byID["lesson/l1"]["version"].(string)
	if st, _ := as("POST", "/api/ingress/review", map[string]string{"agent": "atlas", "kind": "lesson", "id": "l1", "version": v1, "decision": "promote"}); st != 400 {
		t.Errorf("promote from a device: %d", st)
	}
	if st, out := as("POST", "/api/ingress/review", map[string]string{"agent": "atlas", "kind": "lesson", "id": "l1", "version": "0000000000000000", "decision": "accept"}); st != 409 || errCode(out) != "changed" {
		t.Errorf("a stale version: %d %v", st, out)
	}
	if st, out := as("POST", "/api/ingress/review", map[string]string{"agent": "atlas", "kind": "lesson", "id": "l1", "version": v1, "decision": "accept"}); st != 200 {
		t.Errorf("accept a lesson: %d %v", st, out)
	}
	if _, rv := as("GET", "/api/ingress/review", nil); strings.Contains(fmt.Sprint(rv), "id:l1 ") {
		t.Errorf("l1 still open: %v", rv)
	}
	if _, a := do("GET", "/api/audit?limit=5", owner, nil); !strings.Contains(fmt.Sprint(a), "device:My watch (devices.gateway)") {
		t.Errorf("audit: %v", a)
	}

	// Chat: only granted agents; text rows only; a turn in the background, one at a time.
	if st, _ := as("GET", "/api/ingress/agents/nova/conversations", nil); st != 403 {
		t.Errorf("nova is not granted: %d", st)
	}
	if st, out := as("GET", "/api/ingress/agents/atlas/conversations/c1", nil); st != 200 || len(out["messages"].([]any)) != 2 ||
		len([]rune(out["messages"].([]any)[1].(map[string]any)["content"].(string))) != deviceMessageMax+1 {
		t.Errorf("transcript: %d %v", st, out)
	}
	if st, _ := as("GET", "/api/ingress/agents/atlas/conversations/zz", nil); st != 404 {
		t.Errorf("no conversation: %d", st)
	}
	conv.mu.Lock()
	conv.hold = make(chan struct{})
	conv.mu.Unlock()
	st, tr := as("POST", "/api/ingress/agents/atlas/chat", map[string]string{"message": "hi"})
	if st != 202 || tr["conversation"] != "new1" {
		t.Fatalf("chat: %d %v", st, tr)
	}
	turn := tr["turn"].(string)
	if st, out := as("POST", "/api/ingress/agents/atlas/chat", map[string]string{"conversation": "c1", "message": "again"}); st != 409 || errCode(out) != "busy" {
		t.Errorf("a second turn at once: %d %v", st, out)
	}
	if _, tv := as("GET", "/api/ingress/turns/"+turn, nil); tv["state"] != "running" {
		t.Errorf("running: %v", tv)
	}
	close(conv.hold)
	var tv map[string]any
	for i := 0; i < 100; i++ {
		if _, tv = as("GET", "/api/ingress/turns/"+turn, nil); tv["state"] == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if tv["reply"] != "reply to hi" {
		t.Errorf("turn: %v", tv)
	}
	if st, _ := do("GET", "/api/ingress/turns/"+turn, key, nil, "X-Stormo-Device", "dev_ffffffffffffffff"); st != 401 {
		t.Errorf("another device's turn: %d", st)
	}

	// Turns: a poll needs the chat grant still; two starts at once run one turn.
	conv.mu.Lock()
	conv.hold = make(chan struct{})
	conv.mu.Unlock()
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = as("POST", "/api/ingress/agents/atlas/chat", map[string]string{"conversation": "c1", "message": fmt.Sprint("m", i)})
		}()
	}
	wg.Wait()
	accepted := 0
	for _, s := range codes {
		if s == 202 {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("concurrent starts: %v", codes)
	}
	var running string
	c.devices.mu.Lock()
	for id, tn := range c.devices.turns {
		if tn.State == "running" {
			running = id
		}
	}
	c.devices.mu.Unlock()
	if st, _ := do("PATCH", "/api/devices/"+dev, owner, map[string]any{"grants": map[string]any{"status": true, "review": true, "chat": []string{}}}); st != 200 {
		t.Fatal("patch")
	}
	if st, out := as("GET", "/api/ingress/turns/"+running, nil); st != 403 || errCode(out) != "forbidden" || strings.Contains(fmt.Sprint(out), "reply") {
		t.Errorf("a turn read after its chat grant was taken away: %d %v", st, out)
	}
	close(conv.hold)

	// The owner widens the grants, then revokes the device.
	if st, _ := do("PATCH", "/api/devices/"+dev, owner, map[string]any{"grants": map[string]any{"status": true, "chat": []string{"*"}}}); st != 200 {
		t.Errorf("patch: %d", st)
	}
	if st, _ := as("GET", "/api/ingress/agents/nova/conversations", nil); st != 200 {
		t.Errorf("nova after *: %d", st)
	}
	if st, _ := as("GET", "/api/ingress/review", nil); st != 403 {
		t.Errorf("review after it was taken away: %d", st)
	}
	if st, _ := do("DELETE", "/api/devices/"+dev, owner, nil); st != 200 {
		t.Errorf("revoke: %d", st)
	}
	if st, out := as("GET", "/api/ingress/me", nil); st != 401 || errCode(out) != "device_unknown" {
		t.Errorf("revoked: %d %v", st, out)
	}
	if st, _ := do("GET", "/api/ingress/devices/"+dev, key, nil); st != 401 {
		t.Errorf("revoked key lookup: %d", st)
	}

	// Asleep, the gateway's key is dead and it stays down; a wake mints another.
	if _, err := c.Broker.Sleep("devices.gateway"); err != nil {
		t.Fatal(err)
	}
	if st, _ := do("GET", "/api/ingress/pairings/"+pairing, key, nil); st != 401 {
		t.Errorf("key of a sleeping gateway: %d", st)
	}
	time.Sleep(200 * time.Millisecond)
	if services.State("devices.gateway").State != "asleep" {
		t.Error("the keeper woke a gateway the owner put to sleep")
	}
	if _, err := c.Broker.Wake("devices.gateway"); err != nil {
		t.Fatal(err)
	}
	var key2 string
	for i := 0; i < 100; i++ {
		if key2 = dk.key(); key2 != key && services.State("devices.gateway").State == "awake" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if key2 == key {
		t.Fatal("a wake kept the old key")
	}
	if st, _ := do("GET", "/api/ingress/pairings/"+pairing, key, nil); st != 401 {
		t.Errorf("old key after a wake: %d", st)
	}
	if st, _ := do("GET", "/api/ingress/pairings/"+pairing, key2, nil); st != 200 {
		t.Errorf("new key: %d", st)
	}

	// Agents never see or reach it.
	if g := c.Broker.Granted("atlas"); len(g) != 0 {
		t.Errorf("granted to an agent: %v", g)
	}
	for _, tl := range CapabilityTools(c.Broker) {
		if tl.Name == "cap_describe" {
			if _, err := tl.Run(ToolCall{Agent: "atlas", Args: []byte(`{"capability":"devices.gateway"}`)}); err == nil {
				t.Error("cap_describe reached the gateway")
			}
		}
	}

	// Too many wrong codes: the owner waits.
	for i := 0; i < linkFailsAllowed; i++ {
		do("POST", "/api/devices/link", owner, map[string]any{"userCode": "BBBB-BBBB", "preview": true})
	}
	if st, out := do("POST", "/api/devices/link", owner, map[string]any{"userCode": "BBBB-BBBB", "preview": true}); st != 429 || errCode(out) != "rate_limited" {
		t.Errorf("wrong codes: %d %v", st, out)
	}
}

func TestPairingCapsAndIdleDevices(t *testing.T) {
	db := testDB(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Unix(1_700_000_000, 0)
	req := PairingRequest{Name: "Watch", Model: "M", PublicKey: deviceJWK(k), IP: "203.0.113.1"}
	ids := []string{}
	for i := 0; i < maxPairingsPerIP+1; i++ {
		now = now.Add(time.Second)
		id, _, _, err := db.AddPairing(req, now)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	ps, _ := db.PendingPairings(now)
	if len(ps) != maxPairingsPerIP || ps[len(ps)-1].ID != ids[1] {
		t.Errorf("per-address cap evicts the oldest: %v", ps)
	}
	for i := 0; i < maxPairings+5; i++ {
		req.IP = fmt.Sprintf("198.51.100.%d", i)
		now = now.Add(time.Second)
		if _, _, _, err := db.AddPairing(req, now); err != nil {
			t.Fatal(err)
		}
	}
	if ps, _ := db.PendingPairings(now); len(ps) != maxPairings {
		t.Errorf("total cap: %d", len(ps))
	}
	if st, _, _, _ := db.PairingState(ids[0], now); st != "expired" {
		t.Errorf("an evicted pairing reads expired: %s", st)
	}
	if _, _, _, err := db.AddPairing(PairingRequest{Name: "W\x07", Model: "M", PublicKey: deviceJWK(k)}, now); err == nil {
		t.Error("control characters in a name")
	}
	ps, _ = db.PendingPairings(now)
	dv, err := db.ApprovePairing(ps[0].ID, "", DeviceGrants{Status: true}, now)
	if err != nil || dv.Name != "Watch" {
		t.Fatalf("approve: %v %+v", err, dv)
	}
	if _, err := db.ApprovePairing(ps[0].ID, "", DeviceGrants{}, now); !errors.Is(err, errNotPending) {
		t.Errorf("approved twice: %v", err)
	}
	if gone, _ := db.ExpireIdleDevices(30*24*time.Hour, now.Add(29*24*time.Hour)); len(gone) != 0 {
		t.Errorf("expired early: %v", gone)
	}
	db.touchDevice(dv.ID, "203.0.113.9", "PT", now.Add(20*24*time.Hour))
	if gone, _ := db.ExpireIdleDevices(30*24*time.Hour, now.Add(40*24*time.Hour)); len(gone) != 0 {
		t.Errorf("seen 20 days in, expired at 40: %v", gone)
	}
	if gone, _ := db.ExpireIdleDevices(30*24*time.Hour, now.Add(51*24*time.Hour)); len(gone) != 1 {
		t.Errorf("idle 31 days: %v", gone)
	}
	if d, _, _ := db.Device(dv.ID); d.Revoked == nil || d.RevokedReason != "idle" || d.LastIP != "203.0.113.9" || d.Country != "PT" {
		t.Errorf("idle device: %+v", d)
	}
}

func TestKeyFingerprintMatchesTheContract(t *testing.T) {
	x, y := make([]byte, 32), make([]byte, 32)
	for i := range x {
		x[i], y[i] = byte(i+1), byte(i+33)
	}
	j := JWK{Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(x), Y: base64.RawURLEncoding.EncodeToString(y)}
	if f := KeyFingerprint(j); f != "B756-48F2-D3AD" {
		t.Errorf("fingerprint %s", f)
	}
	if NormalizeUserCode(" wdjb-mjht ") != "WDJBMJHT" {
		t.Error("normalize")
	}
	for i := 0; i < 200; i++ {
		c := newUserCode()
		if len(c) != 9 || c[4] != '-' || strings.Trim(strings.ReplaceAll(c, "-", ""), userCodeAlphabet) != "" {
			t.Fatalf("code %q", c)
		}
	}
}

// A request a proxy forwarded is never loopback, however it reached the listener.
func TestProxiedRequestsLoseLoopbackTrust(t *testing.T) {
	_, inst := fixture(t)
	inst.CoreLoopbackTrust = true
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	get := func(h ...string) int {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/fleet", c.Addr.Port), nil)
		for i := 0; i+1 < len(h); i += 2 {
			req.Header.Set(h[i], h[i+1])
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if st := get(); st != 200 {
		t.Fatalf("direct loopback: %d", st)
	}
	for _, h := range []string{"Cf-Connecting-Ip", "X-Real-Ip", "Via", "True-Client-Ip", "X-Forwarded-Proto"} {
		if st := get(h, "203.0.113.7"); st != 401 {
			t.Errorf("%s: %d", h, st)
		}
	}
}

func TestGrantFlags(t *testing.T) {
	cur := DeviceGrants{Status: true, Review: true, Chat: []string{"atlas"}}
	for _, c := range []struct {
		grant, agents string
		want          string
		bad           bool
	}{
		{"", "", "status review chat:atlas", false},
		{"status", "", "status", false},
		{"status,chat", "", "status chat:atlas", false},
		{"", "*", "status review chat:*", false},
		{"review", "nova,atlas", "review chat:nova,atlas", false},
		{"none", "", "none", false},
		{"admin", "", "", true},
	} {
		g, err := parseGrantFlags(c.grant, c.agents, cur)
		if (err != nil) != c.bad || (!c.bad && grantsText(g) != c.want) {
			t.Errorf("%q %q: %s %v", c.grant, c.agents, grantsText(g), err)
		}
	}
	if _, err := parseGrantFlags("chat", "", DeviceGrants{}); err == nil {
		t.Error("chat without agents")
	}
}

// A device can only decide exactly the lesson it was shown: not a look-alike id, not one flagged since.
func TestDeviceDecidesExactlyWhatItSaw(t *testing.T) {
	_, inst := fixture(t)
	root := inst.Root
	lesson := func(id, status, pii string) string {
		return fmt.Sprintf(`{"id":"%s","kind":"memory","scope":"agent","agent":"atlas","unit":"sales","text":"t %s","pii":[%s],"status":"%s"}`, id, id, pii, status)
	}
	// l10 comes first and is flagged; a prefix match on "l1" would land on it.
	write(t, root+"/agents/atlas/learnings/ledger.jsonl", lesson("l10", "proposed", `"phone"`)+"\n"+lesson("l1", "proposed", "")+"\n")
	c := &Core{inst: inst, Learner: NewLearner(LearnerOptions{Inst: inst, DB: testDB(t)})}
	v := lessonVersion("l1", "memory", "agent", "proposed", "t l1", []string{})
	if err := c.deviceDecide("atlas", "lesson", "l1", v, "accept"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(root + "/agents/atlas/learnings/ledger.jsonl")
	if !strings.Contains(string(b), `"id":"l1"`) || strings.Count(string(b), `"status":"accepted"`) != 1 || !strings.Contains(string(b), `"id":"l10"`) {
		t.Fatalf("ledger: %s", b)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(line, `"id":"l10"`) && !strings.Contains(line, `"status":"proposed"`) {
			t.Errorf("l10 was decided: %s", line)
		}
	}
	// Flagged since it was shown: refused.
	write(t, root+"/agents/atlas/learnings/ledger.jsonl", lesson("l2", "proposed", `"email"`)+"\n")
	if err := c.deviceDecide("atlas", "lesson", "l2", lessonVersion("l2", "memory", "agent", "proposed", "t l2", []string{}), "accept"); !errors.Is(err, errChanged) {
		t.Errorf("flagged after it was shown: %v", err)
	}
}

func TestIdleDeviceIsRefusedAtOnceAndTurnsLeaveOnTime(t *testing.T) {
	db := testDB(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Unix(1_700_000_000, 0)
	id, _, _, _ := db.AddPairing(PairingRequest{Name: "Watch", Model: "M", PublicKey: deviceJWK(k)}, now)
	dv, _ := db.ApprovePairing(id, "", DeviceGrants{Status: true}, now)
	if _, ok, _ := db.LiveDevice(dv.ID, 30*24*time.Hour, now.Add(29*24*time.Hour)); !ok {
		t.Fatal("live at 29 days")
	}
	if _, ok, _ := db.LiveDevice(dv.ID, 30*24*time.Hour, now.Add(31*24*time.Hour)); ok {
		t.Fatal("idle 31 days and still live")
	}
	if log, _ := db.AuditLog(10); len(log) == 0 || log[0].Action != "device.revoke" || log[0].Detail != "idle" {
		t.Errorf("an idle unlink at authorization is audited: %+v", log)
	}
	db.touchDevice(dv.ID, "203.0.113.1", "", now.Add(32*24*time.Hour))
	if d, _, _ := db.Device(dv.ID); d.RevokedReason != "idle" || d.LastSeen != nil {
		t.Errorf("an unlinked device was touched back: %+v", d)
	}

	c := &Core{}
	c.devices.turns = map[string]*deviceTurn{
		"old":  {State: "done", finished: time.Now().Add(-deviceTurnKeep - time.Minute)},
		"new":  {State: "done", finished: time.Now()},
		"busy": {State: "running"},
	}
	c.PruneDeviceTurns()
	if _, ok := c.devices.turns["old"]; ok || len(c.devices.turns) != 2 {
		t.Errorf("turns after prune: %v", c.devices.turns)
	}
}

func TestDecodeStrict(t *testing.T) {
	var v struct {
		A string `json:"a"`
	}
	for body, ok := range map[string]bool{`{"a":"x"}`: true, "{\"a\":\"x\"}\n": true, `{"a":"x"} {"a":"y"}`: false, `{"a":"x"}]`: false,
		`{"a":"x","b":1}`: false, `{"a":"` + strings.Repeat("x", 100) + `"}`: false} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if err := decodeStrict(r, 64, &v); (err == nil) != ok {
			t.Errorf("%.30q: %v", body, err)
		}
	}
}

// Two writers of an agent's learnings never interleave: the second waits for the first.
func TestLedgerLockSerializesWriters(t *testing.T) {
	root := t.TempDir()
	unlock, err := learning.LockLedger(root, "atlas", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := learning.LockLedger(root, "atlas", 100*time.Millisecond); !errors.Is(err, learning.ErrLedgerBusy) {
		t.Errorf("a second writer while held: %v", err)
	}
	if u, err := learning.LockLedger(root, "nova", 100*time.Millisecond); err != nil {
		t.Errorf("another agent's learnings are not held: %v", err)
	} else {
		u()
	}
	unlock()
	u, err := learning.LockLedger(root, "atlas", 100*time.Millisecond)
	if err != nil {
		t.Errorf("after release: %v", err)
	} else {
		u()
	}
}
