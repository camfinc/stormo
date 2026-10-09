package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestReviewPageAndDecisions(t *testing.T) {
	_, inst := fixture(t)
	root := inst.Root
	// A skill proposal from the dream for atlas.
	prop := filepath.Join(root, "agents/atlas/learnings/proposals/skills/quote-check")
	write(t, filepath.Join(prop, ".proposal.json"), `{"skill":"quote-check","napHash":"abc","nap":"n1","pii":[]}`)
	write(t, filepath.Join(prop, "SKILL.md"), "---\nname: quote-check\n---\nCheck quotes twice.\n")
	restarted := []string{}
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true,
		Roster: func() []BusAgent {
			return []BusAgent{{ID: "atlas", Unit: "sales", Running: true}, {ID: "nova", Unit: "media"}}
		},
		LearnDeps: LearnDeps{Restart: func(id string) error { restarted = append(restarted, id); return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	do := func(method, path, auth string, body any) (int, map[string]any) {
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
	for _, who := range []string{"", atlasKey} {
		if code, _ := do("GET", "/api/review", who, nil); code != 401 {
			t.Errorf("review as %q: %d", who, code)
		}
		if code, _ := do("POST", "/api/agents/atlas/restart", who, nil); code != 401 {
			t.Errorf("restart as %q: %d", who, code)
		}
	}
	// The page itself carries no data or token.
	r, err := http.Get(base + "/review")
	if err != nil || r.StatusCode != 200 {
		t.Fatal(r, err)
	}
	page, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(page), "/ui/review.js") || strings.Contains(string(page), owner) || r.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Error("page")
	}

	agentOf := func(v map[string]any, id string) map[string]any {
		for _, a := range v["agents"].([]any) {
			if m := a.(map[string]any); m["id"] == id {
				return m
			}
		}
		t.Fatalf("no %s", id)
		return nil
	}
	_, v := do("GET", "/api/review", owner, nil)
	atlas := agentOf(v, "atlas")
	lessons := atlas["lessons"].([]any)
	if len(lessons) != 3 || lessons[0].(map[string]any)["status"] != "proposed" || lessons[2].(map[string]any)["status"] != "accepted" {
		t.Fatalf("lessons proposed first: %v", lessons)
	}
	skills := atlas["skills"].([]any)
	if len(skills) != 1 || skills[0].(map[string]any)["new"] != true || !strings.Contains(fmt.Sprint(skills[0]), "Check quotes twice.") {
		t.Fatalf("skills %v", skills)
	}
	if atlas["unapplied"] != false {
		t.Error("nothing decided yet")
	}

	if code, out := do("POST", "/api/review", owner, map[string]any{"agent": "atlas", "lessons": []string{"l1"}, "decision": "accept"}); code != 200 {
		t.Fatalf("accept %d %v", code, out)
	}
	if code, out := do("POST", "/api/review", owner, map[string]any{"agent": "atlas", "lessons": []string{"l2"}, "decision": "promote", "scope": "group"}); code != 200 {
		t.Fatalf("promote %d %v", code, out)
	}
	if code, out := do("POST", "/api/review", owner, map[string]any{"agent": "atlas", "lessons": []string{"l3"}, "decision": "promote", "scope": "unit"}); code != 400 || !strings.Contains(fmt.Sprint(out), "accept it before promoting") {
		t.Errorf("promoting a proposed lesson: %d %v", code, out)
	}
	if code, _ := do("POST", "/api/review", owner, map[string]any{"agent": "zed", "lessons": []string{"l1"}, "decision": "accept"}); code != 400 {
		t.Errorf("unknown agent: %d", code)
	}
	if code, out := do("POST", "/api/review", owner, map[string]any{"agent": "atlas", "skill": "quote-check", "decision": "accept"}); code != 200 {
		t.Fatalf("skill %d %v", code, out)
	}
	ledger, _ := learning.LoadLedger(root, "atlas")
	for _, e := range ledger {
		switch e.ID {
		case "l1":
			if e.Status != learning.Accepted {
				t.Errorf("l1 %s", e.Status)
			}
		case "l2":
			if e.Scope != manifest.ScopeGroup {
				t.Errorf("l2 scope %s", e.Scope)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "agents/atlas/skills/quote-check/SKILL.md")); err != nil {
		t.Errorf("accepted skill not installed: %v", err)
	}
	_, v = do("GET", "/api/review", owner, nil)
	if agentOf(v, "atlas")["unapplied"] != true || len(agentOf(v, "atlas")["skills"].([]any)) != 0 {
		t.Errorf("after decisions %v", agentOf(v, "atlas"))
	}
	if code, _ := do("POST", "/api/agents/nova/restart", owner, nil); code != 409 {
		t.Errorf("restart a stopped agent: %d", code)
	}
	if code, out := do("POST", "/api/agents/atlas/restart", owner, nil); code != 200 || strings.Join(restarted, ",") != "atlas" {
		t.Fatalf("restart %d %v %v", code, out, restarted)
	}
	_, v = do("GET", "/api/review", owner, nil)
	if agentOf(v, "atlas")["unapplied"] != false {
		t.Error("restart applies the decisions")
	}
}
