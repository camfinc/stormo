package loop

import (
	"context"
	"encoding/json"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/learning"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/pkg/build"
)

func TestSkillRevisionMatchesSnapshotAndChanges(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "skills", "fixture")
	_ = os.MkdirAll(filepath.Join(dir, "scripts"), 0700)
	files := map[string]string{"SKILL.md": "---\nname: fixture\ndescription: fixture\n---\nProcedure", "scripts/check.sh": "exit 0\n"}
	hashes := map[string]string{}
	paths := []string{}
	for p, body := range files {
		_ = os.WriteFile(filepath.Join(dir, p), []byte(body), 0600)
		key := "skills/fixture/" + p
		paths = append(paths, key)
		hashes[key] = build.Sha256([]byte(body))
	}
	lib := &skills.Library{Home: home}
	s, err := lib.Find("fixture")
	if err != nil {
		t.Fatal(err)
	}
	rev := skillRevision(lib, s)
	if want := build.SkillHash(hashes, paths); rev != want {
		t.Fatalf("%s != %s", rev, want)
	}
	_ = os.WriteFile(filepath.Join(dir, "scripts/check.sh"), []byte("exit 1\n"), 0600)
	if skillRevision(lib, s) == rev {
		t.Fatal("script revision unchanged")
	}
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "secret"))
	if skillRevision(lib, s) != "" {
		t.Fatal("symlink attributed")
	}
}

func TestMemoryViewRevisionAudienceAndCut(t *testing.T) {
	a := &Agent{}
	r := memory.View{ID: "m0123456789a", Kind: memory.KindProcedure, Text: "Check the result.", Scope: memory.ScopeAgent, Hot: 1}
	// Use the public ID validator's own generated-record shape.

	b, _ := json.Marshal(memory.SearchResult{Results: []memory.View{r}})
	got := a.memoryOutcomeSubjects(wire.ToolCall{Name: "memory_search"}, string(b), false)
	if len(got) == 0 {
		t.Fatal("fixture record id shape invalid")
	}
	want := &learning.Learning{Kind: learning.KindMemory, Text: r.Text}
	if got[0].SubjectKind != "lesson" || got[0].SubjectRevision != learning.LessonRevision(want) || got[0].SubjectID != learning.LearningID(want.Kind, want.Text) {
		t.Fatal(got[0])
	}
	r.Cut = true
	b, _ = json.Marshal(memory.SearchResult{Results: []memory.View{r}})
	if len(a.memoryOutcomeSubjects(wire.ToolCall{Name: "memory_search"}, string(b), false)) != 0 {
		t.Fatal("cut view attributed")
	}
}

func TestRepeatedProviderCallIDAcrossTurns(t *testing.T) {
	st := &stub{pick: func(req map[string]any) string {
		if lastRow(req)["role"] == "user" {
			return callTool("reused_call", "skill_view", `{"name":"crm-api"}`)
		}
		return say("Done.")
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	a.Env.Skills = &skills.Library{Home: a.Cfg.Home}
	a.Learning = &runtimelearn.Store{Home: a.Cfg.Home}
	_, err := a.Store.Create(wire.Session{ID: "repeated"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := a.Run(context.Background(), "repeated", "Load the procedure", Options{}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := a.Learning.Events(runtimelearn.Hash("repeated"), a.learningAudience("repeated"))
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if events[0].EventID == events[1].EventID || events[0].TaskRoot != events[1].TaskRoot {
		t.Fatal(events)
	}
	for _, e := range events {
		if e.SourceType != "view" || e.TaskOutcome != "unknown" {
			t.Fatal(e)
		}
	}
}
