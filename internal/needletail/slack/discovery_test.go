package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/sched"
)

func TestChannelDiscoveryPaginationDoesNotGrantSend(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/conversations.list" {
			t.Errorf("unexpected mutation/API: %s", r.URL.Path)
		}
		r.ParseForm()
		if r.Form.Get("types") != "public_channel" || r.Form.Get("exclude_archived") != "true" || r.Form.Get("limit") != "100" {
			t.Errorf("wrong params %v", r.Form)
		}
		if r.Form.Get("cursor") == "" {
			fmt.Fprint(w, `{"ok":true,"channels":[{"id":"C0000000099","name":"unrelated","topic":{"value":"DO NOT RETURN"}}],"response_metadata":{"next_cursor":"page-2"}}`)
		} else {
			if r.Form.Get("cursor") != "page-2" {
				t.Error("cursor lost")
			}
			fmt.Fprint(w, `{"ok":true,"channels":[{"id":"C0000000098","name":"acme-control","is_member":true},{"id":"C0000000097","name":"acme-control-old","is_archived":true}],"response_metadata":{"next_cursor":""}}`)
		}
	}))
	defer srv.Close()
	g := discoveryGateway(t, srv.URL)
	tool := g.SendTool()
	first, e := tool.Run(context.Background(), nil, json.RawMessage(`{"action":"channels","query":"#CONTROL"}`))
	if e != nil {
		t.Fatal(e)
	}
	page := first.(map[string]any)
	if len(page["channels"].([]discoveredChannel)) != 0 || page["has_more"] != true || page["next_cursor"] != "page-2" {
		t.Fatal(page)
	}
	second, e := tool.Run(context.Background(), nil, json.RawMessage(`{"action":"channels","query":"control","cursor":"page-2"}`))
	if e != nil {
		t.Fatal(e)
	}
	got := second.(map[string]any)["channels"].([]discoveredChannel)
	if len(got) != 1 || got[0].Name != "acme-control" || got[0].PolicyAllowed || got[0].Member == nil || !*got[0].Member {
		t.Fatal(got)
	}
	if g.CheckTarget(sched.Target{Platform: "slack", Channel: got[0].ID}) == nil {
		t.Fatal("discovery granted send")
	}
	if calls != 2 {
		t.Fatal("unexpected API/send")
	}
}

func TestChannelInfoSeparatesAccessPolicyAndDelivery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.URL.Path != "/conversations.info" || r.Form.Get("channel") != home {
			t.Errorf("unexpected call %s %v", r.URL.Path, r.Form)
		}
		fmt.Fprintf(w, `{"ok":true,"channel":{"id":%q,"name":"home","is_archived":true,"is_member":true,"purpose":{"value":"PRIVATE"}}}`, home)
	}))
	defer srv.Close()
	g := discoveryGateway(t, srv.URL)
	got, e := g.SendTool().Run(context.Background(), nil, json.RawMessage(`{"action":"channel_info","target":"slack"}`))
	if e != nil {
		t.Fatal(e)
	}
	out := got.(map[string]any)
	c := out["channel"].(discoveredChannel)
	if !c.Archived || !c.PolicyAllowed || out["delivery_verified"] != false || out["accessible"] != true {
		t.Fatal(out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("purpose leaked")
	}
}

func TestDiscoveryErrorsNeverBecomeEmptySuccess(t *testing.T) {
	for _, code := range []string{"missing_scope", "channel_not_found", "ratelimited"} {
		t.Run(code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"ok":false,"error":%q}`, code) }))
			defer srv.Close()
			g := discoveryGateway(t, srv.URL)
			for _, args := range []string{`{"action":"channels","channel_type":"private_channel"}`, `{"action":"channel_info","target":"slack:C0000000099"}`} {
				got, e := g.SendTool().Run(context.Background(), nil, json.RawMessage(args))
				if e == nil || got != nil || !strings.Contains(e.Error(), code) {
					t.Fatalf("%v %v", got, e)
				}
				if code == "channel_not_found" && !strings.Contains(e.Error(), "does not prove") {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestDiscoveryRejectsInvalidInputsWithoutNetwork(t *testing.T) {
	g := discoveryGateway(t, "http://127.0.0.1:1")
	for _, args := range []string{`{"action":"channels","channel_type":"im"}`, `{"action":"channel_info","target":"#control"}`, `{"action":"channel_info","target":"slack:U0000000001"}`, `{"action":"channel_info","target":"slack:C0000000099:123"}`} {
		if _, e := g.SendTool().Run(context.Background(), nil, json.RawMessage(args)); e == nil {
			t.Fatal("accepted", args)
		}
	}
}

func discoveryGateway(t *testing.T, base string) *Gateway {
	t.Helper()
	g, err := New(Options{Web: &Web{BaseURL: base, BotToken: "test-token", AppToken: "test-app"}, Turns: newTurns(), HomeChannel: home, Channels: func() []string { return []string{dev} }, Exists: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestChannelMetadataIdentityAndPrivateScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("wrong credential")
		}
		switch r.URL.Path {
		case "/conversations.list":
			if r.Form.Get("types") != "private_channel" {
				t.Error("private type lost")
			}
			fmt.Fprint(w, `{"ok":true,"channels":[{"id":"G0000000099","name":"private-control","is_private":true,"is_member":true}],"response_metadata":{}}`)
		case "/conversations.info":
			fmt.Fprint(w, `{"ok":true,"channel":{"id":"C0000000001"}}`)
		default:
			t.Error("unexpected API", r.URL.Path)
		}
	}))
	defer srv.Close()
	g := discoveryGateway(t, srv.URL)
	out, e := g.discover(context.Background(), "control", "", "private_channel")
	if e != nil {
		t.Fatal(e)
	}
	c := out.(map[string]any)["channels"].([]discoveredChannel)
	if len(c) != 1 || !c[0].Private || c[0].PolicyAllowed {
		t.Fatal(c)
	}
	if _, e := g.inspectChannel(context.Background(), "C0000000099"); e == nil {
		t.Fatal("mismatched identity accepted")
	}
}
