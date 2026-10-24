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

// A store file that cannot be loaded (damaged, another version, too big) is never taken for an
// empty store: every call answers the error, and nothing is written over the file, until the
// file is fixed (or replaced), when the store works again.
func TestKVLoadFailureIsSticky(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.Grants = append(rt.Grants, kvRW...)
	p := filepath.Join(rt.Home, "state", "needletail", "kv.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"v":1,"entries":{"a":{"value":1}`, `{"v":2,"entries":{}}`, `{"v":1,"entries":{"a":{"value":1}}}` + strings.Repeat(" ", 5<<20)} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ { // the second call too: the failure is not forgotten
			fails(t, rt, kvRW, `kv_set("b", 2)`, "error")
			fails(t, rt, kvRW, `return kv_delete("a")`, "error")
			fails(t, rt, kvRW, `return kv_get("a")`, "error")
		}
		if got, _ := os.ReadFile(p); string(got) != body {
			t.Fatalf("the file was written over: %.80s", got)
		}
	}
	if err := os.WriteFile(p, []byte(`{"v":1,"entries":{"a":{"value":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := exec(t, rt, kvRW, `kv_set("b", 2) return {kv_get("a"), kv_get("b")}`)
	wantValue(t, res, `[1, 2]`)
}

// The store's size is what its file holds: keys and values as they are written (no HTML escaping,
// so a '<' is one byte, not six), and the file it writes always loads again (the read bound
// covers the write bound).
func TestKVSizeIsTheFilesSize(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.Grants = append(rt.Grants, kvRW...)
	res, _ := exec(t, rt, kvRW, `
		local v = string.rep("<", 16000)
		for i = 1, 60 do kv_set("k<" .. i, v) end
		return #kv_get("k<1")`)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	wantValue(t, res, `16000`)
	p := filepath.Join(rt.Home, "state", "needletail", "kv.json")
	st, err := os.Stat(p)
	if err != nil || st.Size() > 60*16100 {
		t.Fatalf("kv.json is %d bytes for 60 values of 16000 '<' (%v)", st.Size(), err)
	}
	rt2, _ := newRuntime(t)
	rt2.Home, rt2.Grants = rt.Home, rt.Grants
	res, _ = exec(t, rt2, kvRW, `return #kv_get("k<60")`)
	if res.Err != nil {
		t.Fatalf("another runtime cannot load the file: %v", res.Err)
	}
	wantValue(t, res, `16000`)
	// a store filled to its bound with such values loads again
	res, _ = exec(t, rt2, kvRW, `
		local v = string.rep("<", 16000)
		local n = 60
		while pcall(kv_set, "more" .. n, v) do n = n + 1 end
		return n`)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if st, _ = os.Stat(p); st.Size() > KVMaxBytes+64<<10 {
		t.Errorf("a full store's file is %d bytes, over %d and its margin", st.Size(), KVMaxBytes)
	}
	rt3, _ := newRuntime(t)
	rt3.Home, rt3.Grants = rt.Home, rt.Grants
	if res, _ = exec(t, rt3, kvRW, `return #kv_get("k<1")`); res.Err != nil {
		t.Errorf("a full store's file does not load: %v", res.Err)
	}
}
