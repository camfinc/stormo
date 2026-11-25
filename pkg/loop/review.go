package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
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
	unlock, err := learning.LockLedger(root, agent, 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
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

// DecideLesson accepts or rejects the lesson whose id is exactly id, after check passes on it as the
// ledger holds it now, in the same load and save as the write: a caller that allows only some
// lessons (a linked device) decides exactly the one it checked, never a look-alike id or a version
// changed since.
func DecideLesson(root, agent, id string, status learning.Status, check func(*learning.Learning) error) (*learning.Learning, error) {
	unlock, err := learning.LockLedger(root, agent, 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ledger, err := learning.LoadLedger(root, agent)
	if err != nil {
		return nil, err
	}
	for _, e := range ledger {
		if e.ID != id {
			continue
		}
		if err := check(e); err != nil {
			return nil, err
		}
		e.Status = status
		now := IsoMillis(time.Now())
		e.DecidedAt = &now
		return e, learning.SaveLedger(root, agent, ledger)
	}
	return nil, fmt.Errorf("%s: %w %s", agent, ErrNoLesson, id)
}

// ErrNoLesson is DecideLesson finding no lesson with that exact id.
var ErrNoLesson = errors.New("no learning")

// SkillProposal is learnings/proposals/skills/<skill>/.proposal.json.
type SkillProposal struct {
	Skill   string   `json:"skill"`
	NapHash string   `json:"napHash"`
	Nap     string   `json:"nap"`
	PII     []string `json:"pii"`
	// Stale: the agent changed the version it booted with (commit Base), and the repo's copy has
	// changed since. Accepting would overwrite that change, so it is refused; merge by hand.
	Stale bool   `json:"stale,omitempty"`
	Base  string `json:"base,omitempty"`
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
// A proposal the scrubber flagged is its scrubbed copy, so accepting it is refused: it would write
// the placeholders into the skill. A person accepts the agent's own text with AcceptSkillOriginal.
func DecideSkill(root, agent, skill, decision string) (*SkillProposal, error) {
	return DecideSkillBy(root, agent, skill, decision, Decider{})
}

// DecideSkillBy is DecideSkill recording who decided (the core's review) and why.
func DecideSkillBy(root, agent, skill, decision string, who Decider) (*SkillProposal, error) {
	return decideSkill(root, agent, skill, decision, who, nil)
}

// AcceptSkillOriginal is a person's final word on a proposal the scrubber flagged: it writes the
// agent's own files (SkillOriginals) instead of the scrubbed copy, and records the decision. napHash,
// when set, is the version the person was shown; a proposal that changed since is refused. Nothing is
// written unless every file was read from the nap and matches the proposal's hash.
func AcceptSkillOriginal(inst *instance.Instance, s Store, agent, skill, napHash string) (*SkillProposal, error) {
	return decideSkill(inst.Root, agent, skill, "accepted", Decider{Reason: "a person accepted the agent's original text"},
		func(p SkillProposal) ([]SkillOriginal, error) {
			if napHash != "" && napHash != p.NapHash {
				return nil, fmt.Errorf("%s: %s changed since it was shown; reload the review", agent, skill)
			}
			return SkillOriginals(inst, s, agent, p)
		})
}

// SkillOriginal is a file of a skill proposal as the agent wrote it, before the scrub.
type SkillOriginal struct {
	// Path inside the skill's folder, as the proposal names it.
	Path string
	Body []byte
}

// OriginalRefusal is why a person may not accept a proposal's original text, "" when they may: a
// secret never goes into a skill, and a stale proposal is merged by hand.
func OriginalRefusal(p SkillProposal) string {
	switch {
	case p.Stale:
		return "the agent changed an older version than the repo's: merge what you want by hand, then reject it"
	case slices.Contains(p.PII, "secret"):
		return "the scrubber found a secret, and a skill never holds one: copy what you want by hand without it, then reject it"
	}
	return ""
}

// SkillOriginals reads a proposal's files as the agent wrote them, from the nap it came from (the
// store its dream read), checked against the proposal's hash.
func SkillOriginals(inst *instance.Instance, s Store, agentID string, p SkillProposal) ([]SkillOriginal, error) {
	if why := OriginalRefusal(p); why != "" {
		return nil, fmt.Errorf("%s: %s: %s", agentID, p.Skill, why)
	}
	agent, err := manifest.Load(inst.Root, agentID, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	eng, err := engineFor(agent.Engine.Kind, inst)
	if err != nil {
		return nil, err
	}
	L := eng.Layout()
	var nap Nap
	if ok, err := ReadJSON(s, NapKey(agentID, p.Nap), &nap); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%s: nap %s, which %s came from, is no longer in the store; the agent's next nap proposes it again (rejecting it would keep that version from coming back)", agentID, p.Nap, p.Skill)
	}
	hashes := map[string]string{}
	learningPaths := []string{}
	for _, f := range nap.Files {
		hashes[f.Path] = f.Sha256
		if f.Class == engine.Learning {
			learningPaths = append(learningPaths, f.Path)
		}
	}
	paths := build.SkillDirs(learningPaths, L.SkillsDir)[p.Skill]
	if len(paths) == 0 || build.SkillHash(hashes, paths) != p.NapHash {
		return nil, fmt.Errorf("%s: nap %s does not hold the proposed version of %s; the agent's next nap proposes it again", agentID, p.Nap, p.Skill)
	}
	sort.Strings(paths)
	prefix := L.SkillsDir + "/" + p.Skill + "/"
	out := make([]SkillOriginal, 0, len(paths))
	for _, path := range paths {
		body, err := learningBlob(s, agentID, &nap, path)
		if err != nil {
			return nil, err
		}
		if body == nil || build.Sha256([]byte(*body)) != hashes[path] {
			return nil, fmt.Errorf("%s: the store does not hold %s as nap %s recorded it; the agent's next nap proposes it again", agentID, path, p.Nap)
		}
		out = append(out, SkillOriginal{Path: strings.TrimPrefix(path, prefix), Body: []byte(*body)})
	}
	return out, nil
}

// FindSkillProposal is the agent's open proposal for skill.
func FindSkillProposal(root, agent, skill string) (*SkillProposal, error) {
	props, err := ListSkillProposals(root, agent)
	if err != nil {
		return nil, err
	}
	for i := range props {
		if props[i].Skill == skill {
			return &props[i], nil
		}
	}
	return nil, fmt.Errorf("%s: no skill proposal %s", agent, skill)
}

// decideSkill: originals, when set, is a person accepting a flagged proposal's original text.
func decideSkill(root, agent, skill, decision string, who Decider, originals func(SkillProposal) ([]SkillOriginal, error)) (*SkillProposal, error) {
	unlock, err := learning.LockLedger(root, agent, 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	prop, err := FindSkillProposal(root, agent, skill)
	if err != nil {
		return nil, err
	}
	src := filepath.Join(ProposalsDir(root, agent), skill)
	if decision == "accepted" && prop.Stale {
		return nil, fmt.Errorf("%s: %s was changed by the agent from an older version (%s); the repo's copy has changed since, so accepting would undo that. Merge it by hand from %s, then reject the proposal", agent, skill, or(prop.Base, "an earlier build"), src)
	}
	if decision == "accepted" && len(prop.PII) > 0 && originals == nil {
		return nil, fmt.Errorf("%s: the scrubber flagged %s in %s, so the proposal holds placeholders where the agent's text was and accepting it would write them into the skill. Accept the agent's original text instead (Accept in the app, or `stormo learn skill-accept --original`), or reject it", agent, strings.Join(prop.PII, ", "), skill)
	}
	if decision == "accepted" {
		// Every original is read and checked before the repo's copy is touched.
		var files []SkillOriginal
		if originals != nil && len(prop.PII) > 0 {
			if files, err = originals(*prop); err != nil {
				return nil, err
			}
		}
		dest := filepath.Join(manifest.AgentDir(root, agent), "skills", skill)
		targets := make([]string, len(files))
		for i, f := range files {
			targets[i] = filepath.Join(dest, filepath.FromSlash(f.Path))
			if !strings.HasPrefix(targets[i], dest+string(filepath.Separator)) {
				return nil, fmt.Errorf("%s: %s: a file outside the skill (%s)", agent, skill, f.Path)
			}
		}
		if err := os.RemoveAll(dest); err != nil {
			return nil, err
		}
		if files != nil {
			for i, f := range files {
				if err := os.MkdirAll(filepath.Dir(targets[i]), 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(targets[i], f.Body, 0o644); err != nil {
					return nil, err
				}
			}
		} else if err := copyTree(src, dest, func(p string) bool { return !strings.HasSuffix(p, ".proposal.json") }); err != nil {
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
	wm.SkillDecisions[skill] = SkillDecision{Hash: prop.NapHash, Decision: decision, At: IsoMillis(time.Now()), By: who.By, Reason: who.Reason}
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

// AutoAcceptable: a proposed lesson the review's rules let its model judge. Agent-scoped memory
// only (a user-profile entry is about a person), with no PII flag, seen at least minSeen times
// (ReviewCandidates applies the same rules). Outcome evidence is the model's input, not a rule.
func AutoAcceptable(e *learning.Learning, minSeen int) bool {
	return e.Status == learning.Proposed && e.Scope == manifest.ScopeAgent && e.Kind == learning.KindMemory &&
		len(e.PII) == 0 && e.SeenCount >= minSeen
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
