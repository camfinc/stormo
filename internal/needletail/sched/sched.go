// Package sched is the runtime's scheduler (docs/needletail.md §4.7; the semantics, shared with
// Hermes, are docs/schedules.md): it runs the jobs of cron/jobs.json on their interval or cron
// expression, through the same turn slots as the API (so they share its cap of two turns), each
// run in its own session with source "cron". A job's monitor script runs first and decides
// whether the agent runs at all; a script's output goes into the prompt, or with no_agent is the
// job's whole output; a "[SILENT]" reply is not delivered. The agent manages its own jobs with
// the cronjob_manage tool (tool.go).
//
// The scheduler is cron/jobs.json's single writer: the tick, the runs and the tool all change it
// under one lock, re-reading it first, and write it atomically.
package sched

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// SourceCron is the source of a job run's session (the hooks' platform).
const SourceCron = "cron"

// Defaults.
const (
	DefaultTick          = 30 * time.Second
	DefaultScriptTimeout = time.Hour // Hermes' default for a schedule's script
	keepOutputs          = 10        // run records kept per job
	maxDiff              = 4000      // characters of the monitor's diff in the prompt
	maxShown             = 8000      // characters of the monitor's output in the prompt
	failureNudgeAt       = 3         // failures in a row that add a "worth a review" line
	maxScriptPrompt      = 16 << 10  // bytes of a pre-run script's output in the prompt
	// MaxGates is how many job scripts (monitors, pre-run scripts, script-only jobs) run at once.
	MaxGates = 2
)

// Turns hands out turn slots (api.Server.Reserve), shared with the API's turns.
type Turns interface {
	Reserve(session string, create wire.Session) (Slot, error)
}

// Slot is a reserved turn (api.Reservation).
type Slot interface {
	Turn(ctx context.Context, input string, opts loop.Options) (*loop.Result, error)
	Release()
}

// Options configure a Scheduler.
type Options struct {
	// Home is the agent's home: cron/jobs.json, scripts/, state/needletail/.
	Home  string
	Turns Turns
	// Skills loads a job's skills as skill_view does (nil: a job's skills are reported missing).
	Skills *skills.Library
	// Tools is every tool's name (the registry's), for a job's enabled_toolsets.
	Tools func() []string
	// Env is what a script runs with (tools.CommandEnv) and how its output is redacted.
	Env tools.Env
	// Deliver sends a run's output (nil: Pending, which records and logs).
	Deliver Deliverer
	// End ends a run's session after its turn (sessions.Store.End).
	End func(session, reason string) error
	// Location is the wall clock cron expressions run on (nil: UTC).
	Location      *time.Location
	Now           func() time.Time
	Log           func(string)
	ScriptTimeout time.Duration
	Tick          time.Duration
}

// Scheduler runs one home's jobs. Safe for concurrent use.
type Scheduler struct {
	o                     Options
	path, state, tmp, out string
	mu                    sync.Mutex        // cron/jobs.json
	runMu                 sync.Mutex        // running, requested, stopping, lastStart, warned
	running               map[string]string // id → name
	requested             map[string]bool   // `run` asked while the job was running
	stopping              bool              // the ticks ended: a run cut short now is interrupted
	lastStart             time.Time
	warned                map[string]string
	wg                    sync.WaitGroup
	runCtx                context.Context
	cancelRuns            context.CancelFunc
	kick                  chan struct{}
	gates                 chan struct{} // the scripts running before (or as) a job: MaxGates
}

// New builds a scheduler; Start runs it.
func New(o Options) *Scheduler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.ScriptTimeout <= 0 {
		o.ScriptTimeout = DefaultScriptTimeout
	}
	if o.Tick <= 0 {
		o.Tick = DefaultTick
	}
	if o.Deliver == nil {
		o.Deliver = Pending{Log: o.Log}
	}
	if o.Env.Redact == nil {
		o.Env.Redact = func(s string) string { return s }
	}
	if o.Env.Home == "" {
		o.Env.Home = o.Home
	}
	s := &Scheduler{o: o, path: filepath.Join(o.Home, "cron", "jobs.json"), state: filepath.Join(o.Home, "state", "needletail"),
		tmp: filepath.Join(o.Home, fsx.TmpDir), running: map[string]string{}, requested: map[string]bool{}, warned: map[string]string{}, kick: make(chan struct{}, 1), gates: make(chan struct{}, MaxGates)}
	s.out = filepath.Join(s.state, "jobs")
	s.runCtx, s.cancelRuns = context.WithCancel(context.Background())
	return s
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.o.Log != nil {
		s.o.Log(s.o.Env.Redact(fmt.Sprintf("cron: "+format, args...)))
	}
}

