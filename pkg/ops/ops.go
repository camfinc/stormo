// Package ops is one verb set for both places an agent can run. Local (compose) is the default;
// remote targets the ECS service, and remote changes are confirmed first. An agent runs in one
// place at a time (pkg/place); Handoff moves it with its latest nap. Anything that starts agents
// (the CLI, the core) goes through Start/Restart/Handoff here, so the one-place guard always applies.
package ops

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/aws"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/place"
	"github.com/camfinc/stormo/pkg/secrets"
)

// PickWhere resolves --remote / --target / SWARM_TARGET.
func PickWhere(remote bool, target string, getenv func(string) string) (place.Where, error) {
	v := target
	if remote {
		v = "remote"
	}
	if v == "" {
		v = getenv("SWARM_TARGET")
	}
	if v == "" {
		v = "local"
	}
	if v != "local" && v != "remote" {
		return "", fmt.Errorf(`target must be local or remote, got "%s"`, v)
	}
	return place.Where(v), nil
}

// Deps are the outside world, replaceable in tests.
type Deps struct {
	Inst    *instance.Instance
	AWS     aws.CLI
	Target  deploy.Target
	Confirm func(plan []string) bool
	Log     func(line string)
	// Overrides for tests; default to compose / secrets / sleep.
	LocalSide    func(id string) place.SideState
	SharedTokens func(a *manifest.Agent) (bool, error)
	Sleep        func(time.Duration)
	Up           local.UpOptions
}

// DefaultDeps are the real CLI dependencies.
func DefaultDeps(inst *instance.Instance, yes bool) *Deps {
	return &Deps{Inst: inst, AWS: aws.Exec, Target: deploy.DefaultTarget(inst), Confirm: func(p []string) bool { return PromptConfirm(p, yes) }, Log: func(l string) { fmt.Println(l) }}
}

