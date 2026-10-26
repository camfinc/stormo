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
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestReviewPageAndDecisions(t *testing.T) {
	_, inst := fixture(t)
	inst.CoreWebUI = true
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

	// Runtime settings: the rules hold the sensitive one; the review's verdict shows on the other
	// while its content is the one judged; accepting writes agent.yaml (which must stay valid).
	manifestPath := filepath.Join(root, "agents/atlas/agent.yaml")
	body, _ := os.ReadFile(manifestPath)
	write(t, manifestPath, strings.Replace(string(body), "secrets: [SLACK_BOT_TOKEN, SLACK_APP_TOKEN]", "secrets: [SLACK_BOT_TOKEN, SLACK_APP_TOKEN, OPENROUTER_API_KEY]", 1))
	write(t, filepath.Join(root, "agents/atlas/learnings/proposals/settings.json"), `[
 {"key":"home_channel.slack","value":"C0OPS","opaque":true,"pii":[],"nap":"n2"},
 {"key":"command_allowlist","value":"[\"git status\"]","sensitive":true,"pii":[],"nap":"n2"}]`)
	cands, _, err := loop.ReviewCandidates(inst, "atlas", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, cd := range cands {
		if cd.Kind == loop.KindSetting {
			c.Learner.keepVerdict("atlas", cd, Verdict{Approve: false, Reason: "an unfamiliar channel", Model: "test-model", Rules: "r"})
		}
	}
	_, v = do("GET", "/api/review", owner, nil)
	settings := agentOf(v, "atlas")["settings"].([]any)
	if len(settings) != 2 || v["review"] != "auto" || agentOf(v, "atlas")["autoReview"] != true {
		t.Fatalf("settings %v / review %v", settings, v["review"])
	}
	for _, x := range settings {
		m := x.(map[string]any)
		switch m["key"] {
		case "home_channel.slack":
			if ai, _ := m["ai"].(map[string]any); ai == nil || ai["approve"] != false || ai["reason"] != "an unfamiliar channel" {
				t.Errorf("verdict on the home channel: %v", m)
			}
		case "command_allowlist":
			if !strings.Contains(fmt.Sprint(m["held"]), "widens") || m["ai"] != nil {
				t.Errorf("the allowlist is held by the rules: %v", m)
			}
		}
	}
	if code, out := do("POST", "/api/review", owner, map[string]any{"agent": "atlas", "settings": []string{"home_channel.slack"}, "decision": "accept"}); code != 200 {
		t.Fatalf("accept setting %d %v", code, out)
	}
	if code, out := do("POST", "/api/review", owner, map[string]any{"agent": "atlas", "settings": []string{"command_allowlist"}, "decision": "promote"}); code != 400 {
		t.Errorf("promote a setting: %d %v", code, out)
	}
	a, err := manifest.Load(root, "atlas", inst.Names.Secret)
	if err != nil || a.Channels[0].HomeChannel != "C0OPS" {
		t.Errorf("home channel in agent.yaml: %+v %v", a, err)
	}
}

func TestReviewViewUsesConfirmationThreshold(t *testing.T) {
	_, inst := fixture(t)
	ledger := []*learning.Learning{}
	e, _ := learning.Upsert(&ledger, learning.Sighting{Kind: learning.KindMemory, Text: "Check task assertions before reporting success.", Agent: "atlas", Unit: "sales", Nap: "n", At: "2026-10-26T10:00:00Z"})
	if err := learning.SaveLedger(inst.Root, "atlas", ledger); err != nil {
		t.Fatal(err)
	}
	event := learning.OutcomeEvent{EventID: "check", TaskRoot: "task", Origin: "check", SubjectKind: "lesson", SubjectID: e.ID, SubjectRevision: learning.LessonRevision(e), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	if err := learning.MergeEvidence(inst.Root, "atlas", []learning.OutcomeEvent{event}); err != nil {
		t.Fatal(err)
	}
	learner := NewLearner(LearnerOptions{Inst: inst, DB: testDB(t), DigestDir: t.TempDir()})
	for _, threshold := range []int{2, 1} {
		inst.CoreLearning.AutoAcceptMinConfirmations = threshold
		view, err := learner.review(nil)
		if err != nil {
			t.Fatal(err)
		}
		var lesson *ReviewLesson
		for _, agent := range view.Agents {
			if agent.ID == "atlas" {
				for i := range agent.Lessons {
					if agent.Lessons[i].ID == e.ID {
						copy := agent.Lessons[i]
						lesson = &copy
					}
				}
			}
		}
		if lesson == nil {
			t.Fatal("lesson missing from review")
		}
		if lesson.AutoAcceptable != (threshold == 1) {
			t.Fatalf("threshold %d autoAcceptable=%v", threshold, lesson.AutoAcceptable)
		}
		if threshold == 2 && lesson.Held == "" {
			t.Fatal("insufficient evidence not held for review")
		}
		if threshold == 1 && lesson.Held != "" {
			t.Fatalf("supported lesson still held: %s", lesson.Held)
		}
	}
}
