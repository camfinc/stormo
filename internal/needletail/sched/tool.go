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

	"github.com/camfinc/stormo/internal/needletail/tools"
)

// The cronjob_manage tool: Hermes' name and arguments (the ones that mean something here), so a
// prompt or skill that schedules work reads the same on either engine. A job the agent makes is
// a job like the repo's: after a restart, rehydrate keeps it with its run state, while a job
// agent.yaml defines comes back with the repo's definition (the adapter's MergeSchedules); the
// dream proposes the agent's jobs (and its changes to the repo's) for review.

const toolDescription = `Manage scheduled jobs: action 'create' schedules a job from a prompt and/or skills; 'list' shows the jobs; 'update', 'pause', 'resume', 'remove' and 'run' act on one by job_id (list first: never guess an id). 'run' makes a job due now (it starts within the next tick; its output is delivered as the job's deliver says, it does not come back here).

Jobs run in a fresh session with no chat context, so prompts must be self-contained, and the run's FINAL REPLY is what gets delivered; a run cannot ask questions. Prefer updating a job over creating a near-duplicate.`

const toolSchema = `{"type":"object","properties":{
"action":{"type":"string","description":"One of: create, list, update, pause, resume, remove, run. create needs schedule, and prompt unless no_agent."},
"job_id":{"type":"string","description":"Required for update, pause, resume, remove and run."},
"prompt":{"type":"string","description":"create/update: the job's whole, self-contained prompt (paired with any skills as the task)."},
"schedule":{"type":"string","description":"Required for create. A recurring interval ('30m', 'every 2h', 'every hour'), a day and time ('every monday 9am', 'weekdays at 9am', 'every day at 14:00') or a 5-field cron expression ('0 9 * * 1-5'). One-time schedules are not supported."},
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
			return s.manage(a, from)
		}}
}

// jobView is a job as list shows it.
type jobView struct {
	JobID          string   `json:"job_id"`
	Name           string   `json:"name"`
	Schedule       string   `json:"schedule"`
	Enabled        bool     `json:"enabled"`
	State          string   `json:"state"`
	Prompt         string   `json:"prompt,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	Script         string   `json:"script,omitempty"`
	Monitor        string   `json:"monitor,omitempty"`
	NoAgent        bool     `json:"no_agent,omitempty"`
	Deliver        string   `json:"deliver,omitempty"`
	FailureDeliver string   `json:"failure_deliver,omitempty"`
	Toolsets       []string `json:"enabled_toolsets,omitempty"`
	Repeat         string   `json:"repeat,omitempty"`
	NextRunAt      string   `json:"next_run_at,omitempty"`
	LastRunAt      string   `json:"last_run_at,omitempty"`
	LastStatus     string   `json:"last_status,omitempty"`
	LastError      string   `json:"last_error,omitempty"`
	FailureStreak  int      `json:"failure_streak,omitempty"`
	Running        bool     `json:"running,omitempty"`
	PausedReason   string   `json:"paused_reason,omitempty"`
}

func view(j *Job) jobView {
	v := jobView{JobID: j.ID(), Name: j.Name(), Schedule: j.Schedule().Display, Enabled: j.Enabled(), State: j.Str("state"),
		Prompt: clip(j.Str("prompt"), 300), Skills: j.Skills(), Script: j.Str("script"), Monitor: j.Str("monitor_script"),
		NoAgent: j.Bool("no_agent"), Deliver: j.Str("deliver"), FailureDeliver: j.Str("failure_deliver"), Toolsets: j.Strings("enabled_toolsets"),
		NextRunAt: j.Str("next_run_at"), LastRunAt: j.Str("last_run_at"), LastStatus: j.Str("last_status"), LastError: clip(j.Str("last_error"), 300),
		FailureStreak: j.Int("failure_streak"), Running: j.Bool("running"), PausedReason: j.Str("paused_reason")}
	if v.Schedule == "" {
		v.Schedule = j.Schedule().Expr
	}
	if times, done := j.Repeat(); times > 0 {
		v.Repeat = fmt.Sprintf("%d of %d runs done", done, times)
	}
	return v
}

func (s *Scheduler) manage(a cronArgs, origin json.RawMessage) (any, error) {
	action := strings.ToLower(strings.TrimSpace(a.Action))
	switch action {
	case "list":
		var out []jobView
		all := a.IncludeDisabled == nil || *a.IncludeDisabled
		err := s.mutate(func(f *File) (bool, error) {
			for _, j := range f.Jobs {
				if all || j.Enabled() {
					out = append(out, view(j))
				}
			}
			return false, nil
		})
		if out == nil {
			out = []jobView{}
		}
		return map[string]any{"jobs": out, "count": len(out)}, err
	case "create":
		return s.create(a, origin)
	case "update", "pause", "resume", "remove", "run":
	case "":
		return nil, errors.New("action is required: create, list, update, pause, resume, remove or run")
	default:
		return nil, fmt.Errorf("unknown action %q: create, list, update, pause, resume, remove or run", a.Action)
	}
	if strings.TrimSpace(a.JobID) == "" {
		return nil, fmt.Errorf("job_id is required for %s (list the jobs first)", action)
	}
	var result jobView
	now := s.o.Now()
	kick := false
	err := s.mutate(func(f *File) (bool, error) {
		j := f.Find(a.JobID)
		if j == nil {
			return false, fmt.Errorf("no job %q (list the jobs first)", a.JobID)
		}
		switch action {
		case "remove":
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
			if !j.Enabled() {
				return false, fmt.Errorf("job %s is %s: resume it first", a.JobID, orDefault(j.Str("state"), "disabled"))
			}
			if _, err := s.nextRun(j, now); err != nil {
				return false, fmt.Errorf("job %s cannot run: %v (update its schedule)", a.JobID, err)
			}
			j.Set("next_run_at", s.at(now))
			s.request(a.JobID) // running: once this run ends
			kick = true
		case "update":
			if err := s.apply(j, a, now, false); err != nil {
				return false, err
			}
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
		f.Jobs = append(f.Jobs, j)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"job": view(j), "note": "Created. It runs on its schedule in a fresh session; its final reply is delivered."}, nil
}

// apply sets the fields a create or update names ("" and [] clear on update).
func (s *Scheduler) apply(j *Job, a cronArgs, now time.Time, creating bool) error {
	if a.Schedule != nil {
		sch, err := ParseSchedule(*a.Schedule)
		if err != nil {
			return err
		}
		j.Set("schedule", sch)
		j.Set("schedule_display", sch.Display)
		j.Set("next_run_at", nil)
	}
	if a.Prompt != nil {
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
		if err := s.checkScript(*a.Script); err != nil {
			return err
		}
		j.Set("script", nullable(strings.TrimSpace(*a.Script)))
	}
	if a.Monitor != nil {
		if err := s.checkScript(*a.Monitor); err != nil {
			return err
		}
		j.Set("monitor_script", nullable(strings.TrimSpace(*a.Monitor)))
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

// checkScript refuses a script that does not resolve inside the home's scripts/ ("" clears).
func (s *Scheduler) checkScript(script string) error {
	if strings.TrimSpace(script) == "" {
		return nil
	}
	_, err := s.scriptPath(strings.TrimSpace(script))
	return err
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
