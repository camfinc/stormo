package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

// The rules of the core's automatic review (docs/core.md §6), in code so no prompt can widen them.
// They sort every open proposal into candidates (which the review's model may approve, or pass
// to a person) and held proposals (which always wait for a person). Promotions to a unit or the
// group are never proposals: a person makes them.

// Proposal kinds.
const (
	KindLesson    = "lesson"
	KindSkill     = "skill"
	KindSchedules = "schedules"
	KindSetting   = "setting"
)

// Candidate is a proposal the rules let the review's model judge.
type Candidate struct {
	Kind string `json:"kind"`
	// Ref names it: a lesson id, a skill dir, "schedules", a setting key.
	Ref string `json:"ref"`
	// Hash is its content's: a verdict holds for this exact content only.
	Hash  string `json:"hash"`
	Title string `json:"title"`
	// Body is what the model reads: scrubbed text, the current version beside the proposed one.
	Body string `json:"body"`
}

// Held is a proposal the rules keep for a person, and why.
type Held struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Why  string `json:"why"`
}

// maxSkillReview is the most skill text the review reads; a bigger skill waits for a person.
const maxSkillReview = 48 << 10

// skillTextExt are the files a skill may hold and still be judged automatically: text the agent
// reads. Anything else (a script, a binary) runs or hides something and waits for a person.
var skillTextExt = []string{".md", ".markdown", ".txt"}

