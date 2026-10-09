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

	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The review page's backend (owner token only): what the dream proposed for each agent, the
// decisions (the same loop.Decide / loop.DecideSkill as `stormo learn`, working tree only), and a
// local restart so accepted lessons and skills apply. Lessons are scrubbed text; skill proposals
// are files the agent wrote. Neither reaches anyone but the owner.

// ReviewLesson is one ledger entry on the review page.
type ReviewLesson struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Scope     string   `json:"scope"`
	Text      string   `json:"text"`
	PII       []string `json:"pii"`
	Status    string   `json:"status"`
	SeenCount int      `json:"seenCount"`
	FirstSeen string   `json:"firstSeen"`
	LastSeen  string   `json:"lastSeen"`
	DecidedAt string   `json:"decidedAt,omitempty"`
	Note      string   `json:"note,omitempty"`
	// What auto_accept would take (agent-scoped memory, no PII flag).
	AutoAcceptable bool `json:"autoAcceptable"`
}

// ReviewSkill is one skill proposal.
type ReviewSkill struct {
	Skill string   `json:"skill"`
	PII   []string `json:"pii"`
	Nap   string   `json:"nap"`
	// New skill, or a change to one the agent already has.
	New   bool         `json:"new"`
	Files []ReviewFile `json:"files"`
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
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Unit    string         `json:"unit"`
	State   string         `json:"state"`
	Busy    bool           `json:"busy"`
	Lessons []ReviewLesson `json:"lessons"` // proposed, then accepted (which can be promoted)
	Skills  []ReviewSkill  `json:"skills"`
	// Decisions since it last restarted through the core: restart to apply them.
	Unapplied bool `json:"unapplied"`
}

// ReviewView is GET /api/review.
type ReviewView struct {
	AutoAccept        bool          `json:"autoAccept"`
	AutoAcceptMinSeen int           `json:"autoAcceptMinSeen"`
	AutoRestart       bool          `json:"autoRestart"`
	Agents            []ReviewAgent `json:"agents"`
}

const maxReviewText = 64 << 10

func (l *Learner) review(roster map[string]BusAgent) (*ReviewView, error) {
	root := l.o.Inst.Root
	cfg := l.o.Inst.CoreLearning
	v := &ReviewView{AutoAccept: cfg.AutoAccept, AutoAcceptMinSeen: cfg.AutoAcceptMinSeen, AutoRestart: cfg.AutoRestart, Agents: []ReviewAgent{}}
	unapplied := l.Unapplied()
	ids := manifest.AgentIDs(root)
	sort.Strings(ids)
	for _, id := range ids {
		a := ReviewAgent{ID: id, Name: id, Lessons: []ReviewLesson{}, Skills: []ReviewSkill{}, Unapplied: unapplied[id], State: "stopped"}
		if m, err := manifest.Load(root, id, l.o.Inst.Names.Secret); err == nil {
			a.Name, a.Unit = m.Name, m.Unit
		}
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
				SeenCount: e.SeenCount, FirstSeen: e.FirstSeen, LastSeen: e.LastSeen, AutoAcceptable: loop.AutoAcceptable(e, 1)}
			if r.PII == nil {
				r.PII = []string{}
			}
			if e.DecidedAt != nil {
				r.DecidedAt = *e.DecidedAt
			}
			if e.Note != nil {
				r.Note = *e.Note
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
		for _, p := range props {
			a.Skills = append(a.Skills, skillView(root, id, p))
		}
		v.Agents = append(v.Agents, a)
	}
	return v, nil
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

func skillView(root, agent string, p loop.SkillProposal) ReviewSkill {
	src := filepath.Join(loop.ProposalsDir(root, agent), p.Skill)
	cur := filepath.Join(manifest.AgentDir(root, agent), "skills", p.Skill)
	s := ReviewSkill{Skill: p.Skill, PII: p.PII, Nap: p.Nap, Files: []ReviewFile{}}
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
	Agent    string   `json:"agent"`
	Lessons  []string `json:"lessons"`
	Skill    string   `json:"skill"`
	Decision string   `json:"decision"` // accept | reject | promote (lessons)
	Scope    string   `json:"scope"`    // promote: unit | group
}

func (l *Learner) decide(d reviewDecision) error {
	root := l.o.Inst.Root
	if !slicesContains(manifest.AgentIDs(root), d.Agent) {
		return fmt.Errorf("unknown agent %q", d.Agent)
	}
	switch {
	case d.Skill != "":
		dec := map[string]string{"accept": "accepted", "reject": "rejected"}[d.Decision]
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
	return fmt.Errorf("name lessons or a skill")
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
	if id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/restart"); ok && r.Method == http.MethodPost {
		a, known := byID[id]
		if !known || !a.Running {
			writeJSONBody(w, 409, errBody("conflict", id+" is not running here; it picks up decisions when it starts"))
			return
		}
		if err := c.Learner.o.Deps.Restart(id); err != nil {
			writeJSONBody(w, 500, errBody("failed", clip(err.Error(), 300)))
			return
		}
		c.Learner.markApplied(id)
		writeJSONBody(w, 200, map[string]any{"restarted": id})
		return
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
			writeJSONBody(w, 400, errBody("bad_request", "body is {agent, lessons | skill, decision, scope}"))
			return
		}
		if err := c.Learner.decide(d); err != nil {
			writeJSONBody(w, 400, errBody("refused", err.Error()))
			return
		}
		writeJSONBody(w, 200, map[string]any{"ok": true})
	default:
		writeJSONBody(w, 405, errBody("method_not_allowed", "GET or POST"))
	}
}