// PromptConfirm prints a plan and asks; --yes skips the question, a non-terminal declines.
func PromptConfirm(plan []string, yes bool) bool {
	for _, l := range plan {
		fmt.Println(l)
	}
	if yes {
		return true
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Println("not a terminal: re-run with --yes to apply")
		return false
	}
	fmt.Print("Proceed? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
}

// LocalStore is the instance's directory store.
func LocalStore(root string) loop.Store {
	return loop.FsStore{Root: filepath.Join(root, ".swarm", "store")}
}

func (d *Deps) store(where place.Where) (loop.Store, error) {
	if where == place.Local {
		return LocalStore(d.Inst.Root), nil
	}
	return loop.NewS3Store(d.Target.Bucket, d.Target.Region)
}

func (d *Deps) agent(id string) (*manifest.Agent, error) {
	return manifest.Load(d.Inst.Root, id, d.Inst.Names.Secret)
}

// Status is one agent's row.
type Status struct {
	Agent            string `json:"agent"`
	Unit             string `json:"unit"`
	Where            string `json:"where"`
	State            string `json:"state"` // running | stopped | starting | missing | unknown
	Health           string `json:"health,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	LastNap          string `json:"lastNap,omitempty"`
	PendingLearnings int    `json:"pendingLearnings"`
	PendingSkills    int    `json:"pendingSkills"`
	Detail           string `json:"detail,omitempty"`
}

func (d *Deps) remoteState(id string) (state, detail string) {
	s, err := place.RemoteService(d.Inst, id, d.AWS, d.Target)
	if err != nil {
		return "unknown", strings.SplitN(err.Error(), "\n", 2)[0]
	}
	if s == nil {
		return "missing", "ECS service not created"
	}
	rollout := ""
	for _, x := range s.Deployments {
		if x.Status == "PRIMARY" && x.RolloutState != "" {
			rollout = ", rollout " + x.RolloutState
		}
	}
	state = "starting"
	switch {
	case s.DesiredCount == 0:
		state = "stopped"
	case s.RunningCount >= s.DesiredCount:
		state = "running"
	}
	return state, fmt.Sprintf("%d/%d tasks%s", s.RunningCount, s.DesiredCount, rollout)
}

// AgentStatus reports every agent (or ids) at one place.
func AgentStatus(where place.Where, d *Deps, ids []string) ([]Status, error) {
	if len(ids) == 0 {
		ids = manifest.AgentIDs(d.Inst.Root)
	}
	store, storeErr := d.store(where)
	out := []Status{}
	for _, id := range ids {
		a, err := d.agent(id)
		if err != nil {
			return nil, err
		}
		row := Status{Agent: id, Unit: a.Unit, Where: string(where)}
		if where == place.Local {
			ls := place.LocalStateOf(id, local.ComposeCommand)
			row.State, row.Health = ls.State, ls.Health
			port, err := local.Port(d.Inst.Root, id)
			if err != nil {
				return nil, err
			}
			row.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
		} else {
			row.State, row.Detail = d.remoteState(id)
			row.Endpoint = "ecs://" + d.Target.Cluster + "/" + place.ServiceName(d.Inst, id)
		}
		if storeErr == nil {
			if n, err := loop.Latest(store, id); err == nil && n != nil {
				row.LastNap = n.TakenAt
			}
		}
		ledger, err := learning.LoadLedger(d.Inst.Root, id)
		if err != nil {
			return nil, err
		}
		for _, e := range ledger {
			if e.Status == learning.Proposed {
				row.PendingLearnings++
			}
		}
		props, err := loop.ListSkillProposals(d.Inst.Root, id)
		if err != nil {
			return nil, err
		}
		row.PendingSkills = len(props)
		out = append(out, row)
	}
	return out, nil
}

func ago(iso string, now time.Time) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if iso == "" || err != nil {
		return "never"
	}
	m := int(math.Round(now.Sub(t).Minutes()))
	switch {
	case m < 60:
		return fmt.Sprintf("%dm ago", m)
	case m < 2880:
		return fmt.Sprintf("%dh ago", int(math.Round(float64(m)/60)))
	default:
		return fmt.Sprintf("%dd ago", int(math.Round(float64(m)/1440)))
	}
}

// RenderStatus is the aligned status table.
func RenderStatus(rows []Status, now time.Time) string {
	if len(rows) == 0 {
		return "no agents"
	}
	where := rows[0].Where
	last := "SERVICE"
	if where == "local" {
		last = "API"
	}
	header := []string{"AGENT", "UNIT", "STATE", "LAST NAP", "TO REVIEW", last}
	body := [][]string{}
	for _, r := range rows {
		state := r.State
		if r.Health != "" && r.Health != "healthy" && r.State == "running" {
			state += " (" + r.Health + ")"
		}
		review := "-"
		if r.PendingLearnings > 0 || r.PendingSkills > 0 {
			review = fmt.Sprintf("%d lessons, %d skills", r.PendingLearnings, r.PendingSkills)
		}
		end := r.Endpoint
		if r.Detail != "" {
			end += "  " + r.Detail
		}
		body = append(body, []string{r.Agent, r.Unit, state, ago(r.LastNap, now), review, end})
	}
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
		for _, b := range body {
			widths[i] = max(widths[i], len([]rune(b[i])))
		}
	}
	line := func(cells []string) string {
		parts := make([]string, len(cells))
		for i, c := range cells {
			if i == len(cells)-1 {
				parts[i] = c
			} else {
				parts[i] = c + strings.Repeat(" ", widths[i]-len([]rune(c)))
			}
		}
		return strings.Join(parts, "  ")
	}
	lines := []string{strings.ToUpper(where), line(header)}
	for _, b := range body {
		lines = append(lines, line(b))
	}
	return strings.Join(lines, "\n")
}

func (d *Deps) remoteChange(id, verb string, args []string) (bool, error) {
	full := append([]string{"ecs", "update-service", "--region", d.Target.Region, "--cluster", d.Target.Cluster, "--service", place.ServiceName(d.Inst, id)}, args...)
	if !d.Confirm([]string{verb + " " + id + " on ECS:", "  aws " + strings.Join(full, " ")}) {
		d.Log(id + ": skipped")
		return false, nil
	}
	var out any
	if err := aws.JSON(d.AWS, full, &out); err != nil {
		return false, err
	}
	d.Log(fmt.Sprintf("%s: %s requested (%s)", id, verb, place.ServiceName(d.Inst, id)))
	return true, nil
}

func (d *Deps) sideOf(where place.Where, id string) place.SideState {
	if where == place.Local {
		if d.LocalSide != nil {
			return d.LocalSide(id)
		}
		return place.LocalSide(id, local.ComposeCommand)
	}
	return place.RemoteSide(d.Inst, id, d.AWS, d.Target)
}

// sharedTokens: does a local run use the same Slack app as ECS (no dev tokens under local.agents.<id>)?
func (d *Deps) sharedTokens(a *manifest.Agent) (bool, error) {
	if d.SharedTokens != nil {
		return d.SharedTokens(a)
	}
	f, err := secrets.Load(secrets.Path(d.Inst.Root))
	if err != nil {
		return false, err
	}
	return len(secrets.ProdTokenClashes(f, a)) > 0, nil
}

func (d *Deps) guardRemoteStart(id string) error {
	a, err := d.agent(id)
	if err != nil {
		return err
	}
	shared, err := d.sharedTokens(a)
	if err != nil {
		return err
	}
	if g := place.GuardStart(id, place.Remote, d.sideOf(place.Local, id), shared, false); !g.OK {
		return errors.New(g.Reason)
	}
	return nil
}

func (d *Deps) requireService(id string) (*place.EcsService, error) {
	s, err := place.RemoteService(d.Inst, id, d.AWS, d.Target)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("%s: ECS service %s does not exist yet. Create it once with `stormo deploy render %s` and the printed commands.", id, place.ServiceName(d.Inst, id), id)
	}
	return s, nil
}

func (d *Deps) up(id string, other place.SideState) (int, error) {
	o := d.Up
	o.Other = other
	if o.Log == nil {
		o.Log = d.Log
	}
	port, off, err := local.Up(d.Inst, id, o)
	if err != nil {
		return 0, err
	}
	for _, x := range off {
		what := strings.Join(x.What, ", ")
		if what == "" {
			what = "nothing it gates"
		}
		d.Log(fmt.Sprintf("%s: %s not set, so off: %s", id, x.Name, what))
	}
	return port, nil
}

func Start(where place.Where, id string, d *Deps) error {
	if where == place.Local {
		port, err := d.up(id, "")
		if err == nil {
			d.Log(fmt.Sprintf("%s: running locally · API http://127.0.0.1:%d · stormo chat %s \"hola\"", id, port, id))
		}
		return err
	}
	s, err := d.requireService(id)
	if err != nil {
		return err
	}
	if s.DesiredCount >= 1 {
		d.Log(fmt.Sprintf("%s: already running (%d/%d)", id, s.RunningCount, s.DesiredCount))
		return nil
	}
	if err := d.guardRemoteStart(id); err != nil {
		return err
	}
	_, err = d.remoteChange(id, "start", []string{"--desired-count", "1"})
	return err
}

func Stop(where place.Where, id string, d *Deps) error {
	if where == place.Local {
		if err := local.Down(d.Inst, id); err != nil {
			return err
		}
		d.Log(id + ": stopped locally (final nap saved to .swarm/store)")
		return nil
	}
	s, err := d.requireService(id)
	if err != nil {
		return err
	}
	if s.DesiredCount == 0 {
		d.Log(id + ": already stopped")
		return nil
	}
	_, err = d.remoteChange(id, "stop", []string{"--desired-count", "0"})
	return err
}

func Restart(where place.Where, id string, d *Deps) error {
	if where == place.Local {
		return Start(place.Local, id, d) // up always recycles: final nap → fresh home
	}
	s, err := d.requireService(id)
	if err != nil {
		return err
	}
	args := []string{"--force-new-deployment"}
	if s.DesiredCount == 0 {
		if err := d.guardRemoteStart(id); err != nil { // restarting a stopped service starts it
			return err
		}
		args = append(args, "--desired-count", "1")
	}
	_, err = d.remoteChange(id, "restart", args)
	return err
}

// HandoffOptions tune Handoff.
type HandoffOptions struct {
	// Resume from the source even if the destination holds a newer nap.
	Force   bool
	Poll    time.Duration
	Timeout time.Duration
}

func (d *Deps) waitRemoteStopped(id string, o HandoffOptions) error {
	poll, timeout := o.Poll, o.Timeout
	if poll == 0 {
		poll = 10 * time.Second
	}
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	sleep := d.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for waited := time.Duration(0); ; waited += poll {
		s, err := place.RemoteService(d.Inst, id, d.AWS, d.Target)
		if err != nil {
			return err
		}
		if s == nil || (s.RunningCount == 0 && s.PendingCount == 0) {
			return nil
		}
		if waited >= timeout {
			return fmt.Errorf("%s: ECS tasks still stopping after %s. Re-run the handoff once `stormo -r` shows it stopped.", id, timeout)
		}
		sleep(poll)
	}
}

// Handoff moves an agent to `to`: check the destination can take it, stop the source (its shutdown
// nap is the state to resume), copy that nap and every undreamed one to the destination store,
// start there. Remote stop/start are confirmed like any remote change; declining either stops it.
func Handoff(id string, to place.Where, d *Deps, o HandoffOptions) error {
	from := place.OtherSide(to)
	a, err := d.agent(id)
	if err != nil {
		return err
	}
	// 1. Pre-flight the destination before touching the source, so a failure leaves it running.
	if to == place.Remote {
		if _, err := d.requireService(id); err != nil {
			return err
		}
		s, err := d.store(place.Remote)
		if err != nil {
			return err
		}
		if _, err := s.List(id + "/latest.json"); err != nil {
			return err
		}
	} else {
		if _, err := local.ComposeCommand(); err != nil {
			return err
		}
		f, err := secrets.Load(secrets.Path(d.Inst.Root))
		if err != nil {
			return err
		}
		if missing := secrets.Resolve(f, a, manifest.Local).Missing; len(missing) > 0 {
			return fmt.Errorf("%s: missing secrets for a local run: %s (stormo secrets check %s)", id, strings.Join(missing, ", "), id)
		}
	}
	if d.sideOf(to, id) == place.Running {
		return fmt.Errorf("%s: already running on %s", id, to)
	}
	fromState := d.sideOf(from, id)
	if fromState == place.Unknown {
		flag := ""
		if from == place.Remote {
			flag = " -r"
		}
		return fmt.Errorf("%s: cannot tell whether it is running on %s; check `stormo%s` first", id, from, flag)
	}
	fromStore, err := d.store(from)
	if err != nil {
		return err
	}
	toStore, err := d.store(to)
	if err != nil {
		return err
	}

	// 2. Stop the source; its shutdown nap is what the destination resumes from.
	if fromState == place.Running {
		since := time.Now()
		if from == place.Local {
			if err := local.Down(d.Inst, id); err != nil {
				return err
			}
		} else {
			s, err := d.requireService(id)
			if err != nil {
				return err
			}
			if s.DesiredCount > 0 {
				ok, err := d.remoteChange(id, "stop", []string{"--desired-count", "0"})
				if err != nil {
					return err
				}
				if !ok {
					d.Log(fmt.Sprintf("%s: handoff cancelled, still on %s", id, from))
					return nil
				}
			}
			d.Log(id + ": waiting for the ECS task to stop and take its final nap…")
			if err := d.waitRemoteStopped(id, o); err != nil {
				return err
			}
		}
		if w, err := loop.FinalNapWarning(fromStore, id, since); err != nil {
			return err
		} else if w != "" {
			d.Log("WARNING " + w)
		}
	}

	// 3. Carry the naps over.
	wm, err := loop.LoadWatermark(d.Inst.Root, id)
	if err != nil {
		return err
	}
	after := ""
	if wm.LastNap != nil {
		after = *wm.LastNap
	}
	r, err := loop.SyncNaps(fromStore, toStore, id, after, o.Force)
	if err != nil {
		return err
	}
	if r.Latest != nil {
		d.Log(fmt.Sprintf("%s: resuming from nap %s (%d naps, %d files copied %s → %s)", id, *r.Latest, len(r.Naps), r.Blobs, from, to))
	} else {
		d.Log(fmt.Sprintf("%s: no naps on %s; starting from the baseline", id, from))
	}

	// 4. Start at the destination.
	if to == place.Local {
		port, err := d.up(id, place.Stopped)
		if err != nil {
			return err
		}
		d.Log(fmt.Sprintf("%s: handed off to local · API http://127.0.0.1:%d", id, port))
		return nil
	}
	ok, err := d.remoteChange(id, "start", []string{"--desired-count", "1"})
	if err != nil {
		return err
	}
	if ok {
		d.Log(id + ": handed off to ECS (stormo -r to watch it come up)")
	} else {
		d.Log(fmt.Sprintf("%s: stopped on %s, naps are on %s; start it with `stormo start %s -r` (or `stormo handoff %s --to %s` to go back)", id, from, to, id, id, from))
	}
	return nil
}

// Learn harvests: snapshot now (local, if running), fold naps into proposals, report what needs review.
func Learn(where place.Where, id string, d *Deps) (*loop.DreamReport, error) {
	if where == place.Local && place.LocalStateOf(id, local.ComposeCommand).State == string(place.Running) {
		if err := local.NapNow(d.Inst, id); err != nil {
			d.Log(fmt.Sprintf("%s: nap-now failed (%v); dreaming from existing naps", id, err))
		}
	}
	s, err := d.store(where)
	if err != nil {
		return nil, err
	}
	r, err := loop.Dream(d.Inst, id, s)
	if err != nil {
		return nil, err
	}
	ledger, err := learning.LoadLedger(d.Inst.Root, id)
	if err != nil {
		return nil, err
	}
	pending := 0
	for _, e := range ledger {
		if e.Status == learning.Proposed {
			pending++
		}
	}
	if len(r.Naps) > 0 {
		cron := ""
		if r.CronProposal {
			cron = ", cron changed"
		}
		d.Log(fmt.Sprintf("%s: folded %d naps → %d new lessons, %d re-sighted, %d skill proposals%s", id, len(r.Naps), r.NewLearnings, r.Resighted, len(r.SkillProposals), cron))
	} else {
		d.Log(id + ": no new naps")
	}
	if pending > 0 || len(r.SkillProposals) > 0 {
		remote := ""
		if where == place.Remote {
			remote = " --remote"
		}
		d.Log(fmt.Sprintf("  review: stormo learn list %s --status proposed · stormo learn skills %s", id, id))
		d.Log(fmt.Sprintf("  then:   stormo learn accept|reject %s <id…> · stormo restart %s%s", id, id, remote))
	}
	return r, nil
}