// warnOnce logs msg for key once until it changes (a busy agent, an override not applied); ""
// clears the key without logging.
func (s *Scheduler) warnOnce(key, msg string) {
	s.runMu.Lock()
	seen := s.warned[key] == msg
	if msg == "" {
		delete(s.warned, key)
	} else {
		s.warned[key] = msg
	}
	s.runMu.Unlock()
	if !seen && msg != "" {
		s.logf("%s", msg)
	}
}

func stamp(t time.Time) string { return t.Format(time.RFC3339) }

func parseStamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// Running is the names of the jobs running now (as Hermes' activity gives them) and when the
// last one started (the API's activity).
func (s *Scheduler) Running() ([]string, time.Time) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	out := make([]string, 0, len(s.running))
	for _, name := range s.running {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, s.lastStart
}

func (s *Scheduler) isRunning(id string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	_, ok := s.running[id]
	return ok
}

// request asks for another run of a running job once this one ends (the tool's run); false when
// the job is not running (its next_run_at alone makes it due).
func (s *Scheduler) request(id string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	_, ok := s.running[id]
	if ok {
		s.requested[id] = true
	}
	return ok
}

// takeRequest reports and clears a run asked for while the job ran.
func (s *Scheduler) takeRequest(id string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	r := s.requested[id]
	delete(s.requested, id)
	return r
}

func (s *Scheduler) setStopping() {
	s.runMu.Lock()
	s.stopping = true
	s.runMu.Unlock()
}

func (s *Scheduler) isStopping() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.stopping
}

// mutate changes cron/jobs.json under the lock: read, change (write when change says so).
func (s *Scheduler) mutate(change func(f *File) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := ReadFile(s.path)
	if err != nil {
		return err
	}
	write, err := change(f)
	if err != nil || !write {
		return err
	}
	return f.Write(s.path, s.tmp)
}

// nextRun is the job's next run after from: an interval after from, or its cron expression's
// next match on the scheduler's wall clock.
func (s *Scheduler) nextRun(j *Job, from time.Time) (time.Time, error) {
	sch := j.Schedule()
	switch sch.Kind {
	case "interval":
		if sch.Minutes <= 0 {
			return time.Time{}, errors.New("schedule: an interval needs minutes")
		}
		if sch.Minutes > MaxIntervalMinutes {
			return time.Time{}, fmt.Errorf("schedule: an interval of %d minutes is longer than %d days", sch.Minutes, MaxIntervalMinutes/1440)
		}
		return from.Add(time.Duration(sch.Minutes) * time.Minute), nil
	case "cron":
		c, err := ParseCron(sch.Expr)
		if err != nil {
			return time.Time{}, err
		}
		n := c.Next(from, s.o.Location)
		if n.IsZero() {
			return time.Time{}, fmt.Errorf("cron %q has no run in the next %d years", sch.Expr, searchYears)
		}
		return n, nil
	}
	return time.Time{}, fmt.Errorf("schedule kind %q is not run here (interval or cron)", sch.Kind)
}

func (s *Scheduler) at(t time.Time) string { return stamp(t.In(s.o.Location)) }

// Boot settles the file at start: a run a crash or restart cut short (running still set) is
// recorded as failed, and a job with no next run gets one.
func (s *Scheduler) Boot() error {
	now := s.o.Now()
	return s.mutate(func(f *File) (bool, error) {
		changed := false
		for _, j := range f.Jobs {
			if j.Bool("running") {
				j.Set("running", false)
				j.Set("last_status", wire.StatusError)
				j.Set("last_error", "interrupted: the runtime stopped before the run finished")
				j.Set("failure_streak", j.Int("failure_streak")+1)
				changed = true
			}
			if s.settleNext(j, now) {
				changed = true
			}
		}
		return changed, nil
	})
}

