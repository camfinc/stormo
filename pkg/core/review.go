package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The review page's backend (owner token only): what the dream proposed for each agent (lessons,
// skills, runtime schedules and settings), what the review's first pass said about each and why the
// rules held some for a person, the decisions (the same loop functions as `stormo learn`, working
// tree only), and a local restart so what was accepted applies. Lessons are scrubbed text; skill
// proposals are files the agent wrote. Neither reaches anyone but the owner.

// ReviewLesson is one ledger entry on the review page.
type ReviewLesson struct {
	Evidence  learning.EvidenceSummary `json:"evidence"`
	ID        string                   `json:"id"`
	Kind      string                   `json:"kind"`
	Scope     string                   `json:"scope"`
	Text      string                   `json:"text"`
	PII       []string                 `json:"pii"`
	Status    string                   `json:"status"`
	SeenCount int                      `json:"seenCount"`
	FirstSeen string                   `json:"firstSeen"`
	LastSeen  string                   `json:"lastSeen"`
	DecidedAt string                   `json:"decidedAt,omitempty"`
	Note      string                   `json:"note,omitempty"`
	// What the review's rules let its model judge (agent-scoped memory, no PII flag).
	AutoAcceptable bool `json:"autoAcceptable"`
	// The review's verdict on this exact text, and why the rules hold it for a person.
	AI   *ReviewAI `json:"ai,omitempty"`
	Held string    `json:"held,omitempty"`
}

// ReviewAI is the review's first pass on a proposal: approve, or passed to a person, and why.
type ReviewAI struct {
	Approve bool   `json:"approve"`
	Reason  string `json:"reason"`
	Model   string `json:"model"`
	At      string `json:"at"`
}

// ReviewSchedules is the agent's runtime schedules, as agent.yaml would hold them.
type ReviewSchedules struct {
	Jobs []manifest.Schedule `json:"jobs"`
	// RuntimeJobs are the ids the repo does not have (a reject drops them).
	RuntimeJobs []string `json:"runtimeJobs"`
	// Scripts the agent wrote or edited for them; an accept puts them in agents/<id>/scripts/.
	Scripts []ReviewScript `json:"scripts"`
	PII     []string       `json:"pii"`
	Nap     string         `json:"nap"`
	AI      *ReviewAI      `json:"ai,omitempty"`
	Held    string         `json:"held,omitempty"`
}

// ReviewScript is a script in a schedules proposal, with its text when small and textual.
type ReviewScript struct {
	Name   string `json:"name"`
	Status string `json:"status"` // new | changed
	// Stale: the repo changed it after the agent did; accept is refused.
	Stale bool   `json:"stale,omitempty"`
	Size  int64  `json:"size"`
	Text  string `json:"text,omitempty"`
	// The repo's version, when the script is changed.
	Current *string `json:"current,omitempty"`
}

// ReviewSetting is a setting the agent changed at runtime.
type ReviewSetting struct {
	loop.SettingProposal
	AI   *ReviewAI `json:"ai,omitempty"`
	Held string    `json:"held,omitempty"`
}

// ReviewSkill is one skill proposal.
type ReviewSkill struct {
	Evidence learning.EvidenceSummary `json:"evidence"`
	Skill    string                   `json:"skill"`
	PII      []string                 `json:"pii"`
	Nap      string                   `json:"nap"`
	// New skill, or a change to one the agent already has.
	New bool `json:"new"`
	// Stale: the agent's change is to an older version than the repo's (built from commit Base);
	// the diff against the repo shows the repo's newer edits as removals, and accept is refused.
	Stale bool         `json:"stale"`
	Base  string       `json:"base,omitempty"`
	Files []ReviewFile `json:"files"`
	AI    *ReviewAI    `json:"ai,omitempty"`
	Held  string       `json:"held,omitempty"`
}

// ReviewFile is a file of a skill proposal, with its text when small and textual.
type ReviewFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Text string `json:"text,omitempty"`
	// The current version, when the skill exists and this file differs.
	Current *string `json:"current,omitempty"`
}

