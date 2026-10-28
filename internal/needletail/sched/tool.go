package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// The cronjob_manage tool: Hermes' name and arguments (the ones that mean something here), so a
// prompt or skill that schedules work reads the same on either engine. A job the agent makes is
// a job like the repo's: after a restart, rehydrate keeps it with its run state, while a job
// agent.yaml defines comes back with the repo's definition (the adapter's MergeSchedules); the
// dream proposes the agent's jobs (and its changes to the repo's) for review.

const toolDescription = `Manage scheduled jobs: action 'create' schedules a job from a prompt and/or skills; 'list' shows the jobs; 'update', 'pause', 'resume', 'remove' and 'run' act on one by job_id (list first: never guess an id). 'retry_delivery' retries the retained report without reanalysis (optional deliver explicitly replaces unsent destinations); 'discard_delivery' explicitly discards it. Pending reports block new runs. 'run' makes a job due now (it starts within the next tick; its output is delivered as the job's deliver says, it does not come back here).

Jobs run in a fresh session with no chat context, so prompts must be self-contained, and the run's FINAL REPLY is what gets delivered; a run cannot ask questions. Prefer updating a job over creating a near-duplicate.`

const toolSchema = `{"type":"object","properties":{
"action":{"type":"string","description":"One of: create, list, update, pause, resume, remove, run, retry_delivery, discard_delivery. create needs schedule, and prompt unless no_agent."},
"job_id":{"type":"string","description":"Required for update, pause, resume, remove and run."},
"prompt":{"type":"string","description":"create/update: the job's whole, self-contained prompt (paired with any skills as the task), at most 8 KB."},
"schedule":{"type":"string","description":"Required for create. A recurring interval ('30m', 'every 2h', 'every hour'), a day and time ('every monday 9am', 'weekdays at 9am', 'every day at 14:00') or a 5-field cron expression ('0 9 * * 1-5'). One-time schedules are not jobs (follow_up does something once, later); nothing runs more often than every 5 minutes."},
"name":{"type":"string","description":"Optional human-friendly name."},
"repeat":{"type":"integer","description":"Optional number of runs, after which the job completes. Omit for forever."},
"deliver":{"type":"string","description":"Where the output is posted. Omit for the chat the job was made from (else the home channel); 'local' (record only), 'slack' (the home channel), 'slack:<channel>' or 'slack:<channel>:<thread>'. Comma-combine targets. On update, '' resets."},
"failure_deliver":{"type":"string","description":"Optional target for failure notices only (same grammar); 'local' keeps them out of chat. Omit: failures follow deliver. On update, '' clears."},
"skills":{"type":"array","items":{"type":"string"},"description":"Optional skills loaded before the prompt, in order. On update, [] clears."},
"script":{"type":"string","description":"Optional script in the home's scripts/ (.sh/.bash with bash, else python3) run each time first; its stdout goes into the prompt (with no_agent it is the job). On update, '' clears."},
"monitor":{"type":"string","description":"Optional change detector: a script in scripts/ run first each time; output identical to the last run's skips the run entirely, changed output runs it with the diff in the prompt. The first run always runs. Its output must be deterministic (no timestamps). Not with no_agent. On update, '' clears."},
"no_agent":{"type":"boolean","description":"True: no model; the script runs on schedule and its stdout is delivered as it is (empty stdout sends nothing)."},
"enabled_toolsets":{"type":"array","items":{"type":"string"},"description":"Optional toolsets the run may use (terminal, file, skills, memory). Omit for every tool. On update, [] clears."},
"paused":{"type":"boolean","description":"create only: make the job paused (resume schedules it)."},
"paused_reason":{"type":"string","description":"create only, with paused: why."},
"reason":{"type":"string","description":"pause: why."},
"include_disabled":{"type":"boolean","description":"list: include paused and completed jobs (default true)."}
},"required":["action"]}`

