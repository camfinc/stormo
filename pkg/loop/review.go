package loop

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

// Human review of dream output. Everything here edits files in the working tree; committing them
// is the reviewer's call.

// Change is one review decision applied to learnings.
type Change struct {
	Status *learning.Status
	Scope  *manifest.Scope
	Pinned *bool
	Text   *string
	Note   string
}

// Decide applies a change to learnings by id (or unique id prefix).
func Decide(root, agent string, ids []string, c Change) ([]*learning.Learning, error) {
	ledger, err := learning.LoadLedger(root, agent)
	if err != nil {
		return nil, err
	}
	touched := []*learning.Learning{}
	for _, id := range ids {
		var e *learning.Learning
		for _, x := range ledger {
			if x.ID == id || strings.HasPrefix(x.ID, id) {
				e = x
				break
			}
		}
		if e == nil {
			return nil, fmt.Errorf("%s: no learning %s", agent, id)
		}
		if c.Text != nil {
			// A reviewer rewrite (e.g. dropping a redaction marker) clears the PII flag deliberately.
			e.Text = *c.Text
			e.PII = []string{}
		}
		if c.Status != nil {
			e.Status = *c.Status
			now := IsoMillis(time.Now())
			e.DecidedAt = &now
		}
		if c.Pinned != nil {
			p := *c.Pinned
			e.Pinned = &p
		}
		if c.Note != "" {
			n := c.Note
			e.Note = &n
		}
		if c.Scope != nil {
			if err := learning.Promote(e, *c.Scope); err != nil {
				return nil, err
			}
		}
		touched = append(touched, e)
	}
	return touched, learning.SaveLedger(root, agent, ledger)
}

// SkillProposal is learnings/proposals/skills/<skill>/.proposal.json.
type SkillProposal struct {
	Skill   string   `json:"skill"`
	NapHash string   `json:"napHash"`
	Nap     string   `json:"nap"`
	PII     []string `json:"pii"`
}

func ProposalsDir(root, agent string) string {
	return filepath.Join(manifest.AgentDir(root, agent), "learnings", "proposals", "skills")
}

func ListSkillProposals(root, agent string) ([]SkillProposal, error) {
	dir := ProposalsDir(root, agent)
	out := []SkillProposal{}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return out, nil
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != ".proposal.json" {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var sp SkillProposal
		if err := json.Unmarshal(body, &sp); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, sp)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Skill < out[j].Skill })
	return out, err
}

// DecideSkill accepts (copies into skills/) or rejects a skill proposal and records the decision.
func DecideSkill(root, agent, skill, decision string) (*SkillProposal, error) {
	props, err := ListSkillProposals(root, agent)
	if err != nil {
		return nil, err
	}
	var prop *SkillProposal
	for i := range props {
		if props[i].Skill == skill {
			prop = &props[i]
		}
	}
	if prop == nil {
		return nil, fmt.Errorf("%s: no skill proposal %s", agent, skill)
	}
	src := filepath.Join(ProposalsDir(root, agent), skill)
	if decision == "accepted" {
		dest := filepath.Join(manifest.AgentDir(root, agent), "skills", skill)
		if err := os.RemoveAll(dest); err != nil {
			return nil, err
		}
		if err := copyTree(src, dest, func(p string) bool { return !strings.HasSuffix(p, ".proposal.json") }); err != nil {
			return nil, err
		}
	}
	if err := os.RemoveAll(src); err != nil {
		return nil, err
	}
	pruneEmpty(ProposalsDir(root, agent))
	wm, err := LoadWatermark(root, agent)
	if err != nil {
		return nil, err
	}
	wm.SkillDecisions[skill] = SkillDecision{Hash: prop.NapHash, Decision: decision, At: IsoMillis(time.Now())}
	return prop, SaveWatermark(root, agent, wm)
}

func copyTree(src, dst string, keep func(string) bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !keep(p) {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func pruneEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			pruneEmpty(filepath.Join(dir, e.Name()))
		}
	}
	if entries, _ = os.ReadDir(dir); len(entries) == 0 {
		os.RemoveAll(dir)
	}
}

// AutoAcceptable: a proposed lesson that needs no judgement call. Agent-scoped memory only (a
// user-profile entry is about a person), with no PII flag, seen at least minSeen times.
func AutoAcceptable(e *learning.Learning, minSeen int) bool {
	return e.Status == learning.Proposed && e.Scope == manifest.ScopeAgent && e.Kind == learning.KindMemory &&
		len(e.PII) == 0 && e.SeenCount >= minSeen
}

// AutoAccept accepts agent's auto-acceptable lessons and returns their ids. It writes the ledger
// in the working tree only, like every review decision.
func AutoAccept(root, agent string, minSeen int) ([]string, error) {
	ledger, err := learning.LoadLedger(root, agent)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, e := range ledger {
		if AutoAcceptable(e, minSeen) {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return ids, nil
	}
	accepted := learning.Accepted
	_, err = Decide(root, agent, ids, Change{Status: &accepted, Note: "accepted automatically (core.learning.auto_accept)"})
	return ids, err
}