// settleNext checks an enabled job's schedule, even when it has a next run (a schedule this
// engine cannot run, or one changed under a stored time, must not keep a past time that fires
// every tick): a job whose schedule gives no next run gets none, state "error" and why
// (noNext); one with no (or an unreadable) next run gets its next run after now, and a job in
// state "error" whose schedule works again is scheduled again (Hermes' recoverable error
// jobs). True when it changed the job.
func (s *Scheduler) settleNext(j *Job, now time.Time) bool {
	if !j.Enabled() {
		return false
	}
	if _, err := s.nextRun(j, now); err != nil {
		return s.noNext(j, err.Error())
	}
	if next, ok := parseStamp(j.Str("next_run_at")); ok {
		return s.reanchor(j, next, now)
	}
	base := now
	if last, ok := parseStamp(j.Str("last_run_at")); ok && j.Schedule().Kind == "interval" {
		base = last // an interval keeps its rhythm across a restart
	}
	n, err := s.nextRun(j, base)
	if err != nil {
		return s.noNext(j, err.Error())
	}
	if n.Before(now) {
		n = now // overdue since before the restart: due now
	}
	j.Set("next_run_at", s.at(n))
	if j.Str("state") == "error" {
		j.Set("state", "scheduled")
	}
	return true
}

// reanchor moves a future next run that the job's schedule would not give (the repo changed the
// schedule under it: rehydrate keeps the run state, the repo's definition wins) to the schedule's
// own next time, without firing, as Hermes' _reanchor_stale_cron does for a cron job; a past one is
// left due (a `run`, an overdue time), so it fires once and its finish follows the new schedule.
// A cron time is kept when its own wall clock matches the expression (stamped in another zone:
// Hermes' timezone-migration case); an interval's when it is at most one interval away. True
// when it changed the job.
func (s *Scheduler) reanchor(j *Job, next, now time.Time) bool {
	if !next.After(now) {
		return false
	}
	sch := j.Schedule()
	from := now
	switch sch.Kind {
	case "cron":
		c, err := ParseCron(sch.Expr)
		if err != nil || c.Matches(next.In(s.o.Location)) || c.Matches(next) {
			return false
		}
	case "interval":
		if !next.After(now.Add(time.Duration(sch.Minutes) * time.Minute)) {
			return false
		}
		if last, ok := parseStamp(j.Str("last_run_at")); ok && last.Before(now) {
			from = last
		}
	default:
		return false
	}
	n, err := s.nextRun(j, from)
	if err != nil {
		return false
	}
	if n.Before(now) {
		n = now
	}
	s.logf("job %s: next run %s does not follow its schedule (%s): re-anchored to %s", j.ID(), s.at(next), orDefault(sch.Display, sch.Expr), s.at(n))
	j.Set("next_run_at", s.at(n))
	return true
}

// noNext records that a job has no next run: next_run_at null, state "error", last_error why
// (as Hermes leaves a recurring job whose next run it cannot compute: enabled, so a fixed
// schedule recovers it), and last_status error unless a run just recorded its own. False when
// the job already said so (the file is not rewritten every tick).
func (s *Scheduler) noNext(j *Job, why string) bool {
	if j.Str("state") == "error" && j.Str("next_run_at") == "" && j.Str("last_error") == why {
		return false
	}
	j.Set("next_run_at", nil)
	j.Set("state", "error")
	if !j.Bool("running") {
		j.Set("last_status", wire.StatusError)
	}
	j.Set("last_error", why)
	s.logf("job %s has no next run: %s", j.ID(), why)
	return true
}

// advanceNext moves a job's next run to its schedule's next time after from, which must lie
// after now; a schedule that gives none (or a time not after now) leaves it with no next run
// (noNext) and false.
func (s *Scheduler) advanceNext(j *Job, from, now time.Time) bool {
	n, err := s.nextRun(j, from)
	if err == nil && !n.After(now) {
		err = fmt.Errorf("schedule: the next run (%s) is not after now", s.at(n))
	}
	if err != nil {
		s.noNext(j, err.Error())
		return false
	}
	j.Set("next_run_at", s.at(n))
	return true
}

// SetTurns gives the scheduler its turn slots (the API server's); before Start.
func (s *Scheduler) SetTurns(t Turns) { s.o.Turns = t }

// Start ticks until ctx ends: at once, then every Tick, and when the tool asks for a run.
func (s *Scheduler) Start(ctx context.Context) {
	if err := s.Boot(); err != nil {
		s.logf("cron/jobs.json: %v", err)
	}
	go func() {
		t := time.NewTicker(s.o.Tick)
		defer t.Stop()
		for {
			s.Tick()
			select {
			case <-ctx.Done():
				s.setStopping()
				return
			case <-t.C:
			case <-s.kick:
			}
		}
	}()
}

