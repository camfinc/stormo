package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/learning"
)

// learningAudience never widens an unknown API/job audience to agent scope.
func (a *Agent) learningAudience(session string) string {
	root, _ := a.Store.Root(session)
	if a.Env.Memory != nil {
		aud := a.Env.Memory.AudienceOf(session)
		if aud.Scope != "" {
			return aud.Scope
		}
		if aud.Operator {
			return "agent"
		}
	}
	return "conversation:" + runtimelearn.Hash(root)
}

// skillRevision hashes the entire bounded skill tree, not just its instructions.
// Symlinks and oversized trees cannot be attributed reliably, so produce no event.
func skillRevision(lib *skills.Library, s skills.Skill) string {
	root := lib.Abs(s)
	files := []string{}
	size := int64(0)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fs.ErrInvalid
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fs.ErrInvalid
		}
		size += info.Size()
		if size > 2<<20 || len(files) >= 256 {
			return fs.ErrInvalid
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return ""
	}
	sort.Strings(files)
	h := sha256.New()
	for i, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		rel, _ := filepath.Rel(root, p)
		path := "skills/" + s.Dir + "/" + filepath.ToSlash(rel)
		sum := sha256.Sum256(b)
		if i > 0 {
			h.Write([]byte("\n"))
		}
		fmt.Fprintf(h, "%s\x00%s", path, hex.EncodeToString(sum[:]))
	}

	return hex.EncodeToString(h.Sum(nil))
}
func (a *Agent) outcomeSubject(c wire.ToolCall) *learning.OutcomeEvent {
	if a.Env.Skills == nil {
		return nil
	}
	var args struct {
		Name       string `json:"name"`
		Skill      string `json:"skill"`
		Background bool   `json:"background"`
	}
	if json.Unmarshal([]byte(c.Arguments), &args) != nil {
		return nil
	}
	source, name := "runtime", args.Skill
	if c.Name == "skill_view" {
		source, name = "view", args.Name
	} else if c.Name != "terminal" || args.Background || name == "" {
		return nil
	}
	s, err := a.Env.Skills.Find(name)
	if err != nil {
		return nil
	}
	// The revision is the snapshot's skill hash (the nap's NapHash): the dream's review looks
	// skill evidence up by it, so another hash would never match. A background draft binds the
	// mode-inclusive bundle revision when it is staged (backgroundSkillContext, StagePatch).
	rev := skillRevision(a.Env.Skills, s)
	if rev == "" {
		return nil
	}
	// Opaque subject identity prevents names supplied by clients leaking into review metadata.
	return &learning.OutcomeEvent{SubjectKind: "skill", SubjectID: runtimelearn.Hash(strings.TrimSpace(s.Dir)), SubjectRevision: rev, SourceType: source, ExecutionStatus: "unknown", TaskOutcome: "unknown"}
}

// Full record views establish revision identity, never application success. Cut
// search hits cannot establish exact content revision and are deliberately omitted.
func (a *Agent) memoryOutcomeSubjects(c wire.ToolCall, out string, failed bool) []*learning.OutcomeEvent {
	if c.Name != "memory_search" || failed {
		return nil
	}
	var result memory.SearchResult
	if json.Unmarshal([]byte(out), &result) != nil || len(result.Results) > 20 {
		return nil
	}
	events := []*learning.OutcomeEvent{}
	for _, r := range result.Results {
		if r.Cut || r.Untrusted || !memory.ValidID(r.ID) {
			continue
		}
		body, _ := json.Marshal(struct{ Kind, Text, Updated, Expires, Scope string }{r.Kind, r.Text, r.Updated, r.Expires, r.Scope})
		e := &learning.OutcomeEvent{SubjectKind: "memory", SubjectID: runtimelearn.Hash(r.ID), SubjectRevision: runtimelearn.Hash(string(body)), SourceType: "view", ExecutionStatus: "unknown", TaskOutcome: "unknown"}
		if r.Scope == memory.ScopeAgent && r.Kind != memory.KindUser && len(r.PII) == 0 {
			e.Audience = "agent"
			if r.Hot > 0 {
				lesson := &learning.Learning{Kind: learning.KindMemory, Text: r.Text}
				e.SubjectKind = "lesson"
				e.SubjectID = learning.LearningID(lesson.Kind, lesson.Text)
				e.SubjectRevision = learning.LessonRevision(lesson)
			}
		}
		events = append(events, e)
	}
	return events
}
