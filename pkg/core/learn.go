package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/core/llm"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/ops"
	"github.com/camfinc/stormo/pkg/place"
)

// The learning cycle (docs/core.md §6, phase 6). The core sequences the existing nap/dream loop:
// for each agent, wait until it is quiet, nap it (the same `compose exec nap` as `stormo
// nap-now`), dream its naps into agents/<id>/learnings (working tree only, as `stormo learn`
// does), give the review's first pass what the rules let through (core.learning.review: auto; a
// model approves what is obviously safe and passes the rest to a person), and count what waits for
// review. A cycle runs on the instance's schedule (stormo.yaml core.learning.at) or on demand. The
// core never commits, rejects or promotes anything; the digest lists counts and the `stormo learn`
// commands to review the rest.

// LearnDeps are the cycle's effects; zero values take the instance's real ones.
type LearnDeps struct {
	NapNow func(id string) error
	Dream  func(id string) (*loop.DreamReport, error)
	// Prune drops the naps the dream has folded (loop.Prune).
	Prune func(id string) (*loop.PruneReport, error)
	// Candidates sorts an agent's open proposals by the review's rules (loop.ReviewCandidates).
	Candidates func(id string, minSeen int) ([]loop.Candidate, []loop.Held, error)
	// Rules is the hash of the review's rules now (Reviewer.Rules); Judge asks its model about one
	// candidate (Reviewer.Judge); Approve applies an approval (loop.Approve).
	Rules   func() (string, error)
	Judge   func(id string, c loop.Candidate) (Verdict, error)
	Approve func(id string, c loop.Candidate, who loop.Decider) error
	// AutoReview: the agent lets the review approve on its own (agent.yaml learning.auto_review).
	AutoReview func(id string) bool
	// Restart recycles a local agent so accepted lessons apply (ops.Restart, local only).
	Restart func(id string) error
	Pending func(id string) (learnings, skills int)
	// Sleep waits d or until stop closes (false: stopped).
	Sleep func(d time.Duration, stop <-chan struct{}) bool
}

// LearnerOptions configure a Learner.
type LearnerOptions struct {
	Inst   *instance.Instance
	DB     *DB
	Roster func() []BusAgent
	Deps   LearnDeps
	Now    func() time.Time
	// DigestDir holds the cycle digests (default .swarm/core/digests).
	DigestDir string
	// Poll is how often a busy agent is looked at again (default 30 s).
	Poll time.Duration
	// Gateways serve the review's model calls for a ChatGPT connection.
	Gateways *llm.Gateways
}

// AgentRun is one agent's part of a cycle. Counts only: lessons can hold client data.
type AgentRun struct {
	Agent            string `json:"agent"`
	Started          string `json:"started"`
	Finished         string `json:"finished"`
	Napped           bool   `json:"napped"`
	NapNote          string `json:"napNote,omitempty"`
	Naps             int    `json:"naps"`
	NewLearnings     int    `json:"newLearnings"`
	Resighted        int    `json:"resighted"`
	SkillProposals   int    `json:"skillProposals"`
	CronProposal     bool   `json:"cronProposal"`
	SettingProposals int    `json:"settingProposals"`
	PendingLearnings int    `json:"pendingLearnings"`
	PendingSkills    int    `json:"pendingSkills"`
	// The review's first pass: proposals it approved, passed to a person, and the rules held for a
	// person without asking; why it did not run or stopped. Whether the agent was restarted so the
	// approvals apply (auto_restart), or why not.
	AutoAccepted int    `json:"autoAccepted"`
	Passed       int    `json:"passed"`
	Held         int    `json:"held"`
	ReviewNote   string `json:"reviewNote,omitempty"`
	Restarted    bool   `json:"restarted"`
	RestartNote  string `json:"restartNote,omitempty"`
	// Naps and bodies pruned after the dream, and their size.
	PrunedNaps  int    `json:"prunedNaps"`
	PrunedBytes int64  `json:"prunedBytes"`
	Error       string `json:"error,omitempty"`
}