// Shutdown waits for running jobs until ctx ends, then cancels them and waits a moment more.
func (s *Scheduler) Shutdown(ctx context.Context) {
	s.setStopping()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	s.cancelRuns()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		s.logf("shutdown: jobs still running after cancellation; not waiting for them")
	}
}

// Wait waits for the runs started so far (tests).
func (s *Scheduler) Wait() { s.wg.Wait() }

// Tick starts every due job.
func (s *Scheduler) Tick() {
	now := s.o.Now()
	due := []string{}
	err := s.mutate(func(f *File) (bool, error) {
		changed := false
		for _, j := range f.Jobs {
			id := j.ID()
			if id == "" || !j.Enabled() || s.isRunning(id) {
				continue
			}
			if s.settleNext(j, now) {
				changed = true
				continue
			}
			if next, ok := parseStamp(j.Str("next_run_at")); ok && !now.Before(next) {
				due = append(due, id)
			}
		}
		return changed, nil
	})
	if err != nil {
		s.logf("cron/jobs.json: %v", err)
		return
	}
	for _, id := range due {
		s.start(id, now)
	}
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func safeID(id string) string { return unsafeID.ReplaceAllString(id, "_") }

// start reserves a turn for an agent job, marks the job running and advances its next run (so a
// crash mid-run never runs it twice), then runs it in the background. A busy agent leaves the
// job due for the next tick.
func (s *Scheduler) start(id string, now time.Time) {
	var job *Job
	_ = s.mutate(func(f *File) (bool, error) {
		if j := f.Find(id); j != nil {
			job = &Job{keys: append([]string{}, j.keys...), raw: map[string]json.RawMessage{}}
			for k, v := range j.raw {
				job.raw[k] = v
			}
		}
		return false, nil
	})
	if job == nil {
		return
	}
	session := fmt.Sprintf("cron-%s-%s", safeID(id), now.UTC().Format("20060102T150405"))
	agent, gated := !job.Bool("no_agent"), hasGate(job)
	if agent && s.o.Turns == nil {
		s.warnOnce("turns:"+id, fmt.Sprintf("job %s: no agent to run it", id))
		return
	}
	var slot Slot
	if agent && !gated {
		r, err := s.o.Turns.Reserve(session, wire.Session{Source: SourceCron, Title: job.Name()})
		if err != nil {
			s.warnOnce("busy:"+id, fmt.Sprintf("job %s is due but not started: %v; next tick tries again", id, err))
			return
		}
		s.warnOnce("busy:"+id, "")
		slot = r
	}
	started := false
	_ = s.mutate(func(f *File) (bool, error) {
		j := f.Find(id)
		if j == nil || !j.Enabled() {
			return false, nil
		}
		if !s.advanceNext(j, now, now) {
			return true, nil // no next run: it does not run either
		}
		started = true
		j.Set("running", true)
		return true, nil
	})
	if !started {
		if slot != nil {
			slot.Release()
		}
		return
	}
	s.runMu.Lock()
	s.running[id] = job.Name()
	s.lastStart = now
	s.runMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.runMu.Lock()
			delete(s.running, id)
			s.runMu.Unlock()
		}()
		// A job with a script (a monitor, a pre-run script, a script-only job) runs it under
		// the gates' own cap, holding no turn slot: a script may take an hour, and the agent's
		// two turns are for turns. Its turn slot is taken once the gate says the agent runs
		// (execute).
		release := func() {}
		if gated {
			select {
			case s.gates <- struct{}{}:
				var once sync.Once
				release = func() { once.Do(func() { <-s.gates }) }
			case <-s.runCtx.Done():
				s.finish(s.runCtx, job, interrupted(""))
				return
			}
		}
		o := s.execute(s.runCtx, job, &slot, session, release)
		release()
		if slot != nil {
			slot.Release()
		}
		s.finish(s.runCtx, job, o)
	}()
}

// outcome is one run's result.
type outcome struct {
	status  string // wire.Status*
	err     string // why it failed
	text    string // what is delivered (the reply, the script's output, or the failure notice)
	pause   string // the job cannot run: pause it with this reason
	session string
	doc     string // the run's record
}

func (o outcome) failed() bool { return o.status == wire.StatusError }

// Outcomes never written as last_status: a run the runtime's shutdown cut short, and a gated
// job whose gate said to run the agent when no turn slot was free.
const (
	statusInterrupted = "interrupted"
	statusBusy        = "busy"
)

