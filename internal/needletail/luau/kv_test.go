package luau

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var kvRW = []string{"kv:read", "kv:write"}

// The key-value store: kv_get needs kv:read, kv_set and kv_delete kv:write (declared and granted);
// values are any JSON value, kept across runtimes in state/needletail/kv.json (0600), and expire
// after their ttl on the runtime's clock.
func TestKV(t *testing.T) {
	rt, _ := newRuntime(t)
	fails(t, rt, kvRW, `return kv_get("a")`, "denied") // declared, not granted
	rt.Grants = append(rt.Grants, "kv:read")
	fails(t, rt, kvRW, `kv_set("a", 1)`, "denied")
	rt.Grants = append(rt.Grants, "kv:write")
	fails(t, rt, []string{"kv:write"}, `return kv_get("a")`, "denied") // write does not give read
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rt.Now = func() time.Time { return now }
	res, _ := exec(t, rt, kvRW, `
		kv_set("seen:C1:abc", true, 3600)
		kv_set("profile", {name = "x", n = 2, tags = {"a", "b"}})
		kv_set("count", 41)
		kv_set("count", kv_get("count") + 1)
		return {kv_get("seen:C1:abc"), kv_get("profile"), kv_get("count"), kv_get("missing") == nil, kv_delete("count"), kv_delete("count"), kv_get("count") == nil}`)
	wantValue(t, res, `[true, {"n": 2, "name": "x", "tags": ["a", "b"]}, 42, true, true, false, true]`)
	p := filepath.Join(rt.Home, "state", "needletail", "kv.json")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("kv.json %v %v", st, err)
	}
	// another runtime on the same home reads it; the ttl runs on the runtime's clock
	rt2, _ := newRuntime(t)
	rt2.Home, rt2.Grants = rt.Home, rt.Grants
	rt2.Now = func() time.Time { return now.Add(59 * time.Minute) }
	res, _ = exec(t, rt2, kvRW, `return {kv_get("seen:C1:abc"), kv_get("profile").name}`)
	wantValue(t, res, `[true, "x"]`)
	rt2.Now = func() time.Time { return now.Add(61 * time.Minute) }
	res, _ = exec(t, rt2, kvRW, `kv_set("other", "y") return {kv_get("seen:C1:abc") == nil, kv_get("profile").name}`)
	wantValue(t, res, `[true, "x"]`)
	// the expired entry is pruned from the file at the next write
	body, _ := os.ReadFile(p)
	if strings.Contains(string(body), "seen:C1:abc") || !strings.Contains(string(body), `"other"`) {
		t.Fatalf("kv.json %s", body)
	}
	var f struct {
		V       int                        `json:"v"`
		Entries map[string]json.RawMessage `json:"entries"`
	}
	if json.Unmarshal(body, &f) != nil || f.V != 1 || len(f.Entries) != 2 {
		t.Fatalf("kv.json %s", body)
	}
}

// The store's bounds: keys, values, ttl and the number of keys; secret values refused; a dry run
// (luau_test) reads but never writes.
func TestKVBounds(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.Grants = append(rt.Grants, kvRW...)
	fails(t, rt, kvRW, `kv_set("", 1)`, "bad_args")
	fails(t, rt, kvRW, `kv_set(string.rep("k", 257), 1)`, "bad_args")
	fails(t, rt, kvRW, `kv_set("a\nb", 1)`, "bad_args")
	fails(t, rt, kvRW, `kv_set("a")`, "bad_args") // nil is kv_delete's
	fails(t, rt, kvRW, `kv_set("a", string.rep("x", 17000))`, "too_large")
	fails(t, rt, kvRW, `kv_set("a", 1, 0)`, "bad_args")
	fails(t, rt, kvRW, `kv_set("a", 1, 400 * 86400)`, "bad_args")
	fails(t, rt, kvRW, `kv_set("a", 1, "soon")`, "bad_args")
	fails(t, rt, kvRW, `kv_set("a", "token `+secret+`")`, "refused")
	fails(t, rt, kvRW, `kv_set("`+secret+`", 1)`, "refused")
	old := kvMaxKeys
	kvMaxKeys = 3
	t.Cleanup(func() { kvMaxKeys = old })
	fails(t, rt, kvRW, `for i = 1, 4 do kv_set("k" .. i, i) end`, "full")
	res, _ := exec(t, rt, kvRW, `kv_set("k3", "again") kv_delete("k1") kv_set("k4", 4) return {kv_get("k3"), kv_get("k4")}`)
	wantValue(t, res, `["again", 4]`)
	// a dry run reads, never writes
	call := rt.NewCall("t", "s1", Caps{}, true)
	caps, _ := NewCaps(kvRW, rt.Grants)
	call.Caps = caps
	var out map[string]any
	_ = json.Unmarshal(call.Call(context.Background(), "kv.set", []byte(`{"key":"z","value":1}`)), &out)
	if out["ok"] != false || !strings.Contains(string(mustJSON(out["err"])), "dry_run") {
		t.Fatalf("dry-run kv.set %v", out)
	}
	_ = json.Unmarshal(call.Call(context.Background(), "kv.get", []byte(`{"key":"k4"}`)), &out)
	if out["ok"] != true || out["v"] != float64(4) {
		t.Fatalf("dry-run kv.get %v", out)
	}
}

// sha256 is the hex SHA-256 of a string, with no grant: what a handler keys its state by.
func TestSHA256(t *testing.T) {
	rt, _ := newRuntime(t)
	res, _ := exec(t, rt, nil, `return {sha256("abc"), nt.hash.sha256(""), sha256("ñ")}`)
	wantValue(t, res, `["ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "`+
		"024bb90888ca89a15a19e9bdd8c712bfb070465fce1ef25e43c170ea44fc5e5f"+`"]`)
	fails(t, rt, nil, `return sha256({})`, "bad_args")
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
