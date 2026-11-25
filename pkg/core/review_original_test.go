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

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/loop"
)

// flaggedSkill puts a skill the agent wrote into a nap in the local store, and its scrubbed
// proposal where the dream leaves one.
func flaggedSkill(t *testing.T, inst *instance.Instance, agent, skill, napID, original string) string {
	t.Helper()
	root := inst.Root
	store := loop.LocalStore{Root: root}
	path := "skills/" + skill + "/SKILL.md"
	sha := build.Sha256([]byte(original))
	if err := store.Put(loop.BlobKey(agent, engine.Learning, sha), []byte(original)); err != nil {
		t.Fatal(err)
	}
	nap, _ := json.Marshal(loop.Nap{ID: napID, Agent: agent, Files: []loop.NapFile{{Path: path, Class: engine.Learning, Sha256: sha, Size: len(original)}}})
	if err := store.Put(loop.NapKey(agent, napID), nap); err != nil {
		t.Fatal(err)
	}
	scrubbed, pii := learning.NewScrubber(inst).Scrub(original)
	hash := build.SkillHash(map[string]string{path: sha}, []string{path})
	meta, _ := json.Marshal(loop.SkillProposal{Skill: skill, NapHash: hash, Nap: napID, PII: pii})
	dir := filepath.Join(loop.ProposalsDir(root, agent), skill)
	write(t, filepath.Join(dir, ".proposal.json"), string(meta))
	write(t, filepath.Join(dir, "SKILL.md"), scrubbed)
	return hash
}

func TestReviewAcceptsFlaggedSkillOriginal(t *testing.T) {
	_, inst := fixture(t)
	root := inst.Root
	original := "---\nname: quote-mail\n---\nSend quotes to quotes@example.com before noon.\n"
	hash := flaggedSkill(t, inst, "atlas", "quote-mail", "20261006T100000000Z-a", original)
	flaggedSkill(t, inst, "atlas", "token-note", "20261006T100000000Z-b", "Call the CRM with ghp_testFAKEtoken0000, or mail ops@example.com.\n")
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true,
		Roster: func() []BusAgent { return []BusAgent{{ID: "atlas", Unit: "sales"}} }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const owner = "owner-token-0123456789"
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
	installed := filepath.Join(root, "agents/atlas/skills/quote-mail/SKILL.md")
	agentOf := func(v map[string]any, id string) map[string]any {
		for _, a := range v["agents"].([]any) {
			if m := a.(map[string]any); m["id"] == id {
				return m
			}
		}
		t.Fatalf("no %s", id)
		return nil
	}

	// The review shows the scrubbed copy and says whether a person may take the original.
	_, v := do("GET", "/api/review", owner, nil)
	skills := map[string]map[string]any{}
	for _, s := range agentOf(v, "atlas")["skills"].([]any) {
		m := s.(map[string]any)
		skills[m["skill"].(string)] = m
	}
	mail, note := skills["quote-mail"], skills["token-note"]
	if mail == nil || mail["originalAccept"] != true || strings.Contains(fmt.Sprint(v), "quotes@example.com") {
		t.Fatalf("quote-mail %v (the review must not carry the original)", mail)
	}
	if note == nil || note["originalAccept"] != false || !strings.Contains(fmt.Sprint(note["originalRefused"]), "secret") {
		t.Fatalf("token-note %v", note)
	}

	// A plain accept would write the placeholder: refused.
	if code, out := do("POST", "/api/review", owner, map[string]any{"agent": "atlas", "skill": "quote-mail", "decision": "accept"}); code != 400 || !strings.Contains(fmt.Sprint(out), "original") {
		t.Errorf("plain accept of a flagged skill: %d %v", code, out)
	}
	if _, err := os.Stat(installed); err == nil {
		t.Fatal("the scrubbed copy was installed")
	}

	// The original, for the owner only.
	for _, who := range []string{"", atlasKey} {
		if code, _ := do("GET", "/api/review/original?agent=atlas&skill=quote-mail", who, nil); code != 401 {
			t.Errorf("original as %q: %d", who, code)
		}
	}
	if code, _ := do("GET", "/api/review/original?agent=atlas&skill=nope", owner, nil); code != 404 {
		t.Errorf("unknown skill: %d", code)
	}
	if code, out := do("GET", "/api/review/original?agent=atlas&skill=token-note", owner, nil); code != 409 || !strings.Contains(fmt.Sprint(out), "secret") {
		t.Errorf("a secret's original: %d %v", code, out)
	}
	code, out := do("GET", "/api/review/original?agent=atlas&skill=quote-mail", owner, nil)
	if code != 200 || out["napHash"] != hash || !strings.Contains(fmt.Sprint(out["files"]), "quotes@example.com") {
		t.Fatalf("original %d %v", code, out)
	}

	// Accepting it takes the version that was shown, and only that one.
	accept := map[string]any{"agent": "atlas", "skill": "quote-mail", "decision": "accept", "original": true}
	if code, _ := do("POST", "/api/review", owner, accept); code != 400 {
		t.Errorf("original without the napHash shown: %d", code)
	}
	accept["napHash"] = "another-version"
	if code, _ := do("POST", "/api/review", owner, accept); code != 400 {
		t.Errorf("original of a version not shown: %d", code)
	}
	if code, _ := do("POST", "/api/review", owner, map[string]any{"agent": "atlas", "skill": "quote-mail", "decision": "reject", "original": true, "napHash": hash}); code != 400 {
		t.Errorf("original with reject: %d", code)
	}
	accept["napHash"] = hash
	if code, out := do("POST", "/api/review", owner, accept); code != 200 {
		t.Fatalf("accept original %d %v", code, out)
	}
	if b, err := os.ReadFile(installed); err != nil || string(b) != original {
		t.Errorf("installed %q, %v; want the agent's original", b, err)
	}
	entries, _ := c.DB.AuditLog(20)
	actions := []string{}
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	if !strings.Contains(strings.Join(actions, " "), "review.original") || !strings.Contains(strings.Join(actions, " "), "review.accept-original") {
		t.Errorf("audit %v", actions)
	}
}