// hasGate is a job that runs a script before (or instead of) a turn.
func hasGate(j *Job) bool {
	return strings.TrimSpace(j.Str("script")) != "" || !j.Bool("no_agent") && strings.TrimSpace(j.Str("monitor_script")) != ""
}

// interrupted is the outcome of a run the shutdown cut short.
func interrupted(session string) outcome {
	return outcome{status: statusInterrupted, err: "interrupted: the runtime stopped during the run", session: session}
}

// execute runs one job: its script alone (no_agent), or its monitor, script and turn. A gated job
// (hasGate) comes with no slot: once its gate says the agent runs, it releases the gates' cap
// and takes a turn slot into *slot (the caller releases it); none free is statusBusy, and the
// monitor's new output is stored only once the slot is held, so the next tick sees the change
// again.
func (s *Scheduler) execute(ctx context.Context, j *Job, slot *Slot, session string, release func()) (o outcome) {
	name, id := j.Name(), j.ID()
	defer func() {
		if p := recover(); p != nil {
			o = outcome{status: wire.StatusError, err: fmt.Sprintf("the run crashed: %v", p)}
		}
		if o.failed() && o.text == "" {
			o.text = fmt.Sprintf("⚠ Job '%s' failed\n\n%s", name, o.err)
		}
	}()
	if j.Bool("no_agent") {
		script := strings.TrimSpace(j.Str("script"))
		if script == "" {
			return outcome{status: wire.StatusError, err: "a no_agent job needs a script", pause: "a no_agent job needs a script"}
		}
		ok, out := s.runScript(ctx, script)
		switch {
		case !ok && s.isStopping():
			return interrupted("")
		case !ok:
			return outcome{status: wire.StatusError, err: out, text: fmt.Sprintf("⚠ Job '%s' script failed\n\n%s", name, out), doc: out}
		case !wakes(out) || out == "":
			return outcome{status: wire.StatusSilent, doc: out}
		}
		return outcome{status: wire.StatusOK, text: out, doc: out}
	}
	prompt := j.Str("prompt")
	if strings.TrimSpace(prompt) == "" && len(j.Skills()) == 0 && strings.TrimSpace(j.Str("script")) == "" {
		return outcome{status: wire.StatusError, err: "the job has no prompt, skills or script", pause: "the job has no prompt, skills or script"}
	}
	runContext, commit := "", func() {}
	if mon := strings.TrimSpace(j.Str("monitor_script")); mon != "" {
		ok, out := s.runScript(ctx, mon)
		if !ok && s.isStopping() {
			return interrupted("")
		}
		if !ok {
			return outcome{status: wire.StatusError, err: "monitor: " + out, text: fmt.Sprintf("⚠ Job '%s' monitor failed\n\n%s", name, out)}
		}
		block, changed, store := s.monitorGate(j, out)
		if !changed {
			return outcome{status: wire.StatusNoChange}
		}
		runContext, commit = block, store
	}
	scriptBlock := ""
	if sc := strings.TrimSpace(j.Str("script")); sc != "" {
		ok, out := s.runScript(ctx, sc)
		switch {
		case !ok && s.isStopping():
			return interrupted("")
		case ok && !wakes(out):
			return outcome{status: wire.StatusSilent, doc: "the script's wake gate said not to wake the agent"}
		case ok && out == "":
			return outcome{status: wire.StatusSilent, doc: "the script printed nothing"}
		case ok:
			if len(out) > maxScriptPrompt {
				out = strings.ToValidUTF8(out[:maxScriptPrompt], "") + fmt.Sprintf("\n... [script output truncated: %d bytes, the first %d shown]", len(out), maxScriptPrompt)
			}
			scriptBlock = fence("Script Output", "The following data was collected by the job's script before this run. Use it as context.", out)
		default:
			scriptBlock = fence("Script Error", "The job's script failed. Report this.", out)
		}
	}
	release()
	if *slot == nil {
		r, err := s.o.Turns.Reserve(session, wire.Session{Source: SourceCron, Title: name})
		if err != nil {
			return outcome{status: statusBusy, err: err.Error()}
		}
		*slot = r
	}
	s.warnOnce("busy:"+id, "")
	commit()
	if j.Str("model") != "" || j.Str("provider") != "" {
		s.warnOnce("model:"+id, fmt.Sprintf("job %s pins model %q (provider %q): not applied, it runs on the agent's model", id, j.Str("model"), j.Str("provider")))
	}
	input := s.buildPrompt(j, session, prompt, scriptBlock, runContext)
	res, err := (*slot).Turn(ctx, input, loop.Options{Tools: s.toolsFor(j)})
	if res != nil {
		session = res.Session
		if s.o.End != nil {
			_ = s.o.End(res.Session, "cron_complete")
		}
	}
	if err != nil {
		if ctx.Err() != nil || s.isStopping() {
			return interrupted(session)
		}
		return outcome{status: wire.StatusError, err: s.o.Env.Redact(err.Error()), session: session}
	}
	reply := strings.TrimSpace(res.Text)
	doc := res.Text
	if lines := strings.Split(reply, "\n"); strings.TrimRight(lines[0], " \t\r") == "[CRON_FAILURE]" {
		why := strings.TrimSpace(strings.Join(lines[1:], "\n"))
		if why == "" {
			why = "the agent reported the run as failed"
		}
		return outcome{status: wire.StatusError, err: why, session: session, doc: doc}
	}
	switch {
	case reply == "":
		return outcome{status: wire.StatusError, err: "the agent's reply was empty", session: session}
	case Silent(reply):
		return outcome{status: wire.StatusSilent, session: session, doc: doc}
	}
	return outcome{status: wire.StatusOK, text: res.Text, session: session, doc: doc}
}