type cronArgs struct {
	// expectReport, set by the runtime (never the model) on an approved retry_delivery or
	// discard_delivery, is the hash of the kept report the card showed (ReportHash).
	expectReport    string
	Action          string    `json:"action"`
	JobID           string    `json:"job_id"`
	Prompt          *string   `json:"prompt"`
	Schedule        *string   `json:"schedule"`
	Name            *string   `json:"name"`
	Repeat          *int      `json:"repeat"`
	Deliver         *string   `json:"deliver"`
	FailureDeliver  *string   `json:"failure_deliver"`
	Skills          *[]string `json:"skills"`
	Skill           *string   `json:"skill"`
	Script          *string   `json:"script"`
	Monitor         *string   `json:"monitor"`
	MonitorScript   *string   `json:"monitor_script"`
	NoAgent         *bool     `json:"no_agent"`
	EnabledToolsets *[]string `json:"enabled_toolsets"`
	Paused          bool      `json:"paused"`
	PausedReason    string    `json:"paused_reason"`
	Reason          string    `json:"reason"`
	IncludeDisabled *bool     `json:"include_disabled"`
}

// Tool is cronjob_manage, acting on this scheduler's jobs. Origin tells it the chat a call
// comes from (for a job's "origin" delivery); nil: none.
func (s *Scheduler) Tool(origin func(session string) json.RawMessage) tools.Tool {
	return tools.Tool{Name: CronJobTool, Description: toolDescription, Schema: json.RawMessage(toolSchema),
		Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (any, error) {
			var a cronArgs
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("bad arguments: %v", err)
			}
			if a.Monitor == nil {
				a.Monitor = a.MonitorScript
			}
			if a.Skills == nil && a.Skill != nil {
				l := []string{*a.Skill}
				a.Skills = &l
			}
			var from json.RawMessage
			if origin != nil && env != nil {
				from = origin(env.Session)
			}
			// a run that may not obey what it handles, or a conversation that took such words in,
			// makes no job and changes no job's content: the job would run them later as the
			// agent's own (docs/needletail.md §7)
			why := messaging.UntrustedWhy(ctx)
			if why == "" && env != nil && env.Untrusted != nil {
				why = env.Untrusted(ctx, env.Session)
			}
			a.expectReport = messaging.Bound(ctx, "report_sha256")
			return s.manage(a, from, why)
		}}
}

// Bounds on the agent's own jobs (needletail's; Hermes has none): how many it may make, how long
// a prompt may be and how often a job may run.
const (
	MaxAgentJobs   = 20
	MaxPromptBytes = 8 << 10
	MinEvery       = 5 * time.Minute
)

// agentMade is a job the agent made at runtime: the tool stamps created_at, the repo's render
// leaves it null (the merge keeps run state, so a repo job never gains one; one the agent made
// and a reviewer promoted into agent.yaml keeps its own, which is why trusted also reads
// RepoJobs).
func agentMade(j *Job) bool { return j.Str("created_at") != "" }

// checkPrompt refuses a prompt over MaxPromptBytes or one the scan blocks.
func checkPrompt(p string) error {
	if len(p) > MaxPromptBytes {
		return fmt.Errorf("the prompt is %d bytes: at most %d (put long instructions in a skill and name it in skills)", len(p), MaxPromptBytes)
	}
	if why := ScanPrompt(p); why != "" {
		return errors.New(why)
	}
	return nil
}

// checkEvery refuses a schedule that runs more often than every MinEvery.
func (s *Scheduler) checkEvery(sch wire.JobSchedule) error {
	gap := time.Duration(sch.Minutes) * time.Minute
	if sch.Kind == "cron" {
		c, err := ParseCron(sch.Expr)
		if err != nil {
			return err
		}
		gap = c.MinGap(s.o.Now(), s.o.Location)
		if gap == 0 {
			return nil
		}
	}
	if gap < MinEvery {
		return fmt.Errorf("schedule %q runs every %s: jobs run at most every %s (for something once, sooner, use %s)", orDefault(sch.Display, sch.Expr), gap, MinEvery, FollowUpTool)
	}
	return nil
}