// ReviewAgent is one agent's column of the review page.
type ReviewAgent struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Unit      string           `json:"unit"`
	State     string           `json:"state"`
	Busy      bool             `json:"busy"`
	Lessons   []ReviewLesson   `json:"lessons"` // proposed, then accepted (which can be promoted)
	Skills    []ReviewSkill    `json:"skills"`
	Schedules *ReviewSchedules `json:"schedules"`
	Settings  []ReviewSetting  `json:"settings"`
	// AutoReview: the review's first pass covers this agent (agent.yaml learning.auto_review).
	AutoReview bool `json:"autoReview"`
	// Decisions since it last restarted through the core: restart to apply them.
	Unapplied bool `json:"unapplied"`
}

// ReviewView is GET /api/review.
type ReviewView struct {
	// Review is core.learning.review (auto | manual), with the model it asks and the instance's
	// rules file; AutoAccept is Review == "auto" (the field readers had before).
	Review                     string        `json:"review"`
	ReviewConnection           string        `json:"reviewConnection"`
	ReviewModel                string        `json:"reviewModel,omitempty"`
	ReviewPrompt               string        `json:"reviewPrompt,omitempty"`
	AutoAccept                 bool          `json:"autoAccept"`
	AutoAcceptMinSeen          int           `json:"autoAcceptMinSeen"`
	AutoAcceptMinConfirmations int           `json:"autoAcceptMinConfirmations"`
	AutoRestart                bool          `json:"autoRestart"`
	Agents                     []ReviewAgent `json:"agents"`
}

const maxReviewText = 64 << 10

func (l *Learner) review(roster map[string]BusAgent) (*ReviewView, error) {
	root := l.o.Inst.Root
	cfg := l.o.Inst.CoreLearning
	v := &ReviewView{Review: cfg.Review, ReviewConnection: cfg.ReviewConnection, ReviewModel: cfg.ReviewModel, ReviewPrompt: cfg.ReviewPrompt,
		AutoAccept: cfg.AutoAccept, AutoAcceptMinSeen: cfg.AutoAcceptMinSeen, AutoAcceptMinConfirmations: cfg.AutoAcceptMinConfirmations, AutoRestart: cfg.AutoRestart, Agents: []ReviewAgent{}}
	unapplied := l.Unapplied()
	ids := manifest.AgentIDs(root)
	sort.Strings(ids)
	for _, id := range ids {
		a := ReviewAgent{ID: id, Name: id, Lessons: []ReviewLesson{}, Skills: []ReviewSkill{}, Settings: []ReviewSetting{}, Unapplied: unapplied[id], State: "stopped"}
		if m, err := manifest.Load(root, id, l.o.Inst.Names.Secret); err == nil {
			a.Name, a.Unit, a.AutoReview = m.Name, m.Unit, m.Learning.AutoReview
		}
		ai, held := l.reviewNotes(id)
		if b, ok := roster[id]; ok {
			a.Busy = b.Busy
			if b.Running {
				a.State = "running"
			}
		}
		ledger, err := learning.LoadLedger(root, id)
		if err != nil {
			return nil, err
		}
		for _, e := range ledger {
			if e.Status == learning.Rejected {
				continue
			}
			r := ReviewLesson{ID: e.ID, Kind: string(e.Kind), Scope: string(e.Scope), Text: e.Text, PII: e.PII, Status: string(e.Status),
				Evidence: e.Evidence, SeenCount: e.SeenCount, FirstSeen: e.FirstSeen, LastSeen: e.LastSeen, AutoAcceptable: loop.AutoAcceptable(e, 1)}
			if r.PII == nil {
				r.PII = []string{}
			}
			if e.DecidedAt != nil {
				r.DecidedAt = *e.DecidedAt
			}
			if e.Note != nil {
				r.Note = *e.Note
			}
			if e.Status == learning.Proposed {
				r.AI, r.Held = ai[loop.KindLesson+"/"+e.ID], held[loop.KindLesson+"/"+e.ID]
			}
			a.Lessons = append(a.Lessons, r)
		}
		sort.SliceStable(a.Lessons, func(i, j int) bool {
			pi, pj := a.Lessons[i].Status == "proposed", a.Lessons[j].Status == "proposed"
			if pi != pj {
				return pi
			}
			return a.Lessons[i].LastSeen > a.Lessons[j].LastSeen
		})
		props, err := loop.ListSkillProposals(root, id)
		if err != nil {
			return nil, err
		}
		evidence, err := learning.LoadEvidence(root, id)
		if err != nil {
			return nil, err
		}
		for _, p := range props {
			sv := skillView(root, id, p)
			sv.Evidence = evidence.Summary("skill", build.Sha256([]byte(p.Skill)), p.NapHash, "agent")
			sv.AI, sv.Held = ai[loop.KindSkill+"/"+p.Skill], held[loop.KindSkill+"/"+p.Skill]
			a.Skills = append(a.Skills, sv)
		}
		if jobs, meta, err := loop.ReadSchedulesProposal(root, id); err != nil {
			return nil, err
		} else if meta != nil {
			a.Schedules = &ReviewSchedules{Jobs: jobs, RuntimeJobs: meta.RuntimeJobs, Scripts: scriptsView(root, id, meta.Scripts), PII: meta.PII,
				Nap: meta.Nap, AI: ai[loop.KindSchedules+"/"+loop.KindSchedules], Held: held[loop.KindSchedules+"/"+loop.KindSchedules]}
		}
		settings, err := loop.ListSettingProposals(root, id)
		if err != nil {
			return nil, err
		}
		for _, p := range settings {
			a.Settings = append(a.Settings, ReviewSetting{SettingProposal: p, AI: ai[loop.KindSetting+"/"+p.Key], Held: held[loop.KindSetting+"/"+p.Key]})
		}
		v.Agents = append(v.Agents, a)
	}
	return v, nil
}