// fence is a "## heading" data block before the prompt.
func fence(heading, intro, body string) string {
	return fmt.Sprintf("## %s\n%s\n\n```\n%s\n```\n\n", heading, intro, body)
}

// wakes is the script's wake gate: false only when its last non-empty line is JSON
// {"wakeAgent": false}.
func wakes(out string) bool {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		var gate map[string]any
		if json.Unmarshal([]byte(l), &gate) != nil {
			return true
		}
		return gate["wakeAgent"] != false
	}
	return true
}

var silentTokens = map[string]bool{"[silent]": true, "silent": true, "no_reply": true, "no reply": true, "[no_reply]": true}

// Silent is a reply that asks for no delivery: "[SILENT]" (or SILENT, NO_REPLY, NO REPLY) as the
// whole reply, or as its own first or last line; case and surrounding space do not matter. The
// token inside a sentence is content.
func Silent(reply string) bool {
	r := strings.TrimSpace(reply)
	if silentTokens[strings.ToLower(r)] {
		return true
	}
	lines := strings.Split(r, "\n")
	first, last := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[len(lines)-1])
	return silentTokens[strings.ToLower(first)] || silentTokens[strings.ToLower(last)]
}

// cronHint opens every agent run's prompt: what a scheduled run is and how to stay silent.
const cronHint = "[Scheduled job: you are running as a scheduled job, not in a conversation. Your final reply is " +
	"delivered as the job's output: do not send it yourself. If there is genuinely nothing new to report, reply " +
	"with exactly \"[SILENT]\" and nothing else; it is a control token, so never translate it or add to it. If " +
	"the run cannot do its task and must count as failed, put [CRON_FAILURE] alone on the first line and say why " +
	"below it. This is one run of an existing job: do not create or change scheduled jobs because the task " +
	"below mentions a schedule.]\n\n"

// buildPrompt is the run's input: the hint, the script's block, the prompt and the monitor's run
// context, after the job's skills (each loaded as skill_view does, for this session).
func (s *Scheduler) buildPrompt(j *Job, session, prompt, scriptBlock, runContext string) string {
	p := prompt
	if runContext != "" {
		p += "\n\n## Run Context\n" + runContext
	}
	p = cronHint + scriptBlock + p
	names := j.Skills()
	if len(names) == 0 {
		return p
	}
	parts, missing := []string{}, []string{}
	for _, n := range names {
		if s.o.Skills == nil {
			missing = append(missing, n)
			continue
		}
		v, err := s.o.Skills.View(session, n, "")
		if err != nil {
			missing = append(missing, n)
			s.logf("job %s: skill %s: %v", j.ID(), n, err)
			continue
		}
		parts = append(parts, fmt.Sprintf("[The job loads the %q skill: follow its instructions. Its SKILL.md (directory %s) follows.]\n\n%s", v.Name, v.SkillDir, strings.TrimSpace(v.Content)))
	}
	if len(missing) > 0 {
		parts = append([]string{fmt.Sprintf("[The job lists skills that were not found and are skipped: %s. Start your reply with a one-line notice of it.]", strings.Join(missing, ", "))}, parts...)
	}
	return strings.Join(append(parts, p), "\n\n")
}