// ReviewCandidates sorts agent's open proposals by the rules.
func ReviewCandidates(inst *instance.Instance, agentID string, minSeen int) ([]Candidate, []Held, error) {
	root := inst.Root
	cands, held := []Candidate{}, []Held{}

	ledger, err := learning.LoadLedger(root, agentID)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ledger {
		if e.Status != learning.Proposed {
			continue
		}
		switch {
		case e.Kind == learning.KindUser:
			held = append(held, Held{KindLesson, e.ID, "about a person (user profile)"})
		case len(e.PII) > 0:
			held = append(held, Held{KindLesson, e.ID, "the scrubber flagged " + strings.Join(e.PII, ", ")})
		case e.Scope != manifest.ScopeAgent:
			held = append(held, Held{KindLesson, e.ID, "shared beyond the agent (" + string(e.Scope) + ")"})
		case e.SeenCount < minSeen:
			held = append(held, Held{KindLesson, e.ID, fmt.Sprintf("seen %d of %d times", e.SeenCount, minSeen)})
		default:
			cands = append(cands, Candidate{Kind: KindLesson, Ref: e.ID, Hash: e.ID, Title: "lesson " + e.ID,
				Body: fmt.Sprintf("A lesson the agent wrote into its own memory (seen %d times):\n\n%s", e.SeenCount, e.Text)})
		}
	}

	skills, err := ListSkillProposals(root, agentID)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range skills {
		c, why := skillCandidate(root, agentID, p)
		if why != "" {
			held = append(held, Held{KindSkill, p.Skill, why})
		} else {
			cands = append(cands, c)
		}
	}

	agent, err := manifest.Load(root, agentID, inst.Names.Secret)
	if err != nil {
		return nil, nil, err
	}
	if list, meta, err := ReadSchedulesProposal(root, agentID); err != nil {
		return nil, nil, err
	} else if meta != nil {
		c, why := schedulesCandidate(agent, list, meta)
		if why != "" {
			held = append(held, Held{KindSchedules, KindSchedules, why})
		} else {
			cands = append(cands, c)
		}
	}

	settings, err := ListSettingProposals(root, agentID)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range settings {
		switch {
		case p.Sensitive:
			held = append(held, Held{KindSetting, p.Key, "widens what the agent may do"})
		case len(p.PII) > 0:
			held = append(held, Held{KindSetting, p.Key, "the scrubber flagged " + strings.Join(p.PII, ", ")})
		default:
			cands = append(cands, Candidate{Kind: KindSetting, Ref: p.Key, Hash: settingHash(p), Title: "setting " + p.Key,
				Body: fmt.Sprintf("A setting the agent changed at runtime.\n\nsetting: %s\nnow in the repo: %s\nproposed: %s", p.Key, orNone(p.Current), p.Value)})
		}
	}
	return cands, held, nil
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func settingHash(p SettingProposal) string { return shaText(p.Key + "\n" + p.Value) }

func skillCandidate(root, agent string, p SkillProposal) (Candidate, string) {
	switch {
	case len(p.PII) > 0:
		return Candidate{}, "the scrubber flagged " + strings.Join(p.PII, ", ")
	case p.Stale:
		return Candidate{}, "changes an older version than the repo's (merge by hand)"
	}
	src := filepath.Join(ProposalsDir(root, agent), p.Skill)
	cur := filepath.Join(manifest.AgentDir(root, agent), "skills", p.Skill)
	var b strings.Builder
	size := 0
	why := ""
	files := []string{}
	_ = filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".proposal.json") {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	isNew := !fileExists(cur)
	if isNew {
		fmt.Fprintf(&b, "A new skill the agent wrote: %s\n", p.Skill)
	} else {
		fmt.Fprintf(&b, "A change the agent made to its skill %s (current version, then proposed)\n", p.Skill)
	}
	for _, rel := range files {
		if !slices.Contains(skillTextExt, strings.ToLower(filepath.Ext(rel))) {
			why = "holds " + rel + ", which is not text the agent reads (scripts and binaries wait for a person)"
			break
		}
		body, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			why = err.Error()
			break
		}
		size += len(body)
		if !isNew {
			if old, err := os.ReadFile(filepath.Join(cur, filepath.FromSlash(rel))); err == nil {
				if string(old) == string(body) {
					continue
				}
				size += len(old)
				fmt.Fprintf(&b, "\n--- current %s ---\n%s\n", rel, old)
			}
		}
		fmt.Fprintf(&b, "\n--- proposed %s ---\n%s\n", rel, body)
	}
	if why == "" && size > maxSkillReview {
		why = fmt.Sprintf("%d KB of text, more than the review reads", size>>10)
	}
	if why != "" {
		return Candidate{}, why
	}
	return Candidate{Kind: KindSkill, Ref: p.Skill, Hash: p.NapHash, Title: "skill " + p.Skill, Body: b.String()}, ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func shaText(s string) string { return build.Sha256([]byte(s)) }

// schedulesCandidate: only new or changed jobs that run a prompt with the agent's own model and
// deliver where the agent already delivers; a job removed from the repo's list, a script, a
// monitor, another model or an explicit delivery target waits for a person.
func schedulesCandidate(agent *manifest.Agent, list []manifest.Schedule, meta *SchedulesProposal) (Candidate, string) {
	if len(meta.PII) > 0 {
		return Candidate{}, "the scrubber flagged " + strings.Join(meta.PII, ", ")
	}
	repo := map[string]manifest.Schedule{}
	for _, s := range agent.Schedules {
		repo[s.ID] = s
	}
	kinds := []string{"", "home", "origin"}
	for _, c := range agent.Channels {
		kinds = append(kinds, c.Kind)
	}
	inList := map[string]bool{}
	changed := []manifest.Schedule{}
	for _, s := range list {
		inList[s.ID] = true
		if old, ok := repo[s.ID]; ok && sameSchedules([]manifest.Schedule{old}, []manifest.Schedule{s}) {
			continue
		}
		switch {
		case s.Script != "" || s.Monitor != "":
			return Candidate{}, "job " + s.ID + " runs a script"
		case s.Model != "" || s.Provider != "":
			return Candidate{}, "job " + s.ID + " uses another model"
		case !slices.Contains(kinds, s.Deliver) || !slices.Contains(kinds, s.OnFailure):
			return Candidate{}, "job " + s.ID + " delivers to an explicit target"
		}
		changed = append(changed, s)
	}
	for id := range repo {
		if !inList[id] {
			return Candidate{}, "removes the repo's job " + id
		}
	}
	var b strings.Builder
	b.WriteString("Scheduled jobs the agent created or changed at runtime (new or changed jobs only; for a changed job, the repo's version first).\n")
	for _, s := range changed {
		if old, ok := repo[s.ID]; ok {
			y, _ := yaml.Marshal(old)
			fmt.Fprintf(&b, "\n--- repo's job %s ---\n%s", s.ID, y)
		}
		y, _ := yaml.Marshal(s)
		fmt.Fprintf(&b, "\n--- proposed job %s ---\n%s", s.ID, y)
	}
	return Candidate{Kind: KindSchedules, Ref: KindSchedules, Hash: meta.Hash, Title: "schedules", Body: b.String()}, ""
}

// Approve applies an approval of the review to one candidate, recording who approved it and why.
// It refuses when the proposal changed since it was judged (its hash no longer matches).
func Approve(inst *instance.Instance, agentID string, c Candidate, who Decider) error {
	cands, _, err := ReviewCandidates(inst, agentID, 1)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(cands, func(x Candidate) bool { return x.Kind == c.Kind && x.Ref == c.Ref && x.Hash == c.Hash })
	if i < 0 {
		return fmt.Errorf("%s: %s %s changed or is no longer open", agentID, c.Kind, c.Ref)
	}
	switch c.Kind {
	case KindLesson:
		st := learning.Accepted
		_, err := Decide(inst.Root, agentID, []string{c.Ref}, Change{Status: &st, Note: who.note()})
		return err
	case KindSkill:
		_, err := DecideSkillBy(inst.Root, agentID, c.Ref, "accepted", who)
		return err
	case KindSchedules:
		_, err := DecideSchedules(inst, agentID, "accepted", who)
		return err
	case KindSetting:
		_, err := DecideSetting(inst, agentID, c.Ref, "accepted", who)
		return err
	}
	return fmt.Errorf("unknown proposal kind %q", c.Kind)
}

func (d Decider) note() string {
	if d.By == "" {
		return ""
	}
	if d.Reason == "" {
		return "approved by " + d.By
	}
	return "approved by " + d.By + ": " + d.Reason
}