// jobView is a job as list shows it.
type jobView struct {
	DeliveryHold      bool           `json:"delivery_hold,omitempty"`
	JobID             string         `json:"job_id"`
	Name              string         `json:"name"`
	Schedule          string         `json:"schedule"`
	Enabled           bool           `json:"enabled"`
	State             string         `json:"state"`
	Prompt            string         `json:"prompt,omitempty"`
	Skills            []string       `json:"skills,omitempty"`
	Script            string         `json:"script,omitempty"`
	Monitor           string         `json:"monitor,omitempty"`
	NoAgent           bool           `json:"no_agent,omitempty"`
	Deliver           string         `json:"deliver,omitempty"`
	FailureDeliver    string         `json:"failure_deliver,omitempty"`
	Toolsets          []string       `json:"enabled_toolsets,omitempty"`
	Repeat            string         `json:"repeat,omitempty"`
	NextRunAt         string         `json:"next_run_at,omitempty"`
	LastRunAt         string         `json:"last_run_at,omitempty"`
	LastStatus        string         `json:"last_status,omitempty"`
	DeliveryRecovery  map[string]any `json:"delivery_recovery,omitempty"`
	LastDeliveryError string         `json:"last_delivery_error,omitempty"`
	LastError         string         `json:"last_error,omitempty"`
	FailureStreak     int            `json:"failure_streak,omitempty"`
	Running           bool           `json:"running,omitempty"`
	PausedReason      string         `json:"paused_reason,omitempty"`
}

func view(j *Job) jobView {
	v := jobView{DeliveryHold: j.Bool("delivery_hold"), JobID: j.ID(), Name: j.Name(), Schedule: j.Schedule().Display, Enabled: j.Enabled(), State: j.Str("state"),
		Prompt: clip(j.Str("prompt"), 300), Skills: j.Skills(), Script: j.Str("script"), Monitor: j.Str("monitor_script"),
		NoAgent: j.Bool("no_agent"), Deliver: j.Str("deliver"), FailureDeliver: j.Str("failure_deliver"), Toolsets: j.Strings("enabled_toolsets"),
		NextRunAt: j.Str("next_run_at"), LastRunAt: j.Str("last_run_at"), LastStatus: j.Str("last_status"), LastError: clip(j.Str("last_error"), 300),
		LastDeliveryError: clip(j.Str("last_delivery_error"), 300), FailureStreak: j.Int("failure_streak"), Running: j.Bool("running"), PausedReason: j.Str("paused_reason")}
	if v.Schedule == "" {
		v.Schedule = j.Schedule().Expr
	}
	if times, done := j.Repeat(); times > 0 {
		v.Repeat = fmt.Sprintf("%d of %d runs done", done, times)
	}
	return v
}