// monitorGate compares the monitor's output with the one that last woke the agent (its exact
// bytes' hash, Hermes' monitor_state). Unchanged: false. Changed, or the first run: the block
// for the prompt (the diff and the current output), and store, which keeps the new output and
// hash; execute calls it once the run holds its turn slot and before the turn, so a failed run
// does not alert on the same output again, and a run the busy agent could not take does.
func (s *Scheduler) monitorGate(j *Job, out string) (string, bool, func()) {
	sum := sha256.Sum256([]byte(out))
	hash := hex.EncodeToString(sum[:])
	prev, had := j.monitor()
	if had && prev.LastOutputHash == hash {
		return "", false, nil
	}
	snap := filepath.Join(s.state, "monitor", safeID(j.ID())+".out")
	shown := out
	if len([]rune(shown)) > maxShown {
		shown = string([]rune(shown)[:maxShown]) + "\n... [output truncated]"
	}
	current := "### Current output\n\n```\n" + shown + "\n```"
	block := "## Monitor Baseline (first run)\n\nThis is the first observation of the monitored source: there is no previous output to compare with.\n\n" + current
	if had {
		old, _ := os.ReadFile(snap)
		diff := unifiedDiff(string(old), out)
		if len([]rune(diff)) > maxDiff {
			diff = string([]rune(diff)[:maxDiff]) + "\n... [diff truncated]"
		}
		block = "## MONITOR CHANGE DETECTED\n\nThe monitored source's output changed since the last run.\n\n### Diff (previous → current)\n\n```diff\n" + diff + "\n```\n\n" + current
	}
	id := j.ID()
	return block, true, func() {
		if err := fsx.WriteAtomicIn(snap, s.tmp, []byte(out), 0o600); err != nil {
			s.logf("job %s: monitor snapshot: %v", id, err)
		}
		state := monitorState{LastOutputHash: hash, LastChangedAt: s.at(s.o.Now())}
		if err := s.mutate(func(f *File) (bool, error) {
			if live := f.Find(id); live != nil {
				live.Set("monitor_state", state)
				return true, nil
			}
			return false, nil
		}); err != nil {
			s.logf("job %s: monitor state: %v", id, err)
		}
	}
}

// finish delivers a run's output and records it on the job: last run, status, error, failure
// streak (one more on a failure, reset by any success), runs completed, and the next run
// (re-anchored on the end of the run; none once repeat.times runs are done).
func (s *Scheduler) finish(ctx context.Context, j *Job, o outcome) {
	id, now := j.ID(), s.o.Now()
	if o.status == statusBusy {
		// the gate ran but no turn was free: due again as it was, nothing recorded
		s.warnOnce("busy:"+id, fmt.Sprintf("job %s: its gate wants a turn but none is free (%s); next tick runs it again", id, o.err))
		if err := s.mutate(func(f *File) (bool, error) {
			if live := f.Find(id); live != nil {
				live.Set("running", false)
				due := j.Str("next_run_at")
				if _, ok := parseStamp(due); !ok {
					due = s.at(now)
				}
				live.Set("next_run_at", due)
				return true, nil
			}
			return false, nil
		}); err != nil {
			s.logf("job %s: %v", id, err)
		}
		return
	}
	if o.status == statusInterrupted {
		s.takeRequest(id)
		s.record(j, o, now)
		if err := s.mutate(func(f *File) (bool, error) {
			if live := f.Find(id); live != nil {
				live.Set("running", false)
				live.Set("next_run_at", s.at(now)) // due again at the next start
				return true, nil
			}
			return false, nil
		}); err != nil {
			s.logf("job %s: %v", id, err)
		}
		s.logf("job %s: interrupted by the shutdown; due again at the next start", id)
		return
	}
	requested := s.takeRequest(id)
	streak := j.Int("failure_streak")
	if o.failed() {
		streak++
		if streak >= failureNudgeAt {
			o.text += fmt.Sprintf("\n\nThis job has failed %d runs in a row: worth a review. Fix its prompt or script, or pause it (cronjob_manage, action pause).", streak)
		}
	}
	if o.pause != "" {
		o.text = fmt.Sprintf("⚠ Job '%s' was paused: %s", j.Name(), o.pause)
	}
	deliveryErr := ""
	if o.text != "" && (o.status == wire.StatusOK || o.failed()) {
		deliveryErr = s.deliver(ctx, j, o)
	}
	s.record(j, o, now)
	err := s.mutate(func(f *File) (bool, error) {
		live := f.Find(id)
		if live == nil {
			return false, nil // removed while it ran
		}
		live.Set("last_run_at", s.at(now))
		status := o.status
		if !o.failed() && deliveryErr != "" {
			status = "delivery_failed" // the run worked: the streak stands
		}
		live.Set("last_status", status)
		if o.failed() {
			live.Set("last_error", o.err)
			live.Set("failure_streak", streak)
		} else {
			live.Set("last_error", nil)
			live.Set("failure_streak", 0)
		}
		if deliveryErr != "" {
			live.Set("last_delivery_error", deliveryErr)
		} else {
			live.Set("last_delivery_error", nil)
		}
		times, completed := live.Repeat()
		completed++
		live.SetCompleted(completed)
		switch {
		case o.pause != "":
			live.Set("enabled", false)
			live.Set("state", "paused")
			live.Set("paused_at", s.at(now))
			live.Set("paused_reason", "Auto-paused by the scheduler: "+o.pause)
		case times > 0 && completed >= times:
			live.Set("enabled", false)
			live.Set("state", "completed")
			live.Set("next_run_at", nil)
		default:
			live.Set("running", true) // the run's own status stands (noNext)
			if s.advanceNext(live, now, now) && requested {
				live.Set("next_run_at", s.at(now)) // `run` was asked while it ran
			}
		}
		live.Set("running", false)
		return true, nil
	})
	if err != nil {
		s.logf("job %s: %v", id, err)
	}
	line := fmt.Sprintf("job %s: %s", id, statusLine(o, deliveryErr))
	s.logf("%s", line)
}