// reviewNotes are, by kind + "/" + ref, the review's verdict on each open proposal as it reads now
// (a verdict on older content is not shown) and why the rules hold the others for a person.
func (l *Learner) reviewNotes(agent string) (map[string]*ReviewAI, map[string]string) {
	ai, held := map[string]*ReviewAI{}, map[string]string{}
	cands, hs, err := loop.ReviewCandidates(l.o.Inst, agent, l.o.Inst.CoreLearning.AutoAcceptMinSeen)
	if err != nil {
		return ai, held
	}
	for _, h := range hs {
		held[h.Kind+"/"+h.Ref] = h.Why
	}
	latest := l.LatestVerdicts(agent)
	for _, c := range cands {
		if v, ok := latest[c.Kind+"/"+c.Ref]; ok && v.Hash == c.Hash {
			ai[c.Kind+"/"+c.Ref] = &ReviewAI{Approve: v.Approve, Reason: v.Reason, Model: v.Model, At: v.At}
		}
	}
	return ai, held
}

func readSmallText(p string) (string, int64, bool) {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return "", 0, false
	}
	if st.Size() > maxReviewText {
		return "", st.Size(), true
	}
	b, err := os.ReadFile(p)
	if err != nil || strings.ContainsRune(string(b), 0) {
		return "", st.Size(), true
	}
	return string(b), st.Size(), true
}

func scriptsView(root, agent string, scripts []loop.ScriptChange) []ReviewScript {
	out := []ReviewScript{}
	for _, c := range scripts {
		text, size, _ := readSmallText(filepath.Join(loop.SchedulesScriptsDir(root, agent), c.Name))
		r := ReviewScript{Name: c.Name, Status: c.Status, Stale: c.Stale, Size: size, Text: text}
		if c.Status == "changed" {
			if cur, _, ok := readSmallText(filepath.Join(manifest.AgentDir(root, agent), "scripts", c.Name)); ok {
				r.Current = &cur
			}
		}
		out = append(out, r)
	}
	return out
}

func skillView(root, agent string, p loop.SkillProposal) ReviewSkill {
	src := filepath.Join(loop.ProposalsDir(root, agent), p.Skill)
	cur := filepath.Join(manifest.AgentDir(root, agent), "skills", p.Skill)
	s := ReviewSkill{Skill: p.Skill, PII: p.PII, Nap: p.Nap, Stale: p.Stale, Base: p.Base, Files: []ReviewFile{}}
	if s.PII == nil {
		s.PII = []string{}
	}
	if _, err := os.Stat(cur); err != nil {
		s.New = true
	}
	_ = filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".proposal.json") {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		text, size, _ := readSmallText(path)
		f := ReviewFile{Path: filepath.ToSlash(rel), Size: size, Text: text}
		if !s.New {
			if old, _, ok := readSmallText(filepath.Join(cur, rel)); ok && old != text {
				f.Current = &old
			} else if !ok {
				empty := ""
				f.Current = &empty
			}
		}
		s.Files = append(s.Files, f)
		return nil
	})
	return s
}