// Cycle is one learning cycle.
type Cycle struct {
	ID       int64      `json:"id"`
	Trigger  string     `json:"trigger"` // schedule | manual
	Started  string     `json:"started"`
	Finished string     `json:"finished,omitempty"`
	Status   string     `json:"status"` // running | done | interrupted
	Digest   string     `json:"digest,omitempty"`
	Runs     []AgentRun `json:"runs"`
}

// Learner runs learning cycles; one at a time.
type Learner struct {
	o         LearnerOptions
	mu        sync.Mutex
	running   int64
	unapplied map[string]bool
	stop      chan struct{}
}

// ErrCycleRunning: a cycle is already running.
var ErrCycleRunning = errors.New("a learning cycle is already running")

// NewLearner builds a learner. Cycles a stopped core left running are marked interrupted.
func NewLearner(o LearnerOptions) *Learner {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Poll == 0 {
		o.Poll = 30 * time.Second
	}
	if o.DigestDir == "" {
		o.DigestDir = filepath.Join(CoreDir(o.Inst.Root), "digests")
	}
	inst := o.Inst
	d := &o.Deps
	if d.NapNow == nil {
		d.NapNow = func(id string) error { return local.NapNow(inst, id) }
	}
	if d.Dream == nil {
		d.Dream = func(id string) (*loop.DreamReport, error) { return loop.Dream(inst, id, ops.LocalStore(inst.Root)) }
	}
	if d.Prune == nil {
		d.Prune = func(id string) (*loop.PruneReport, error) { return loop.Prune(inst, id, ops.LocalStore(inst.Root), 0) }
	}
	if d.Candidates == nil {
		d.Candidates = func(id string, minSeen int) ([]loop.Candidate, []loop.Held, error) {
			return loop.ReviewCandidates(inst, id, minSeen)
		}
	}
	reviewer := &Reviewer{Inst: inst, Gateways: o.Gateways, Scrub: Scrubber(learning.NewScrubber(inst))}
	if d.Rules == nil {
		d.Rules = func() (string, error) { _, h, err := reviewer.Rules(); return h, err }
	}
	if d.Judge == nil {
		d.Judge = func(id string, c loop.Candidate) (Verdict, error) {
			a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
			if err != nil {
				return Verdict{}, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			return reviewer.Judge(ctx, a, c)
		}
	}
	if d.Approve == nil {
		d.Approve = func(id string, c loop.Candidate, who loop.Decider) error { return loop.Approve(inst, id, c, who) }
	}
	if d.AutoReview == nil {
		d.AutoReview = func(id string) bool {
			a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
			return err == nil && a.Learning.AutoReview
		}
	}
	if d.Restart == nil {
		d.Restart = func(id string) error {
			deps := ops.DefaultDeps(inst, false)
			deps.Log = func(l string) { log.Printf("learn: restart %s: %s", id, l) }
			return ops.Restart(place.Local, id, deps)
		}
	}
	if d.Pending == nil {
		d.Pending = func(id string) (int, int) {
			n := 0
			if ledger, err := learning.LoadLedger(inst.Root, id); err == nil {
				for _, e := range ledger {
					if e.Status == learning.Proposed {
						n++
					}
				}
			}
			skills, _ := loop.ListSkillProposals(inst.Root, id)
			return n, len(skills)
		}
	}
	if d.Sleep == nil {
		d.Sleep = func(dur time.Duration, stop <-chan struct{}) bool {
			t := time.NewTimer(dur)
			defer t.Stop()
			select {
			case <-t.C:
				return true
			case <-stop:
				return false
			}
		}
	}
	if _, err := o.DB.Exec(`UPDATE learn_cycles SET status = 'interrupted', finished = ? WHERE status = 'running'`, o.Now().UnixMilli()); err != nil {
		log.Printf("learn: %v", err)
	}
	return &Learner{o: o, stop: make(chan struct{}), unapplied: map[string]bool{}}
}

// Stop ends a running cycle at its next wait and the scheduler.
func (l *Learner) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
}