func (s *Scheduler) manage(a cronArgs, origin json.RawMessage, untrusted string) (any, error) {
	action := strings.ToLower(strings.TrimSpace(a.Action))
	// Information flow (docs/needletail.md §7): from a tainted run, making, changing or removing a
	// job (its prompt, script, skills, targets, schedule), and repairing or dropping a held report,
	// need a person's approval; list, run, pause and resume do not.
	if untrusted != "" {
		switch action {
		case "create", "update", "remove", "retry_delivery", "discard_delivery":
			what := "a scheduled job (cronjob_manage " + action + ")"
			if a.JobID != "" {
				what = "a change to the scheduled job " + a.JobID + " (cronjob_manage " + action + ")"
			}
			return nil, messaging.NeedsApproval(what, untrusted)
		}
	}
	switch action {
	case "retry_delivery", "discard_delivery":
		a.Action = action
		result, err := s.deliveryAction(a, untrusted)
		if err == nil && action == "discard_delivery" {
			err = s.mutate(func(f *File) (bool, error) {
				j := f.Find(a.JobID)
				if j == nil {
					return false, nil
				}
				j.Set("delivery_hold", false)
				j.Set("last_status", "delivery_discarded")
				j.Set("last_delivery_error", "Retained report explicitly discarded; no delivery receipt.")
				return true, nil
			})
		}
		return result, err
	case "list":
		recovery, recoveryErr := s.deliveryViews()
		if recoveryErr != nil {
			return nil, recoveryErr
		}
		var out []jobView
		all := a.IncludeDisabled == nil || *a.IncludeDisabled
		err := s.mutate(func(f *File) (bool, error) {
			for _, j := range f.Jobs {
				if all || j.Enabled() {
					v := view(j)
					v.DeliveryRecovery = recovery[j.ID()]
					out = append(out, v)
				}
			}
			return false, nil
		})
		if out == nil {
			out = []jobView{}
		}
		return map[string]any{"jobs": out, "count": len(out)}, err
	case "create":
		if untrusted != "" {
			return nil, messaging.Deferred("a scheduled job", untrusted)
		}
		return s.create(a, origin)
	case "update", "pause", "resume", "remove", "run":
		if fields := repoFixed(a); action == "update" && untrusted != "" && len(fields) > 0 {
			return nil, messaging.Deferred("a change to a job's "+strings.Join(fields, ", "), untrusted+" (its schedule, repeat and targets can change, and it can be paused, resumed, run and removed)")
		}
	case "":
		return nil, errors.New("action is required: create, list, update, pause, resume, remove, run, retry_delivery or discard_delivery")
	default:
		return nil, fmt.Errorf("unknown action %q: create, list, update, pause, resume, remove, run, retry_delivery or discard_delivery", a.Action)
	}
	if strings.TrimSpace(a.JobID) == "" {
		return nil, fmt.Errorf("job_id is required for %s (list the jobs first)", action)
	}
	if (action == "remove" || action == "run" && !s.isRunning(a.JobID)) && s.deliveryBlocked(a.JobID) {
		return nil, errors.New("report pending delivery: retry_delivery or explicitly discard_delivery first")
	}
	var result jobView
	now := s.o.Now()
	kick := false
	if action == "update" && (a.Deliver != nil || a.FailureDeliver != nil) {
		// the targets are checked before the file's lock is taken: the policy may ask Slack whose
		// DM a channel is, and the scheduler's ticks and runs wait on that lock
		var origin json.RawMessage
		found := false
		if err := s.mutate(func(f *File) (bool, error) {
			if j := f.Find(a.JobID); j != nil {
				found = true
				if j.Has("origin") {
					origin = append(json.RawMessage{}, j.raw["origin"]...)
				}
			}
			return false, nil
		}); err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("no job %q (list the jobs first)", a.JobID)
		}
		if err := s.checkLanes(a, origin); err != nil {
			return nil, err
		}
	}
	err := s.mutate(func(f *File) (bool, error) {
		j := f.Find(a.JobID)
		if j == nil {
			return false, fmt.Errorf("no job %q (list the jobs first)", a.JobID)
		}
		switch action {
		case "remove":
			if s.isRunning(a.JobID) {
				return false, errors.New("wait for the active run and its delivery to settle before removing the job")
			}
			f.Remove(a.JobID)
			_ = os.Remove(filepath.Join(s.state, "monitor", safeID(a.JobID)+".out"))
		case "pause":
			j.Set("enabled", false)
			j.Set("state", "paused")
			j.Set("paused_at", s.at(now))
			j.Set("paused_reason", nullable(a.Reason))
		case "resume":
			j.Set("enabled", true)
			j.Set("state", "scheduled")
			j.Set("paused_at", nil)
			j.Set("paused_reason", nil)
			// an occurrence that elapsed while it was paused stays due (one run, not a replay)
			if next, ok := parseStamp(j.Str("next_run_at")); !ok || next.After(now) {
				j.Set("next_run_at", nil)
				s.settleNext(j, now)
			}
		case "run":
			if j.Bool("delivery_hold") {
				return false, errors.New("delivery is held: recover or explicitly discard_delivery before running again")
			}
			if !j.Enabled() {
				return false, fmt.Errorf("job %s is %s: resume it first", a.JobID, orDefault(j.Str("state"), "disabled"))
			}
			if _, err := s.nextRun(j, now); err != nil {
				return false, fmt.Errorf("job %s cannot run: %v (update its schedule)", a.JobID, err)
			}
			// due now, unless it is already due (or waiting in line for the lane): its due time,
			// and so its place in line, stays as it is
			if next, ok := parseStamp(j.Str("next_run_at")); !ok || next.After(now) {
				j.Set("next_run_at", s.at(now))
			}
			s.request(a.JobID) // running: once this run ends
			kick = true
		case "update":
			if s.trusted(j) {
				if fields := repoFixed(a); len(fields) > 0 {
					return false, fmt.Errorf("job %s is defined in agent.yaml (the repo's job, delivered as the repo's): its %s cannot be changed here. "+
						"Make a job of your own with the change (create; the dream proposes your jobs for review) and pause this one, or ask a person to change agent.yaml; "+
						"its schedule, repeat and targets can be updated, and it can be paused, resumed and run", a.JobID, strings.Join(fields, ", "))
				}
			}
			if err := s.apply(j, a, now, false); err != nil {
				return false, err
			}
		}
		if action == "update" || action == "remove" || action == "pause" {
			// a gate's result kept for the old definition is not this job's; under the file's
			// lock, as a busy run's finish puts its job in line, so the result of a gate running
			// now is checked against the changed definition (finish)
			s.unwait(a.JobID)
		}
		result = view(j)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if kick {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	if action == "remove" {
		return map[string]any{"removed": a.JobID}, nil
	}
	return map[string]any{"job": result}, nil
}

func (s *Scheduler) create(a cronArgs, origin json.RawMessage) (any, error) {
	if a.Schedule == nil || strings.TrimSpace(*a.Schedule) == "" {
		return nil, errors.New("schedule is required for create")
	}
	noAgent := a.NoAgent != nil && *a.NoAgent
	if !noAgent && (a.Prompt == nil || strings.TrimSpace(*a.Prompt) == "") && (a.Skills == nil || len(*a.Skills) == 0) {
		return nil, errors.New("prompt is required for create (or skills, or no_agent with a script)")
	}
	now := s.o.Now()
	j := NewJob()
	j.Set("id", newID())
	j.Set("skills", []string{})
	j.Set("no_agent", false)
	j.Set("repeat", json.RawMessage(`{"times":null,"completed":0}`))
	j.Set("enabled", true)
	j.Set("state", "scheduled")
	j.Set("created_at", s.at(now))
	j.Set("running", false)
	j.Set("origin", origin)
	if a.Deliver == nil {
		j.Set("deliver", "origin")
	}
	if err := s.apply(j, a, now, true); err != nil {
		return nil, err
	}
	// the targets are checked outside the file's lock (create holds it only to add the job), the
	// default origin as a named one: a job made in a chat its deliveries may not reach is refused
	// now, not at every run
	if err := s.checkLanes(a, origin); err != nil {
		return nil, err
	}
	if a.Deliver == nil {
		if err := s.checkTargets("origin", origin); err != nil {
			return nil, fmt.Errorf("%w (the default deliver is origin, the chat the job is made from: name another, such as local or slack)", err)
		}
	}
	if j.Str("name") == "" {
		j.Set("name", clip(j.Str("prompt"), 50))
	}
	if a.Paused {
		j.Set("enabled", false)
		j.Set("state", "paused")
		j.Set("paused_at", s.at(now))
		j.Set("paused_reason", nullable(a.PausedReason))
	}
	err := s.mutate(func(f *File) (bool, error) {
		n := 0
		for _, x := range f.Jobs {
			if !s.trusted(x) { // the agent's own: not the repo's, even one promoted from its own
				n++
			}
		}
		if n >= MaxAgentJobs {
			return false, fmt.Errorf("you have %d jobs of your own, the most there may be (%d): update or remove one first", n, MaxAgentJobs)
		}
		f.Jobs = append(f.Jobs, j)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"job": view(j), "note": "Created. It runs on its schedule in a fresh session; its final reply is delivered."}, nil
}

// repoFixed are the fields an update names that a repo job keeps as agent.yaml says: what it runs
// (prompt, skills, scripts, monitor, no_agent, toolsets) and its name, by which a handler's jobs:
// names it. Its deliveries are trusted as the repo's, so what it delivers stays the repo's.
func repoFixed(a cronArgs) []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{{"prompt", a.Prompt != nil}, {"skills", a.Skills != nil}, {"script", a.Script != nil}, {"monitor", a.Monitor != nil},
		{"no_agent", a.NoAgent != nil}, {"enabled_toolsets", a.EnabledToolsets != nil}, {"name", a.Name != nil}} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// checkLanes holds the deliver and failure_deliver a create or update names to the messaging
// policy (checkTargets; "" deliver is origin), origin being the job's. Called outside the file's
// lock: the policy may ask Slack whose DM a channel is.
func (s *Scheduler) checkLanes(a cronArgs, origin json.RawMessage) error {
	if a.Deliver != nil {
		d := strings.TrimSpace(*a.Deliver)
		if d == "" {
			d = "origin"
		}
		if err := s.checkTargets(d, origin); err != nil {
			return err
		}
	}
	if a.FailureDeliver != nil {
		if err := s.checkTargets(*a.FailureDeliver, origin); err != nil {
			return err
		}
	}
	return nil
}

// apply sets the fields a create or update names ("" and [] clear on update). The targets are
// checked before (checkLanes), outside the file's lock; apply reads them for their grammar only.
func (s *Scheduler) apply(j *Job, a cronArgs, now time.Time, creating bool) error {
	if a.Schedule != nil {
		sch, err := ParseSchedule(*a.Schedule)
		if err != nil {
			return err
		}
		if err := s.checkEvery(sch); err != nil {
			return err
		}
		j.Set("schedule", sch)
		j.Set("schedule_display", sch.Display)
		j.Set("next_run_at", nil)
	}
	if a.Prompt != nil {
		if err := checkPrompt(*a.Prompt); err != nil {
			return err
		}
		j.Set("prompt", *a.Prompt)
	}
	if a.Name != nil {
		j.Set("name", strings.TrimSpace(*a.Name))
	}
	if a.Repeat != nil {
		var times any
		if *a.Repeat > 0 {
			times = *a.Repeat
		}
		_, done := j.Repeat()
		j.Set("repeat", json.RawMessage(fmt.Sprintf(`{"times":%s,"completed":%d}`, mustJSON(times), done)))
	}
	if a.Deliver != nil {
		d := strings.TrimSpace(*a.Deliver)
		if d == "" {
			d = "origin"
		}
		if _, err := Targets(d, nil); err != nil {
			return err
		}
		j.Set("deliver", d)
	}
	if a.FailureDeliver != nil {
		if _, err := Targets(*a.FailureDeliver, nil); err != nil {
			return err
		}
		j.Set("failure_deliver", nullable(strings.TrimSpace(*a.FailureDeliver)))
	}
	if a.Skills != nil {
		l := []string{}
		for _, n := range *a.Skills {
			if n = strings.TrimSpace(n); n != "" {
				if s.o.Skills != nil {
					if _, err := s.o.Skills.Find(n); err != nil {
						return fmt.Errorf("skill %s: %v", n, err)
					}
				}
				l = append(l, n)
			}
		}
		j.Set("skills", l)
		if len(l) > 0 {
			j.Set("skill", l[0])
		} else {
			j.Set("skill", nil)
		}
	}
	if a.Script != nil {
		rel, err := s.checkScript(*a.Script)
		if err != nil {
			return err
		}
		j.Set("script", nullable(rel))
	}
	if a.Monitor != nil {
		rel, err := s.checkScript(*a.Monitor)
		if err != nil {
			return err
		}
		j.Set("monitor_script", nullable(rel))
		j.Set("monitor_state", nil)
	}
	if a.NoAgent != nil {
		j.Set("no_agent", *a.NoAgent)
	}
	if a.EnabledToolsets != nil {
		var l any
		if len(*a.EnabledToolsets) > 0 {
			l = *a.EnabledToolsets
		}
		j.Set("enabled_toolsets", l)
	}
	if j.Bool("no_agent") {
		if strings.TrimSpace(j.Str("script")) == "" {
			return errors.New("a no_agent job needs a script")
		}
		if strings.TrimSpace(j.Str("monitor_script")) != "" {
			return errors.New("monitor cannot be used with no_agent")
		}
	}
	if j.Schedule().Kind == "" {
		return errors.New("the job has no schedule")
	}
	if !creating && a.Schedule == nil {
		return nil
	}
	if j.Enabled() {
		s.settleNext(j, now)
	}
	return nil
}

// checkTargets reads a deliver or failure_deliver value and holds each target to the messaging
// policy ("origin" is the job's origin, the chat it was made from, else the home channel, as its
// deliveries read it).
func (s *Scheduler) checkTargets(lane string, origin json.RawMessage) error {
	targets, err := Targets(lane, origin)
	if err != nil {
		return err
	}
	if s.checkTarget == nil {
		return nil
	}
	for _, t := range targets {
		if err := s.checkTarget(t); err != nil {
			return fmt.Errorf("deliver %s: %w", t, err)
		}
	}
	return nil
}

// checkScript refuses a script that does not resolve inside the home's scripts/ ("" clears) and
// gives its path relative to scripts/, as agent.yaml names it (the home's absolute path is not
// the repo's, and the dream proposes what is stored).
func (s *Scheduler) checkScript(script string) (string, error) {
	script = strings.TrimSpace(script)
	if script == "" {
		return "", nil
	}
	real, err := s.scriptPath(script)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.o.Home, "scripts")
	if !filepath.IsAbs(script) {
		return filepath.ToSlash(filepath.Clean(script)), nil
	}
	if rel, err := filepath.Rel(dir, filepath.Clean(script)); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return filepath.ToSlash(rel), nil
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	rel, err := filepath.Rel(realDir, real)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