func statusLine(o outcome, deliveryErr string) string {
	s := o.status
	if o.err != "" {
		s += " (" + clip(o.err, 200) + ")"
	}
	if deliveryErr != "" {
		s += "; delivery: " + clip(deliveryErr, 200)
	}
	if o.session != "" {
		s += " session=" + o.session
	}
	return s
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// deliver sends the output to the lane's targets: failure_deliver for a failure when the job has
// one, else deliver. "" when every target took it.
func (s *Scheduler) deliver(ctx context.Context, j *Job, o outcome) string {
	lane := j.Str("deliver")
	if o.failed() {
		if fd := strings.TrimSpace(j.Str("failure_deliver")); fd != "" {
			lane = fd
		}
	}
	var origin json.RawMessage
	if j.Has("origin") {
		origin = j.raw["origin"]
	}
	targets, err := Targets(lane, origin)
	errs := []string{}
	if err != nil {
		errs = append(errs, err.Error())
	}
	for _, t := range targets {
		d := Delivery{JobID: j.ID(), JobName: j.Name(), Target: t, Text: o.text, Failure: o.failed(), Session: o.session}
		if err := s.o.Deliver.Deliver(ctx, d); err != nil {
			errs = append(errs, t.String()+": "+err.Error())
		}
	}
	return s.o.Env.Redact(strings.Join(errs, "; "))
}

// record keeps the run's output in state/needletail/jobs/<id>/<time>.md (the newest
// keepOutputs per job): what a "local" job delivers to, and what any run left. A run that had
// nothing to do (no_change) leaves none.
func (s *Scheduler) record(j *Job, o outcome, at time.Time) {
	if o.status == wire.StatusNoChange {
		return
	}
	dir := filepath.Join(s.out, safeID(j.ID()))
	var b strings.Builder
	fmt.Fprintf(&b, "# Job: %s\n\n- id: %s\n- run: %s\n- status: %s\n", j.Name(), j.ID(), s.at(at), o.status)
	if o.session != "" {
		fmt.Fprintf(&b, "- session: %s\n", o.session)
	}
	if o.err != "" {
		fmt.Fprintf(&b, "- error: %s\n", o.err)
	}
	body := o.doc
	if body == "" {
		body = o.text
	}
	if body != "" {
		fmt.Fprintf(&b, "\n## Response\n\n%s\n", body)
	}
	name := at.UTC().Format("20060102T150405.000") + ".md"
	if err := fsx.WriteAtomicIn(filepath.Join(dir, name), s.tmp, []byte(s.o.Env.Redact(b.String())), 0o600); err != nil {
		s.logf("job %s: run record: %v", j.ID(), err)
		return
	}
	entries, _ := os.ReadDir(dir)
	names := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keepOutputs {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// newID is a job id: 12 hex digits, like Hermes'.
func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
