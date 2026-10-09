// Package review is `stormo review`: a read-only health sweep of the local fleet, the evidence an
// architecture review turns into findings. It only reads: compose ps, docker inspect/logs/exec
// cat, the nap store, the learnings, the core's /api/fleet. Raw output (it can quote log lines)
// goes to .swarm/review/, never into git.
package review

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
)

type CronIssue struct {
	Job     string `json:"job"`
	Problem string `json:"problem"`
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// AnalyzeSchedules finds problems in an agent's scheduled jobs (as its engine reads its schedules
// file): failed runs, failure streaks, delivery errors, overdue runs. Paused jobs are skipped.
func AnalyzeSchedules(jobs []engine.ScheduleStatus, now time.Time) []CronIssue {
	out := []CronIssue{}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		if j.Failed {
			out = append(out, CronIssue{j.Name, "last run " + j.LastStatus + ": " + clip(j.LastError, 160)})
		}
		if j.FailureStreak > 0 {
			out = append(out, CronIssue{j.Name, fmt.Sprintf("failure streak %d", j.FailureStreak)})
		}
		if j.DeliveryError != "" {
			out = append(out, CronIssue{j.Name, "delivery error: " + clip(j.DeliveryError, 160)})
		}
		if !j.NextRunAt.IsZero() && now.Sub(j.NextRunAt) > 15*time.Minute {
			out = append(out, CronIssue{j.Name, "overdue since " + j.NextRunAt.UTC().Format(time.RFC3339)})
		}
	}
	return out
}

// knownNoise is log noise from Stormo's own side (the engine adds its own, Engine.LogNoise).
var knownNoise = []engine.LogNoise{
	{Re: regexp.MustCompile(`PID 1 with no init above it`), Why: "entrypoint override in one-off docker runs"},
}

type LogGroup struct {
	Level   string `json:"level"`
	Pattern string `json:"pattern"`
	Count   int    `json:"count"`
	Known   string `json:"known,omitempty"`
}

type LogSummary struct {
	Errors   int        `json:"errors"`
	Warnings int        `json:"warnings"`
	Top      []LogGroup `json:"top"`
}

