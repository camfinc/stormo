package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FakeWeb is a Slack Web API with two pages of members and one of conversations; missing names
// methods that answer missing_scope.
func FakeWeb(t *testing.T, token string, missing ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		if r.Header.Get("Authorization") != "Bearer "+token {
			reply(map[string]any{"ok": false, "error": "invalid_auth"})
			return
		}
		method := r.URL.Path[1:]
		for _, m := range missing {
			if m == method {
				reply(map[string]any{"ok": false, "error": "missing_scope", "needed": "users:read"})
				return
			}
		}
		switch method {
		case "auth.test":
			reply(map[string]any{"ok": true, "team": "Acme", "team_id": "T1", "user": "atlas", "user_id": "UBOT"})
		case "users.list":
			if r.Form.Get("cursor") == "" {
				reply(map[string]any{"ok": true, "members": []any{
					map[string]any{"id": "U2", "name": "zed", "profile": map[string]any{"real_name": "Zed Zulu", "display_name": ""}},
					map[string]any{"id": "UGONE", "name": "gone", "deleted": true},
					map[string]any{"id": "USLACKBOT", "name": "slackbot", "is_bot": true},
				}, "response_metadata": map[string]any{"next_cursor": "p2"}})
				return
			}
			reply(map[string]any{"ok": true, "members": []any{
				map[string]any{"id": "U1", "name": "ana", "is_admin": true, "tz": "Europe/Lisbon", "profile": map[string]any{"real_name": "Ana Alves", "display_name": "Ana"}},
				map[string]any{"id": "U3", "name": "gus", "is_restricted": true, "profile": map[string]any{"real_name": "Gus Guest"}},
				map[string]any{"id": "UB2", "name": "nova", "is_bot": true, "profile": map[string]any{"real_name": "Nova"}},
			}, "response_metadata": map[string]any{"next_cursor": ""}})
		case "conversations.list":
			reply(map[string]any{"ok": true, "channels": []any{
				map[string]any{"id": "C2", "name": "sales", "is_member": true},
				map[string]any{"id": "G1", "name": "deals", "is_private": true, "is_member": true},
				map[string]any{"id": "C1", "name": "general", "is_member": false},
				map[string]any{"id": "D1", "is_im": true, "user": "U1"},
			}})
		case "conversations.open":
			reply(map[string]any{"ok": true, "channel": map[string]any{"id": "D-" + r.Form.Get("users")}})
		default:
			reply(map[string]any{"ok": false, "error": "unknown_method"})
		}
	}))
	t.Cleanup(srv.Close)
	saved := APIBase
	APIBase = srv.URL + "/"
	t.Cleanup(func() { APIBase = saved })
	return srv
}

func TestFetchDirectory(t *testing.T) {
	FakeWeb(t, "xoxb-ok")
	d, err := FetchDirectory(context.Background(), nil, "xoxb-ok")
	if err != nil {
		t.Fatal(err)
	}
	if d.Team != (Team{"T1", "Acme"}) || d.Bot.ID != "UBOT" {
		t.Errorf("team/bot: %+v %+v", d.Team, d.Bot)
	}
	// Both pages, deleted members and Slackbot dropped; people first by name, then bots.
	var ids []string
	for _, p := range d.People {
		ids = append(ids, p.ID)
	}
	if want := []string{"U1", "U3", "U2", "UB2"}; len(ids) != len(want) || ids[0] != "U1" || ids[1] != "U3" || ids[2] != "U2" || ids[3] != "UB2" {
		t.Errorf("people = %v, want %v", ids, want)
	}
	ana, gus, zed, nova := d.People[0], d.People[1], d.People[2], d.People[3]
	if ana.Name != "Ana" || ana.RealName != "Ana Alves" || !ana.Owner || ana.TZ != "Europe/Lisbon" || ana.Bot || ana.Guest {
		t.Errorf("ana = %+v", ana)
	}
	if !gus.Guest || zed.Name != "Zed Zulu" || !nova.Bot {
		t.Errorf("gus %+v zed %+v nova %+v", gus, zed, nova)
	}
	if len(d.Channels) != 3 || d.Channels[0].Name != "deals" || !d.Channels[0].Private || d.Channels[1].Member {
		t.Errorf("channels = %+v", d.Channels)
	}
	if len(d.DMs) != 1 || d.DMs[0] != (DM{"D1", "U1"}) || len(d.Partial) != 0 || d.Fetched.IsZero() {
		t.Errorf("dms %+v partial %v", d.DMs, d.Partial)
	}
}

func TestFetchDirectoryMissingScopeAndBadToken(t *testing.T) {
	FakeWeb(t, "xoxb-ok", "users.list")
	d, err := FetchDirectory(context.Background(), nil, "xoxb-ok")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.People) != 0 || len(d.Channels) != 3 || len(d.Partial) != 1 {
		t.Errorf("a missing scope leaves the rest: people %d channels %d partial %v", len(d.People), len(d.Channels), d.Partial)
	}
	_, err = FetchDirectory(context.Background(), nil, "pbpaste | not a token")
	if se, ok := err.(*Error); !ok || se.Code != "invalid_auth" {
		t.Errorf("bad token: %v", err)
	}
	id, err := OpenDM(context.Background(), nil, "xoxb-ok", "U3")
	if err != nil || id != "D-U3" {
		t.Errorf("OpenDM = %q, %v", id, err)
	}
}