func (l *Learner) agents(only []string) ([]string, error) {
	ids := manifest.AgentIDs(l.o.Inst.Root)
	sort.Strings(ids)
	if len(only) == 0 {
		return ids, nil
	}
	out := []string{}
	for _, id := range only {
		if !slices.Contains(ids, id) {
			return nil, fmt.Errorf("unknown agent %q", id)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// Start runs a cycle in the background and returns its id. force naps busy agents too.
func (l *Learner) Start(trigger string, only []string, force bool) (int64, error) {
	ids, err := l.agents(only)
	if err != nil {
		return 0, err
	}
	id, err := l.begin(trigger)
	if err != nil {
		return 0, err
	}
	go l.run(id, trigger, ids, force)
	return id, nil
}

// RunCycle runs a cycle to the end (tests, and Start's goroutine).
func (l *Learner) RunCycle(trigger string, only []string, force bool) (*Cycle, error) {
	ids, err := l.agents(only)
	if err != nil {
		return nil, err
	}
	id, err := l.begin(trigger)
	if err != nil {
		return nil, err
	}
	l.run(id, trigger, ids, force)
	return l.Cycle(id)
}

func (l *Learner) begin(trigger string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running != 0 {
		return 0, ErrCycleRunning
	}
	res, err := l.o.DB.Exec(`INSERT INTO learn_cycles(trigger, started, status) VALUES(?, ?, 'running')`, trigger, l.o.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	l.running = id
	return id, nil
}

func (l *Learner) roster() map[string]BusAgent {
	out := map[string]BusAgent{}
	if l.o.Roster != nil {
		for _, a := range l.o.Roster() {
			out[a.ID] = a
		}
	}
	return out
}

func (l *Learner) run(cycle int64, trigger string, ids []string, force bool) {
	defer func() {
		l.mu.Lock()
		l.running = 0
		l.mu.Unlock()
	}()
	stagger := time.Duration(l.o.Inst.CoreLearning.StaggerMinutes) * time.Minute
	quiet := time.Duration(l.o.Inst.CoreLearning.QuietWaitMinutes) * time.Minute
	status := "done"
	for i, id := range ids {
		if i > 0 && trigger == "schedule" && stagger > 0 {
			// Naps across the fleet are spread out; a manual cycle runs straight through.
			if !l.o.Deps.Sleep(stagger, l.stop) {
				status = "interrupted"
				break
			}
		}
		r, ok := l.runAgent(id, force, quiet)
		if !ok {
			status = "interrupted"
			break
		}
		if _, err := l.o.DB.Exec(`INSERT INTO learn_runs(cycle, agent, started, finished, napped, nap_note, naps, new_learnings, resighted, skill_proposals,
			cron_proposal, pending_learnings, pending_skills, error, pruned_naps, pruned_bytes, auto_accepted, restarted, restart_note,
			setting_proposals, passed, held, review_note)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			cycle, id, msOf(r.Started), msOf(r.Finished), r.Napped, r.NapNote, r.Naps, r.NewLearnings, r.Resighted, r.SkillProposals,
			r.CronProposal, r.PendingLearnings, r.PendingSkills, r.Error, r.PrunedNaps, r.PrunedBytes, r.AutoAccepted, r.Restarted, r.RestartNote,
			r.SettingProposals, r.Passed, r.Held, r.ReviewNote); err != nil {
			log.Printf("learn: %s: %v", id, err)
		}
	}
	digest := ""
	if c, err := l.Cycle(cycle); err == nil {
		c.Status = status
		if p, err := l.writeDigest(c); err != nil {
			log.Printf("learn: digest: %v", err)
		} else {
			digest = p
		}
	}
	if _, err := l.o.DB.Exec(`UPDATE learn_cycles SET status = ?, finished = ?, digest = ? WHERE id = ?`, status, l.o.Now().UnixMilli(), digest, cycle); err != nil {
		log.Printf("learn: %v", err)
	}
}

func msOf(iso string) int64 {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// runAgent naps (when it can) and dreams one agent. ok is false when the core is stopping.
func (l *Learner) runAgent(id string, force bool, quiet time.Duration) (r AgentRun, ok bool) {
	r = AgentRun{Agent: id, Started: isoMs(l.o.Now().UnixMilli())}
	a, known := l.roster()[id]
	switch {
	case !known || !a.Running:
		r.NapNote = "not running: dreamed its stored naps"
	default:
		waited := time.Duration(0)
		for a.Busy && !force && waited < quiet {
			if !l.o.Deps.Sleep(l.o.Poll, l.stop) {
				return r, false
			}
			waited += l.o.Poll
			a = l.roster()[id]
		}
		if a.Busy && !force {
			r.NapNote = fmt.Sprintf("busy for %s: no fresh nap (dreamed its stored naps)", quiet)
		} else if err := l.o.Deps.NapNow(id); err != nil {
			r.NapNote = "nap failed: " + clip(err.Error(), 200)
		} else {
			r.Napped = true
		}
	}
	rep, err := l.o.Deps.Dream(id)
	if err != nil {
		r.Error = clip(err.Error(), 300)
	} else if rep != nil {
		r.Naps, r.NewLearnings, r.Resighted, r.SkillProposals, r.CronProposal = len(rep.Naps), rep.NewLearnings, rep.Resighted, len(rep.SkillProposals), rep.CronProposal
		r.SettingProposals = rep.SettingProposals
		l.autoReview(id, &r)
		if pr, err := l.o.Deps.Prune(id); err != nil {
			log.Printf("learn: pruning %s: %v", id, err)
		} else if pr != nil {
			r.PrunedNaps, r.PrunedBytes = pr.Naps, pr.Bytes
		}
	}
	r.PendingLearnings, r.PendingSkills = l.o.Deps.Pending(id)
	r.Finished = isoMs(l.o.Now().UnixMilli())
	return r, true
}

// maxJudged is how many proposals one agent's run asks the review's model about; the rest wait
// for the next cycle (a verdict already given does not count).
const maxJudged = 40

// autoReview runs the review's first pass over one agent's open proposals (core.learning.review:
// auto), then auto_restart. The model only approves or passes to a person; the rules in code hold
// some proposals for a person without asking it.
func (l *Learner) autoReview(id string, r *AgentRun) {
	cfg := l.o.Inst.CoreLearning
	if cfg.Review != instance.ReviewAuto {
		return
	}
	if !l.o.Deps.AutoReview(id) {
		r.ReviewNote = "off for this agent (learning.auto_review)"
		return
	}
	cands, held, err := l.o.Deps.Candidates(id, cfg.AutoAcceptMinSeen)
	if err != nil {
		r.ReviewNote = "rules: " + clip(err.Error(), 200)
		return
	}
	r.Held = len(held)
	rules, err := l.o.Deps.Rules()
	if err != nil {
		r.ReviewNote = clip(err.Error(), 200)
		r.Passed = len(cands)
		return
	}
	judged := 0
	for i, c := range cands {
		v, ok := l.verdict(id, c, rules)
		if !ok {
			if judged == maxJudged {
				r.Passed += len(cands) - i
				r.ReviewNote = fmt.Sprintf("%d left for the next cycle", len(cands)-i)
				break
			}
			judged++
			if v, err = l.o.Deps.Judge(id, c); err != nil {
				// The model is out of reach: everything left waits for a person (or the next cycle).
				r.Passed += len(cands) - i
				r.ReviewNote = "model unavailable: " + clip(err.Error(), 200)
				break
			}
			l.keepVerdict(id, c, v)
		}
		if !v.Approve {
			r.Passed++
			continue
		}
		if err := l.o.Deps.Approve(id, c, loop.Decider{By: "core review (" + v.Model + ")", Reason: v.Reason}); err != nil {
			log.Printf("learn: review %s %s %s: %v", id, c.Kind, c.Ref, err)
			r.Passed++
			continue
		}
		r.AutoAccepted++
	}
	if r.AutoAccepted == 0 {
		return
	}
	l.markUnapplied(id)
	if !cfg.AutoRestart {
		r.RestartNote = "applies when it next restarts"
		return
	}
	a, ok := l.roster()[id]
	switch {
	case !ok || !a.Running:
		r.RestartNote = "not running: applies when it starts"
	case a.Busy:
		r.RestartNote = "busy: applies when it next restarts"
	default:
		if err := l.o.Deps.Restart(id); err != nil {
			r.RestartNote = "restart failed: " + clip(err.Error(), 200)
		} else {
			r.Restarted = true
			l.markApplied(id)
		}
	}
}

// verdict is the review's earlier answer on this exact content under these rules.
func (l *Learner) verdict(agent string, c loop.Candidate, rules string) (Verdict, bool) {
	var v Verdict
	var at int64
	err := l.o.DB.QueryRow(`SELECT approve, reason, model, at FROM review_verdicts WHERE agent = ? AND kind = ? AND ref = ? AND hash = ? AND rules = ?`,
		agent, c.Kind, c.Ref, c.Hash, rules).Scan(&v.Approve, &v.Reason, &v.Model, &at)
	if err != nil {
		return Verdict{}, false
	}
	v.Rules, v.At = rules, isoMs(at)
	return v, true
}

func (l *Learner) keepVerdict(agent string, c loop.Candidate, v Verdict) {
	if _, err := l.o.DB.Exec(`INSERT OR REPLACE INTO review_verdicts(agent, kind, ref, hash, rules, approve, reason, model, at) VALUES(?,?,?,?,?,?,?,?,?)`,
		agent, c.Kind, c.Ref, c.Hash, v.Rules, v.Approve, v.Reason, v.Model, l.o.Now().UnixMilli()); err != nil {
		log.Printf("learn: keeping a verdict: %v", err)
	}
}

// LatestVerdicts are the review's newest verdict on each of an agent's proposals (kind + "/" + ref),
// with the content hash it was given for: the review page shows it while the proposal is unchanged.
func (l *Learner) LatestVerdicts(agent string) map[string]struct {
	Verdict
	Hash string
} {
	out := map[string]struct {
		Verdict
		Hash string
	}{}
	rows, err := l.o.DB.Query(`SELECT kind, ref, hash, rules, approve, reason, model, at FROM review_verdicts WHERE agent = ? ORDER BY at`, agent)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var kind, ref, hash string
		var v Verdict
		var at int64
		if rows.Scan(&kind, &ref, &hash, &v.Rules, &v.Approve, &v.Reason, &v.Model, &at) == nil {
			v.At = isoMs(at)
			out[kind+"/"+ref] = struct {
				Verdict
				Hash string
			}{v, hash}
		}
	}
	return out
}

// Unapplied are the agents with lessons accepted since they last (re)started through the core.
func (l *Learner) Unapplied() map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]bool{}
	for k := range l.unapplied {
		out[k] = true
	}
	return out
}

func (l *Learner) markUnapplied(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unapplied[id] = true
}

func (l *Learner) markApplied(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.unapplied, id)
}

// writeDigest writes the cycle's summary: counts and review commands, never lesson text.
func (l *Learner) writeDigest(c *Cycle) (string, error) {
	if err := os.MkdirAll(l.o.DigestDir, 0o700); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Learning cycle %d (%s)\n\n", c.ID, c.Trigger)
	fmt.Fprintf(&b, "Started %s, %s.\n\n", c.Started, c.Status)
	fmt.Fprintln(&b, "| agent | nap | naps folded | new lessons | seen again | skill proposals | cron | waiting for review | naps pruned |")
	fmt.Fprintln(&b, "|---|---|---|---|---|---|---|---|---|")
	review, auto := []string{}, []string{}
	for _, r := range c.Runs {
		nap := "yes"
		if !r.Napped {
			nap = "no: " + r.NapNote
		}
		cron := ""
		if r.CronProposal {
			cron = "yes"
		}
		pending := fmt.Sprintf("%d lessons, %d skills", r.PendingLearnings, r.PendingSkills)
		if r.Error != "" {
			pending = "dream failed: " + r.Error
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %d | %s | %s | %d (%.1f MB) |\n", r.Agent, nap, r.Naps, r.NewLearnings, r.Resighted, r.SkillProposals, cron, pending,
			r.PrunedNaps, float64(r.PrunedBytes)/(1<<20))
		if r.AutoAccepted+r.Passed+r.Held > 0 || r.ReviewNote != "" {
			line := fmt.Sprintf("%s: %d approved", r.Agent, r.AutoAccepted)
			if r.AutoAccepted > 0 {
				line += " (" + map[bool]string{true: "restarted, in effect now", false: r.RestartNote}[r.Restarted] + ")"
			}
			line += fmt.Sprintf(", %d passed to a person, %d held by the rules", r.Passed, r.Held)
			if r.ReviewNote != "" {
				line += "; " + r.ReviewNote
			}
			auto = append(auto, line)
		}
		if r.SettingProposals > 0 || r.CronProposal {
			review = append(review, "stormo learn settings "+r.Agent, "stormo learn schedules "+r.Agent)
		}
		if r.PendingLearnings > 0 {
			review = append(review, "stormo learn list "+r.Agent)
		}
		if r.PendingSkills > 0 {
			review = append(review, "stormo learn skills "+r.Agent)
		}
	}
	if len(auto) > 0 {
		fmt.Fprintf(&b, "\nThe review's first pass (core.learning.review: auto; it approves or passes to a person, never rejects):\n\n")
		for _, a := range auto {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	if len(review) > 0 {
		fmt.Fprintf(&b, "\nOpen the review page with `stormo core review`, or from the terminal.\nTo review what waits for you (rejecting and promoting are always yours):\n\n")
		for _, cmd := range review {
			fmt.Fprintf(&b, "    %s\n", cmd)
		}
	} else {
		fmt.Fprintln(&b, "\nNothing waits for review.")
	}
	fmt.Fprintln(&b, "\nThe dream wrote to agents/*/learnings in the working tree only; nothing was committed.")
	name := fmt.Sprintf("%s-cycle-%d.md", time.UnixMilli(msOf(c.Started)).Format("2006-01-02"), c.ID)
	p := filepath.Join(l.o.DigestDir, name)
	return p, os.WriteFile(p, []byte(b.String()), 0o600)
}

// Cycle reads one cycle with its runs.
func (l *Learner) Cycle(id int64) (*Cycle, error) {
	c := &Cycle{ID: id, Runs: []AgentRun{}}
	var started int64
	var finished sql.NullInt64
	if err := l.o.DB.QueryRow(`SELECT trigger, started, finished, status, digest FROM learn_cycles WHERE id = ?`, id).Scan(&c.Trigger, &started, &finished, &c.Status, &c.Digest); err != nil {
		return nil, err
	}
	c.Started, c.Finished = isoMs(started), optIso(finished)
	rows, err := l.o.DB.Query(`SELECT agent, started, finished, napped, nap_note, naps, new_learnings, resighted, skill_proposals, cron_proposal,
		pending_learnings, pending_skills, error, pruned_naps, pruned_bytes, auto_accepted, restarted, restart_note,
		setting_proposals, passed, held, review_note FROM learn_runs WHERE cycle = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r AgentRun
		var s, f int64
		if err := rows.Scan(&r.Agent, &s, &f, &r.Napped, &r.NapNote, &r.Naps, &r.NewLearnings, &r.Resighted, &r.SkillProposals, &r.CronProposal,
			&r.PendingLearnings, &r.PendingSkills, &r.Error, &r.PrunedNaps, &r.PrunedBytes, &r.AutoAccepted, &r.Restarted, &r.RestartNote,
			&r.SettingProposals, &r.Passed, &r.Held, &r.ReviewNote); err != nil {
			return nil, err
		}
		r.Started, r.Finished = isoMs(s), isoMs(f)
		c.Runs = append(c.Runs, r)
	}
	return c, rows.Err()
}

// Cycles are the newest n cycles.
func (l *Learner) Cycles(n int) ([]Cycle, error) {
	if n <= 0 || n > 100 {
		n = 10
	}
	rows, err := l.o.DB.Query(`SELECT id FROM learn_cycles ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []Cycle{}
	for _, id := range ids {
		c, err := l.Cycle(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, nil
}

// catchUp is how long after the scheduled time a missed cycle (the core was down) still starts.
const catchUp = 6 * time.Hour

func (l *Learner) location() *time.Location {
	if tz := l.o.Inst.CoreLearning.Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

// scheduledAt is today's start time in the schedule's zone, or false when there is no schedule.
func (l *Learner) scheduledAt(now time.Time) (time.Time, bool) {
	at := l.o.Inst.CoreLearning.At
	if at == "" {
		return time.Time{}, false
	}
	var h, m int
	if _, err := fmt.Sscanf(at, "%d:%d", &h, &m); err != nil {
		return time.Time{}, false
	}
	n := now.In(l.location())
	return time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, l.location()), true
}

// Due: the schedule's time today has come (within the catch-up window) and no scheduled cycle
// started since.
func (l *Learner) Due(now time.Time) bool {
	at, ok := l.scheduledAt(now)
	if !ok || now.Before(at) || now.Sub(at) > catchUp {
		return false
	}
	var n int
	if err := l.o.DB.QueryRow(`SELECT COUNT(*) FROM learn_cycles WHERE trigger = 'schedule' AND started >= ?`, at.UnixMilli()).Scan(&n); err != nil {
		return false
	}
	return n == 0
}

// Next is when the next scheduled cycle starts, or "".
func (l *Learner) Next(now time.Time) string {
	at, ok := l.scheduledAt(now)
	if !ok {
		return ""
	}
	if !now.Before(at) {
		at = at.AddDate(0, 0, 1)
	}
	return isoMs(at.UnixMilli())
}

// Run starts scheduled cycles when they are due, checking every interval until stop closes.
func (l *Learner) Run(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			l.Stop()
			return
		case <-t.C:
		}
		if l.Due(l.o.Now()) {
			if _, err := l.Start("schedule", l.o.Inst.CoreLearning.Agents, false); err != nil && !errors.Is(err, ErrCycleRunning) {
				log.Printf("learn: scheduled cycle: %v", err)
			}
		}
	}
}

// LearningView is /api/learning: the schedule, the cycle running now and the last ones. Counts only.
type LearningView struct {
	At       string  `json:"at,omitempty"`
	Timezone string  `json:"timezone,omitempty"`
	Next     string  `json:"next,omitempty"`
	Running  *int64  `json:"running"`
	Cycles   []Cycle `json:"cycles"`
}

// View is the learner's state.
func (l *Learner) View(n int) (*LearningView, error) {
	cs, err := l.Cycles(n)
	if err != nil {
		return nil, err
	}
	v := &LearningView{At: l.o.Inst.CoreLearning.At, Timezone: l.location().String(), Next: l.Next(l.o.Now()), Cycles: cs}
	l.mu.Lock()
	if l.running != 0 {
		id := l.running
		v.Running = &id
	}
	l.mu.Unlock()
	return v, nil
}