// reviewDecision is POST /api/review's body.
type reviewDecision struct {
	Agent     string   `json:"agent"`
	Lessons   []string `json:"lessons"`
	Skill     string   `json:"skill"`
	Schedules bool     `json:"schedules"`
	Settings  []string `json:"settings"`
	Decision  string   `json:"decision"` // accept | reject | promote (lessons)
	Scope     string   `json:"scope"`    // promote: unit | group
}

func (l *Learner) decide(d reviewDecision) error {
	root := l.o.Inst.Root
	if !slicesContains(manifest.AgentIDs(root), d.Agent) {
		return fmt.Errorf("unknown agent %q", d.Agent)
	}
	dec := map[string]string{"accept": "accepted", "reject": "rejected"}[d.Decision]
	switch {
	case d.Schedules || len(d.Settings) > 0:
		if dec == "" {
			return fmt.Errorf("schedules and settings are accepted or rejected")
		}
		if d.Schedules {
			if _, err := loop.DecideSchedules(l.o.Inst, d.Agent, dec, loop.Decider{}); err != nil {
				return err
			}
		}
		for _, key := range d.Settings {
			if _, err := loop.DecideSetting(l.o.Inst, d.Agent, key, dec, loop.Decider{}); err != nil {
				return err
			}
		}
		// A reject of schedules changes the next boot too (its runtime jobs are dropped).
		l.markUnapplied(d.Agent)
		return nil
	case d.Skill != "":
		if dec == "" {
			return fmt.Errorf("a skill is accepted or rejected")
		}
		if _, err := loop.DecideSkill(root, d.Agent, d.Skill, dec); err != nil {
			return err
		}
		if dec == "accepted" {
			l.markUnapplied(d.Agent)
		}
		return nil
	case len(d.Lessons) > 0:
		var c loop.Change
		switch d.Decision {
		case "accept":
			st := learning.Accepted
			c.Status = &st
		case "reject":
			st := learning.Rejected
			c.Status = &st
		case "promote":
			sc := manifest.Scope(d.Scope)
			if sc != manifest.ScopeUnit && sc != manifest.ScopeGroup {
				return fmt.Errorf("promote to unit or group")
			}
			c.Scope = &sc
		default:
			return fmt.Errorf("decision is accept, reject or promote")
		}
		if _, err := loop.Decide(root, d.Agent, d.Lessons, c); err != nil {
			return err
		}
		if d.Decision != "reject" {
			l.markUnapplied(d.Agent)
		}
		return nil
	}
	return fmt.Errorf("name lessons, a skill, schedules or settings")
}

func slicesContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// handleReview serves /api/review (GET, POST) and /api/agents/<id>/restart for the owner.
func (c *Core) handleReview(w http.ResponseWriter, r *http.Request, roster func() []BusAgent) {
	if !c.caller(r).Owner {
		writeJSONBody(w, 401, errBody("unauthorized", "the owner token is required (open the page with `stormo core review`)"))
		return
	}
	byID := map[string]BusAgent{}
	for _, a := range roster() {
		byID[a.ID] = a
	}
	switch r.Method {
	case http.MethodGet:
		v, err := c.Learner.review(byID)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", clip(err.Error(), 300)))
			return
		}
		writeJSONBody(w, 200, v)
	case http.MethodPost:
		var d reviewDecision
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&d); err != nil {
			writeJSONBody(w, 400, errBody("bad_request", "body is {agent, lessons | skill | schedules | settings, decision, scope}"))
			return
		}
		if err := c.Learner.decide(d); err != nil {
			writeJSONBody(w, 400, errBody("refused", err.Error()))
			return
		}
		target := append(append(append([]string{}, d.Lessons...), d.Settings...), d.Skill)
		if d.Schedules {
			target = append(target, "schedules")
		}
		c.DB.Audit(c.caller(r).Actor(), "review."+d.Decision, d.Agent, strings.TrimSpace(strings.Join(target, " ")))
		writeJSONBody(w, 200, map[string]any{"ok": true})
	default:
		writeJSONBody(w, 405, errBody("method_not_allowed", "GET or POST"))
	}
}
