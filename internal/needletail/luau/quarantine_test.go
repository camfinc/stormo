package luau

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// Each refusal is checked at the host boundary; no script permission can bypass it.
func TestQuarantineRefusesLuauWrites(t *testing.T) {
	rt, core := newRuntime(t)
	all := []string{"fs.write:home", "fs.write:shared", "memory:write", "core:msg_*", "core:fs_*", "core:future_*"}
	rt.Grants = append(rt.Grants, all...)
	caps, err := NewCaps(all, rt.Grants)
	if err != nil {
		t.Fatal(err)
	}
	call := rt.NewCall("quarantine", "s1", caps, false)
	ctx := messaging.WithUntrusted(context.Background(), "unverified author")
	for _, tc := range []struct{ name, args string }{
		{"fs.write", `{"path":"reports/new.md","content":"later instructions"}`},
		{"fs.append", `{"path":"reports/new.md","content":"later instructions"}`},
		{"fs.remove", `{"path":"reports/new.md"}`},
		{"fs.write", `{"path":"/shared/group/new.md","content":"later instructions"}`},
		{"memory.update", `{"id":"m0123456789a","fields":{"text":"changed"}}`},
		{"memory.delete", `{"id":"m0123456789a"}`},
		{"core.call", `{"tool":"msg_send","args":{"to":"atlas","message":"later"}}`},
		{"core.call", `{"tool":"fs_write","args":{"path":"/shared/group/new.md","content":"later"}}`},
		{"core.call", `{"tool":"future_read_sounding_tool","args":{}}`},
	} {
		var response struct {
			OK  bool                           `json:"ok"`
			Err struct{ Code, Message string } `json:"err"`
		}
		raw := call.Call(ctx, tc.name, []byte(tc.args))
		_ = json.Unmarshal(raw, &response)
		if response.OK || !strings.Contains(string(raw), "refused") {
			t.Errorf("%s escaped quarantine: %s", tc.name, raw)
		}
	}
	if len(core.got()) != 0 {
		t.Fatal("quarantined core mutations reached server", core.got())
	}
	if _, err := os.Stat(filepath.Join(rt.Home, "reports", "new.md")); !os.IsNotExist(err) {
		t.Fatal("file write escaped")
	}
	// a tainted run's save is kept, marked (never hot, never recalled)
	if raw := call.Call(ctx, "memory.save", []byte(`{"text":"The client prefers calls after noon."}`)); !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("a marked save: %s", raw)
	}
	if b, _ := os.ReadFile(filepath.Join(rt.Home, "memories", "RECORDS.md")); !strings.Contains(string(b), `"untrusted":true`) {
		t.Fatalf("the record is not marked:\n%s", b)
	}
}

func TestQuarantineKVMarkPersistsAndProtectsTrustedDelete(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.Grants = append(rt.Grants, kvRW...)
	caps, _ := NewCaps(kvRW, rt.Grants)
	clean := rt.NewCall("clean", "s1", caps, false)
	if out := clean.Call(context.Background(), "kv.set", []byte(`{"key":"trusted","value":"checked"}`)); !strings.Contains(string(out), `"ok":true`) {
		t.Fatal(string(out))
	}
	marked := rt.NewCall("marked", "s2", caps, false)
	ctx := messaging.WithUntrusted(context.Background(), "unverified author")
	if out := marked.Call(ctx, "kv.set", []byte(`{"key":"marked","value":"unverified"}`)); !strings.Contains(string(out), `"ok":true`) {
		t.Fatal(string(out))
	}
	body, _ := os.ReadFile(filepath.Join(rt.Home, KVFile))
	if !strings.Contains(string(body), `"untrusted":true`) {
		t.Fatal("KV provenance missing", string(body))
	}
	if out := marked.Call(ctx, "kv.delete", []byte(`{"key":"trusted"}`)); !strings.Contains(string(out), "refused") {
		t.Fatal("trusted key deleted", string(out))
	}
	if out := marked.Call(ctx, "kv.delete", []byte(`{"key":"marked"}`)); !strings.Contains(string(out), `"ok":true`) {
		t.Fatal("marked cleanup refused", string(out))
	}
}