var (
	levelRe   = regexp.MustCompile(`\b(ERROR|CRITICAL|WARNING|Traceback)\b`)
	stampRe   = regexp.MustCompile(`^\S+ \S+ `)
	uuidRe    = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f-]{27,}\b`)
	hexRe     = regexp.MustCompile(`(?i)\b[0-9a-f]{12,}\b`)
	slackIDRe = regexp.MustCompile(`\b(U|D|C|A)[0-9A-Z]{8,}\b`)
	numRe     = regexp.MustCompile(`\d+(\.\d+)?`)
	wsRe      = regexp.MustCompile(`\s+`)
)

// SummarizeLogs groups error/warning lines by a normalized pattern (timestamps, numbers, ids
// stripped); lines matching noise (Stormo's and the engine's) are marked known.
func SummarizeLogs(text string, noise ...engine.LogNoise) LogSummary {
	noise = append(append([]engine.LogNoise{}, knownNoise...), noise...)
	groups := map[string]*LogGroup{}
	order := []string{}
	s := LogSummary{Top: []LogGroup{}}
	for _, line := range strings.Split(text, "\n") {
		m := levelRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		level := "error"
		if m[1] == "WARNING" {
			level = "warning"
			s.Warnings++
		} else {
			s.Errors++
		}
		p := stampRe.ReplaceAllString(line, "")
		p = uuidRe.ReplaceAllString(p, "<uuid>")
		p = hexRe.ReplaceAllString(p, "<hex>")
		p = slackIDRe.ReplaceAllString(p, "<slack-id>")
		p = numRe.ReplaceAllString(p, "N")
		p = clip(strings.TrimSpace(wsRe.ReplaceAllString(p, " ")), 220)
		g, ok := groups[p]
		if !ok {
			g = &LogGroup{Level: level, Pattern: p}
			for _, k := range noise {
				if k.Re.MatchString(line) {
					g.Known = k.Why
					break
				}
			}
			groups[p] = g
			order = append(order, p)
		}
		g.Count++
	}
	for _, p := range order {
		s.Top = append(s.Top, *groups[p])
	}
	sort.SliceStable(s.Top, func(i, j int) bool {
		ki, kj := s.Top[i].Known != "", s.Top[j].Known != ""
		if ki != kj {
			return !ki
		}
		return s.Top[i].Count > s.Top[j].Count
	})
	if len(s.Top) > 12 {
		s.Top = s.Top[:12]
	}
	return s
}

type NapFreshness struct {
	Count      int     `json:"count"`
	Latest     *string `json:"latest"`
	AgeMinutes *int    `json:"ageMinutes"`
	Stale      bool    `json:"stale"`
}

// Freshness compares the latest nap's age with the interval the nap sidecar actually runs with.
func Freshness(takenAt []string, intervalSeconds int, now time.Time) NapFreshness {
	var latest time.Time
	for _, t := range takenAt {
		if p, err := time.Parse(time.RFC3339Nano, t); err == nil && p.After(latest) {
			latest = p
		}
	}
	if latest.IsZero() {
		return NapFreshness{Stale: true}
	}
	age := int(math.Round(now.Sub(latest).Minutes()))
	iso := loop.IsoMillis(latest)
	return NapFreshness{Latest: &iso, AgeMinutes: &age, Stale: age*60 > intervalSeconds*3+120}
}

// sh runs a read-only command. both keeps stderr too: `docker logs` replays the container's
// stderr on stderr. A failing command's stderr is always kept.
func sh(both bool, argv ...string) (bool, string) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if both {
		out, err := cmd.CombinedOutput()
		return err == nil, string(out)
	}
	out, err := cmd.Output()
	s := string(out)
	if ee, ok := err.(*exec.ExitError); ok {
		s += string(ee.Stderr)
	}
	return err == nil, s
}

type Container struct {
	State         string  `json:"state"`
	Health        *string `json:"health"`
	Restarts      int     `json:"restarts"`
	StartedAt     string  `json:"startedAt"`
	RestartPolicy string  `json:"restartPolicy"`
	Image         string  `json:"image"`
	NapInterval   *int    `json:"napInterval,omitempty"`
}

// containerID asks compose for one service's container in the agent's project, "" if it has none.
// The name differs by compose flavour (swarm-x-agent-1 in v2, swarm-x_agent_1 in v1), so never
// build it by hand.
func containerID(compose []string, id, service string) string {
	if compose == nil {
		return ""
	}
	argv := append(append([]string{}, compose...), "-p", "swarm-"+id, "ps", "--all", "-q", service)
	ok, out := sh(false, argv...)
	if !ok {
		return ""
	}
	if f := strings.Fields(out); len(f) > 0 {
		return f[0]
	}
	return ""
}

func inspect(name string) *Container {
	if name == "" {
		return nil
	}
	ok, out := sh(false, "docker", "inspect", name)
	if !ok {
		return nil
	}
	var cs []struct {
		State struct {
			Status    string
			StartedAt string
			Health    *struct{ Status string }
		}
		RestartCount int
		HostConfig   struct{ RestartPolicy struct{ Name string } }
		Config       struct {
			Image string
			Env   []string
		}
	}
	if json.Unmarshal([]byte(out), &cs) != nil || len(cs) == 0 {
		return nil
	}
	c := cs[0]
	r := &Container{State: c.State.Status, Restarts: c.RestartCount, StartedAt: c.State.StartedAt, RestartPolicy: c.HostConfig.RestartPolicy.Name, Image: c.Config.Image}
	if c.State.Health != nil {
		r.Health = &c.State.Health.Status
	}
	for _, e := range c.Config.Env {
		if v, ok := strings.CutPrefix(e, "SWARM_NAP_INTERVAL="); ok {
			var n int
			if _, err := fmt.Sscan(v, &n); err == nil {
				r.NapInterval = &n
			}
		}
	}
	return r
}

type Learnings struct {
	Proposed       int `json:"proposed"`
	SkillProposals int `json:"skillProposals"`
}

type AgentReport struct {
	ID         string          `json:"id"`
	Unit       string          `json:"unit"`
	Model      string          `json:"model"`
	LocalModel string          `json:"localModel,omitempty"`
	Container  *Container      `json:"container"`
	NapSidecar *Container      `json:"napSidecar"`
	Platforms  json.RawMessage `json:"platforms"`
	Cron       []CronIssue     `json:"cron"`
	Naps       NapFreshness    `json:"naps"`
	Logs       LogSummary      `json:"logs"`
	Learnings  Learnings       `json:"learnings"`
}

type CoreReport struct {
	Up  bool        `json:"up"`
	Log *LogSummary `json:"log"`
}

type Report struct {
	At     string        `json:"at"`
	Since  string        `json:"since"`
	Core   CoreReport    `json:"core"`
	Agents []AgentReport `json:"agents"`
}

// Collect sweeps the local fleet.
func Collect(inst *instance.Instance, ids []string, since string) (*Report, error) {
	if since == "" {
		since = "24h"
	}
	if len(ids) == 0 {
		ids = manifest.AgentIDs(inst.Root)
	}
	var fleet struct {
		Agents []struct {
			ID       string `json:"id"`
			Activity *struct {
				Platforms json.RawMessage `json:"platforms"`
			} `json:"activity"`
		} `json:"agents"`
	}
	client := http.Client{Timeout: 5 * time.Second}
	coreUp := false
	if res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/fleet", core.CorePort())); err == nil {
		coreUp = json.NewDecoder(res.Body).Decode(&fleet) == nil
		res.Body.Close()
	}
	now := time.Now()
	r := &Report{At: loop.IsoMillis(now), Since: since, Agents: []AgentReport{}}
	compose, _ := local.ComposeCommand() // nil: no compose, every agent reads as not running
	for _, id := range ids {
		m, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			return nil, err
		}
		eng, err := engines.Get(m.Engine.Kind, inst)
		if err != nil {
			return nil, err
		}
		agentID := containerID(compose, id, "agent")
		agent := inspect(agentID)
		nap := inspect(containerID(compose, id, "nap"))
		naps := []string{}
		dir := filepath.Join(inst.Root, ".swarm", "store", id, "naps")
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			var n struct {
				TakenAt string `json:"takenAt"`
			}
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && json.Unmarshal(b, &n) == nil {
				naps = append(naps, n.TakenAt)
			}
		}
		cron := []CronIssue{}
		if agent != nil && agent.State == "running" {
			if L := eng.Layout(); L.Schedules != "" {
				if ok, out := sh(false, "docker", "exec", agentID, "cat", L.Home+"/"+L.Schedules); ok {
					if jobs, err := eng.Schedules([]byte(out)); err != nil {
						cron = []CronIssue{{"(" + L.Schedules + ")", "unreadable"}}
					} else {
						cron = AnalyzeSchedules(jobs, now)
					}
				}
			}
		}
		logs := ""
		if agent != nil {
			_, logs = sh(true, "docker", "logs", "--since", since, agentID)
		}
		ledger, _ := learning.LoadLedger(inst.Root, id)
		proposed := 0
		for _, e := range ledger {
			if e.Status == learning.Proposed {
				proposed++
			}
		}
		props, _ := loop.ListSkillProposals(inst.Root, id)
		interval := m.Learning.NapIntervalSeconds
		if nap != nil && nap.NapInterval != nil {
			interval = *nap.NapInterval
		}
		fresh := Freshness(naps, interval, now)
		fresh.Count = len(naps)
		a := AgentReport{ID: id, Unit: m.Unit, Model: m.Engine.Model, Container: agent, NapSidecar: nap, Platforms: json.RawMessage("null"),
			Cron: cron, Naps: fresh, Logs: SummarizeLogs(logs, eng.LogNoise()...), Learnings: Learnings{proposed, len(props)}}
		if m.Engine.Local != nil {
			a.LocalModel = m.Engine.Local.Model
		}
		for _, fa := range fleet.Agents {
			if fa.ID == id && fa.Activity != nil && len(fa.Activity.Platforms) > 0 {
				a.Platforms = fa.Activity.Platforms
			}
		}
		r.Agents = append(r.Agents, a)
	}
	r.Core.Up = coreUp
	if _, err := os.Stat(filepath.Join(inst.Root, ".swarm", "core", "core.log")); err == nil {
		_, out := sh(false, "tail", "-n", "2000", filepath.Join(inst.Root, ".swarm", "core", "core.log"))
		s := SummarizeLogs(out)
		r.Core.Log = &s
	}
	return r, nil
}

// Render is the markdown summary.
func Render(r *Report) string {
	core := "core: DOWN"
	if r.Core.Up {
		core = "core: up"
	}
	if r.Core.Log != nil {
		core += fmt.Sprintf(" · log errors %d, warnings %d", r.Core.Log.Errors, r.Core.Log.Warnings)
	}
	lines := []string{fmt.Sprintf("# Fleet review %s (logs since %s)", r.At, r.Since), "", core, ""}
	for _, a := range r.Agents {
		model := a.Model
		if a.LocalModel != "" {
			model = a.LocalModel
		}
		lines = append(lines, fmt.Sprintf("## %s (%s, %s)", a.ID, a.Unit, model))
		if c := a.Container; c != nil {
			h := ""
			if c.Health != nil {
				h = "/" + *c.Health
			}
			lines = append(lines, fmt.Sprintf("- container: %s%s, restarts %d, since %s, policy %s", c.State, h, c.Restarts, c.StartedAt, c.RestartPolicy))
		} else {
			lines = append(lines, "- container: not running")
		}
		if n := a.NapSidecar; n != nil {
			iv := "?"
			if n.NapInterval != nil {
				iv = fmt.Sprint(*n.NapInterval)
			}
			lines = append(lines, fmt.Sprintf("- nap sidecar: %s, policy %s, interval %s s", n.State, n.RestartPolicy, iv))
		} else {
			lines = append(lines, "- nap sidecar: not running")
		}
		latest, age, stale := "-", "?", ""
		if a.Naps.Latest != nil {
			latest = *a.Naps.Latest
		}
		if a.Naps.AgeMinutes != nil {
			age = fmt.Sprint(*a.Naps.AgeMinutes)
		}
		if a.Naps.Stale {
			stale = " **STALE**"
		}
		lines = append(lines, fmt.Sprintf("- naps: %d, latest %s (%s min ago)%s", a.Naps.Count, latest, age, stale))
		var platforms map[string]struct {
			State string `json:"state"`
		}
		if json.Unmarshal(a.Platforms, &platforms) == nil && len(platforms) > 0 {
			names := []string{}
			for k := range platforms {
				names = append(names, k)
			}
			sort.Strings(names)
			parts := []string{}
			for _, k := range names {
				parts = append(parts, k+" "+platforms[k].State)
			}
			lines = append(lines, "- platforms: "+strings.Join(parts, ", "))
		}
		cron := "ok"
		if len(a.Cron) > 0 {
			parts := []string{}
			for _, c := range a.Cron {
				parts = append(parts, c.Job+": "+c.Problem)
			}
			cron = strings.Join(parts, "; ")
		}
		lines = append(lines, "- cron: "+cron, fmt.Sprintf("- logs: %d errors, %d warnings", a.Logs.Errors, a.Logs.Warnings))
		for _, t := range a.Logs.Top {
			known := ""
			if t.Known != "" {
				known = " (known: " + t.Known + ")"
			}
			lines = append(lines, fmt.Sprintf("  - %d× %s%s: %s", t.Count, t.Level, known, t.Pattern))
		}
		lines = append(lines, fmt.Sprintf("- learnings waiting: %d lessons, %d skill proposals", a.Learnings.Proposed, a.Learnings.SkillProposals), "")
	}
	return strings.Join(lines, "\n")
}

// Run collects, writes .swarm/review/<stamp>.{json,md} and returns the markdown path.
func Run(inst *instance.Instance, ids []string, since string) (string, string, error) {
	r, err := Collect(inst, ids, since)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(inst.Root, ".swarm", "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(r.At)
	body, _ := learning.MarshalIndent(r)
	if err := os.WriteFile(filepath.Join(dir, stamp+".json"), body, 0o644); err != nil {
		return "", "", err
	}
	md := Render(r)
	p := filepath.Join(dir, stamp+".md")
	return md, p, os.WriteFile(p, []byte(md), 0o644)
}