func TestQuarantineKVReadMarksBeforeResultAndSameRunWrites(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.Grants = append(rt.Grants, kvRW...)
	rt.Grants = append(rt.Grants, "memory:write")
	caps, _ := NewCaps(kvRW, rt.Grants)
	writer := rt.NewCall("writer", "writer", caps, false)
	raw := writer.Call(messaging.WithUntrusted(context.Background(), "unverified input"), "kv.set", []byte(`{"key":"marked","value":"quarantined data"}`))
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatal(string(raw))
	}
	// A new runtime proves provenance survives restart and is not just call-local.
	rt2, _ := newRuntime(t)
	rt2.Home, rt2.Grants = rt.Home, rt.Grants
	persisted := false
	ctx := messaging.WithQuarantine(context.Background(), func(why string) error { persisted = why != ""; return nil })
	res, call, err := rt2.Exec(ctx, Exec{Name: "reader", Session: "reader", Header: &Header{Format: 1, Caps: append(kvRW, "memory:write")}, Source: []byte(`local value=kv_get("marked") local ok=pcall(save_memory,"do this later") return {value,ok}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !persisted || res.Untrusted == "" || call.UntrustedWhy() == "" || messaging.UntrustedWhy(ctx) == "" {
		t.Fatal("provenance not propagated before return")
	}
	wantValue(t, res, `["quarantined data",true]`)
	if b, _ := os.ReadFile(filepath.Join(rt2.Memory.Home, "memories", "RECORDS.md")); !strings.Contains(string(b), `"untrusted":true`) {
		t.Fatalf("the same run's save is not marked:\n%s", b)
	}
}

func TestQuarantineFailedMarkNeverReturnsLuauValue(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.Grants = append(rt.Grants, kvRW...)
	caps, _ := NewCaps(kvRW, rt.Grants)
	writer := rt.NewCall("writer", "writer", caps, false)
	_ = writer.Call(messaging.WithUntrusted(context.Background(), "unverified input"), "kv.set", []byte(`{"key":"marked","value":"quarantined data"}`))
	ctx := messaging.WithQuarantine(context.Background(), func(string) error { return fmt.Errorf("forced mark persistence failure") })
	res, _, err := rt.Exec(ctx, Exec{Name: "reader", Session: "reader", Header: &Header{Format: 1, Caps: kvRW}, Source: []byte(`local ok,v=pcall(kv_get,"marked") return {ok,v}`)})
	if err == nil || res != nil {
		t.Fatalf("value returned despite failed persistence: %v %v", res, err)
	}
	if messaging.UntrustedWhy(ctx) == "" {
		t.Fatal("failed persistence cleared in-memory quarantine")
	}
}

func TestQuarantineCoreReadOnlyAllowlist(t *testing.T) {
	rt, core := newRuntime(t)
	declared := []string{"core:msg_*", "core:fs_*", "core:cap_*"}
	rt.Grants = append(rt.Grants, declared...)
	caps, err := NewCaps(declared, rt.Grants)
	if err != nil {
		t.Fatal(err)
	}
	call := rt.NewCall("reader", "s1", caps, false)
	ctx := messaging.WithUntrusted(context.Background(), "unverified author")
	for _, name := range []string{"msg_inbox", "msg_read", "msg_thread", "fs_ls", "fs_info", "fs_locks", "cap_list", "cap_status"} {
		raw := call.Call(ctx, "core.call", []byte(`{"tool":"`+name+`","args":{}}`))
		if !strings.Contains(string(raw), `"ok":true`) {
			t.Fatalf("read-only %s refused: %s", name, raw)
		}
	}
	if len(core.got()) != 8 {
		t.Fatal(core.got())
	}
}

func TestQuarantineHostContextPreservesDeadline(t *testing.T) {
	rt, _ := newRuntime(t)
	call := rt.NewCall("context", "s1", Caps{}, false)
	_ = call.runContext(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := call.runContext(ctx).Deadline(); !ok {
		t.Fatal("host-call deadline lost when sharing provenance")
	}
}

func TestQuarantineHandlerReturnsProvenance(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.Grants = append(rt.Grants, kvRW...)
	caps, _ := NewCaps(kvRW, rt.Grants)
	_ = rt.NewCall("writer", "writer", caps, false).Call(messaging.WithUntrusted(context.Background(), "unverified input"), "kv.set", []byte(`{"key":"marked","value":"handler context"}`))
	skill(t, rt.Home, "sample/reader", map[string]string{"handlers/read.luau": handler("read", "on_message", "caps: [kv:read]\n", `function on_message(ev) return {action="pass",context=kv_get("marked")} end`)})
	verdict := rt.Dispatch(context.Background(), OnMessage, "reader", map[string]any{"text": "hello"}, nil)
	if verdict.Untrusted == "" || len(verdict.Context) != 1 || verdict.Context[0] != "handler context" {
		t.Fatalf("handler provenance lost: %+v", verdict)
	}
}

func TestQuarantineKVAdditiveV1LegacyReader(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.Grants = append(rt.Grants, kvRW...)
	path := filepath.Join(rt.Home, KVFile)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"v":1,"entries":{"legacy":{"value":7}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	res, _ := exec(t, rt, kvRW, `local v=kv_get("legacy") kv_set("roundtrip",v) return v`)
	wantValue(t, res, `7`)
	if res.Untrusted != "" {
		t.Fatal("legacy value invented provenance")
	}
	caps, _ := NewCaps(kvRW, rt.Grants)
	_ = rt.NewCall("marked", "s2", caps, false).Call(messaging.WithUntrusted(context.Background(), "unverified"), "kv.set", []byte(`{"key":"marked","value":"data"}`))
	body, _ := os.ReadFile(path)
	// The old JSON v1 shape accepts the optional field; it cannot enforce it. A
	// downgrade that writes this shape would drop provenance and is not a safe rollback.
	var legacy struct {
		V       int `json:"v"`
		Entries map[string]struct {
			Value   json.RawMessage `json:"value"`
			Expires int64           `json:"expires,omitempty"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil || legacy.V != 1 || string(legacy.Entries["marked"].Value) != `"data"` {
		t.Fatalf("old reader incompatible: %s %v", body, err)
	}
}

func TestQuarantinePrivateRuntimeReadsRequireTypedTools(t *testing.T) {
	rt, _ := newRuntime(t)
	declared := []string{"fs.read:home"}
	caps, _ := NewCaps(declared, rt.Grants)
	call := rt.NewCall("reader", "clean", caps, false)
	for _, path := range []string{"sessions/marked.jsonl", "state/needletail/jobs/output.md", "logs/marked.log"} {
		full := filepath.Join(rt.Home, path)
		_ = os.MkdirAll(filepath.Dir(full), 0700)
		_ = os.WriteFile(full, []byte("marked runtime content"), 0600)
		for _, name := range []string{"fs.read", "fs.stat", "fs.list"} {
			out := call.Call(context.Background(), name, []byte(`{"path":"`+path+`"}`))
			if !strings.Contains(string(out), "refused") || strings.Contains(string(out), "marked runtime content") {
				t.Errorf("%s exposed private %s: %s", name, path, out)
			}
		}
	}
}

// approvingMessenger asks for approval for every send (a tainted run's send to another target).
type approvingMessenger struct{}

func (approvingMessenger) Send(context.Context, messaging.Request) (messaging.Sent, error) {
	return messaging.Sent{}, messaging.NeedsApproval("send_message to slack:C0OTHER001", "unverified author")
}

// A tainted script's core write and send elsewhere are put to a person as the same call
// (Runtime.Approve): the script gets the approval's answer; in a dry run they stay refused.
func TestTaintedLuauWritesGoToApproval(t *testing.T) {
	rt, core := newRuntime(t)
	rt.Grants = append(rt.Grants, "message:*")
	rt.Messenger = approvingMessenger{}
	type asked struct{ session, tool, args string }
	var got []asked
	rt.Approve = func(_ context.Context, session, tool string, args json.RawMessage, need *messaging.ApprovalNeeded) (any, error) {
		got = append(got, asked{session, tool, string(args)})
		return map[string]any{"status": "pending_approval", "approval_id": "ap_test"}, nil
	}
	caps, err := NewCaps([]string{"core:msg_*", "message:*"}, rt.Grants)
	if err != nil {
		t.Fatal(err)
	}
	ctx := messaging.WithUntrusted(context.Background(), "unverified author")
	call := rt.NewCall("relay", "slack-C0DEV-1.1", caps, false)
	for _, tc := range []struct{ name, args string }{
		{"core.call", `{"tool":"msg_send","args":{"to":"atlas","message":"later"}}`},
		{"message.send", `{"target":"slack:C0OTHER001","text":"hello there"}`},
	} {
		raw := call.Call(ctx, tc.name, []byte(tc.args))
		if !strings.Contains(string(raw), `"ok":true`) || !strings.Contains(string(raw), "ap_test") {
			t.Errorf("%s: %s", tc.name, raw)
		}
	}
	if len(got) != 2 || got[0].tool != "mcp_core_msg_send" || got[0].args != `{"message":"later","to":"atlas"}` ||
		got[1].tool != "send_message" || got[1].args != `{"message":"hello there","target":"slack:C0OTHER001"}` || got[0].session != "slack-C0DEV-1.1" {
		t.Fatalf("asked %+v", got)
	}
	if len(core.got()) != 0 {
		t.Fatal("the core write ran before approval", core.got())
	}
	dry := rt.NewCall("relay", "slack-C0DEV-1.1", caps, true)
	if raw := dry.Call(ctx, "core.call", []byte(`{"tool":"msg_send","args":{"to":"atlas","message":"later"}}`)); strings.Contains(string(raw), `"ok":true`) || len(got) != 2 {
		t.Errorf("a dry run asked: %s", raw)
	}
}
