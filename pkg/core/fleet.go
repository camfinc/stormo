package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/ops"
	"github.com/camfinc/stormo/pkg/place"
	"github.com/camfinc/stormo/pkg/secrets"
	"go.yaml.in/yaml/v3"
)

// The fleet registry behind the office UI (docs/core.md §2, phase 2). Manifests give the roster and
// the floor plan (one room per unit); a timer polls each agent's local compose project and caches
// the result. Requests only ever read the cache: compose ps is a subprocess.

type FleetUnit struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Desk is what sits on an agent's desk in the office UI, from its persona's `desk:` frontmatter.
type Desk struct {
	// Desk props in slot order (personas/README.md has the catalog).
	Props []string `json:"props"`
	// Screen app: records | inbox | code | design | charts | chat (the UI ignores names it does not draw).
	App string `json:"app,omitempty"`
	// One app per monitor, when the persona lists them; App is the first.
	Apps    []string `json:"apps,omitempty"`
	Screens int      `json:"screens,omitempty"`
	// One item on the floor beside the desk.
	Side string `json:"side,omitempty"`
}

// Sprite is how a persona's character is drawn in the office, from its `sprite:` frontmatter.
type Sprite struct {
	Skin      string `json:"skin,omitempty"`
	Hair      string `json:"hair,omitempty"`
	Shirt     string `json:"shirt,omitempty"`
	Pants     string `json:"pants,omitempty"`
	HairStyle string `json:"hair_style,omitempty"`
	Accessory string `json:"accessory,omitempty"`
}

// Platform is one messaging platform's state as the engine reports it.
type Platform struct {
	State          string `json:"state"`
	NeedsAttention bool   `json:"needsAttention"`
}

// Activity is what the agent is doing right now, from its engine's own API (Hermes
// `/health/detailed`, `/api/sessions`, `/api/jobs`). Counts, states and times only: session titles
// and previews hold client data and never leave the core.
type Activity struct {
	ActiveAgents int  `json:"activeAgents"`
	GatewayBusy  bool `json:"gatewayBusy"`
	// Source of the most recently active session: slack, cron, api_server, telegram, …
	Source string `json:"source,omitempty"`
	// Scheduled jobs running right now (their names: configuration, not client data).
	RunningJobs []string `json:"runningJobs"`
	// When a scheduled job last finished: idle time counts from this or the last session.
	LastJobAt  string              `json:"lastJobAt,omitempty"`
	LastActive string              `json:"lastActive,omitempty"`
	Platforms  map[string]Platform `json:"platforms"`
	PolledAt   string              `json:"polledAt"`
}

// FleetAgent is one desk of the office. Field names are read by the office UI.
type FleetAgent struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Unit       string   `json:"unit"`
	Role       string   `json:"role"`
	Model      string   `json:"model"`
	LocalModel string   `json:"localModel,omitempty"`
	Channels   []string `json:"channels"`
	// Channels that need an optional secret: live only when its value is set.
	OptionalChannels []string `json:"optionalChannels"`
	Avatar           bool     `json:"avatar"`
	Desk             *Desk    `json:"desk"`
	Sprite           *Sprite  `json:"sprite"`
	// nil when the agent is not running locally or its API did not answer.
	Activity *Activity `json:"activity"`
	// running | starting | stopped | exited | restarting | unknown
	State              string `json:"state"`
	Health             string `json:"health,omitempty"`
	Detail             string `json:"detail,omitempty"`
	Endpoint           string `json:"endpoint"`
	LastNap            string `json:"lastNap,omitempty"`
	NapIntervalSeconds int    `json:"napIntervalSeconds"`
	PendingLearnings   int    `json:"pendingLearnings"`
	PendingSkills      int    `json:"pendingSkills"`
}

// NapChanges is what a nap changed, as counts per file class only: paths can carry session or user ids.
type NapChanges struct {
	Learning int `json:"learning"`
	State    int `json:"state"`
	Raw      int `json:"raw"`
}

// NapEvent: a new nap landed in the store (the core's robot goes to note it).
type NapEvent struct {
	Agent   string     `json:"agent"`
	TakenAt string     `json:"takenAt"`
	Reason  string     `json:"reason"`
	Changed NapChanges `json:"changed"`
	Removed int        `json:"removed"`
}

// DiffNaps counts files added or changed per class, and files gone, between two naps of one agent.
func DiffNaps(prev, next *loop.Nap) NapEvent {
	before := map[string]string{}
	for _, f := range prev.Files {
		before[f.Path] = f.Sha256
	}
	var c NapChanges
	for _, f := range next.Files {
		if sha, ok := before[f.Path]; !ok || sha != f.Sha256 {
			switch f.Class {
			case "learning":
				c.Learning++
			case "state":
				c.State++
			case "raw":
				c.Raw++
			}
		}
		delete(before, f.Path)
	}
	return NapEvent{Agent: next.Agent, TakenAt: next.TakenAt, Reason: next.Reason, Changed: c, Removed: len(before)}
}

// FleetSnapshot is the cached registry.
type FleetSnapshot struct {
	PolledAt *string      `json:"polledAt"`
	Units    []FleetUnit  `json:"units"`
	Agents   []FleetAgent `json:"agents"`
}

// PsFn reports an agent's compose state.
type PsFn func(id string) (place.LocalState, error)

// ActivityFetch performs one GET against an agent's engine API.
type ActivityFetch func(ctx context.Context, url string, header http.Header) (*http.Response, error)

// FleetOptions configure a Fleet; zero values take the defaults.
type FleetOptions struct {
	Inst *instance.Instance
	// Where sibling repos named by `persona.repo` live (avatars). Default: SWARM_REPOS_DIR, else the
	// instance's parent.
	ReposDir         string
	Interval         time.Duration
	Ps               PsFn
	ActivityInterval time.Duration
	ActivityFetch    ActivityFetch
	// API keys per agent (default: API_SERVER_KEY from secrets.local.yaml, local overlay applied).
	APIKey func(id string) string
}

var (
	composeOnce sync.Once
	composeCmd  []string
	composeErr  error
)

// ComposePs is the default PsFn: `compose ps` of the agent's project.
func ComposePs(id string) (place.LocalState, error) {
	composeOnce.Do(func() { composeCmd, composeErr = local.ComposeCommand() })
	if composeErr != nil {
		return place.LocalState{State: "unknown", Detail: "docker compose not found"}, nil
	}
	return place.LocalStateOf(id, func() ([]string, error) { return composeCmd, nil }), nil
}

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	colorRe  = regexp.MustCompile(`^#[0-9a-f]{6}$`)
	sourceRe = regexp.MustCompile(`^[a-z_-]{1,24}$`)
	fmRe     = regexp.MustCompile(`^---\r?\n((?s:.*?))\r?\n---[ \t]*(?:\r?\n|$)`)
	jobName  = regexp.MustCompile(`[^\p{L}\p{N} ._:/()-]`)

	hairStyles  = map[string]bool{"short": true, "fade": true, "long": true, "updo": true, "curly": true, "bald": true}
	accessories = map[string]bool{"none": true, "headset": true, "glasses": true, "cap": true, "earrings": true}
)

const maxProps = 6

// frontmatterKey is one key of a persona.md YAML frontmatter, or nil.
func frontmatterKey(md, key string) any {
	m := fmRe.FindStringSubmatch(md)
	if m == nil {
		return nil
	}
	var fm map[string]any
	if yaml.Unmarshal([]byte(m[1]), &fm) != nil {
		return nil
	}
	return fm[key]
}

func validName(v any) string {
	if s, ok := v.(string); ok && nameRe.MatchString(s) {
		return s
	}
	return ""
}

// ReadDesk is the `desk:` block of a persona.md frontmatter, reduced to names the UI may turn into
// CSS classes. Anything else (bad types, odd names, extra props) is dropped rather than failing.
func ReadDesk(md string) *Desk {
	d, ok := frontmatterKey(md, "desk").(map[string]any)
	if !ok {
		return nil
	}
	desk := &Desk{Props: []string{}}
	if props, ok := d["props"].([]any); ok {
		for _, p := range props {
			if n := validName(p); n != "" && len(desk.Props) < maxProps {
				desk.Props = append(desk.Props, n)
			}
		}
	}
	if apps, ok := d["app"].([]any); ok {
		names := []string{}
		for _, a := range apps {
			if n := validName(a); n != "" && len(names) < 2 {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			desk.App, desk.Apps = names[0], names
		}
	} else if n := validName(d["app"]); n != "" {
		desk.App = n
	}
	if s, ok := d["screens"].(int); ok && (s == 1 || s == 2) {
		desk.Screens = s
	}
	desk.Side = validName(d["side"])
	return desk
}

// ReadSprite is the `sprite:` block of a persona.md frontmatter: colors as #rrggbb, hair style and
// accessory from short lists. They become inline CSS variables in the UI, so anything else is dropped.
func ReadSprite(md string) *Sprite {
	d, ok := frontmatterKey(md, "sprite").(map[string]any)
	if !ok {
		return nil
	}
	color := func(k string) string {
		if s, ok := d[k].(string); ok && colorRe.MatchString(strings.ToLower(s)) {
			return strings.ToLower(s)
		}
		return ""
	}
	sp := &Sprite{Skin: color("skin"), Hair: color("hair"), Shirt: color("shirt"), Pants: color("pants")}
	if s, ok := d["hair_style"].(string); ok && hairStyles[s] {
		sp.HairStyle = s
	}
	if s, ok := d["accessory"].(string); ok && accessories[s] {
		sp.Accessory = s
	}
	return sp
}

var avatarRe = regexp.MustCompile(`-avatar-v(\d+)\.png$`)

// FindAvatar is the newest baked avatar for a persona (`<dir>/avatar/<slug>-avatar-vN.png`), or "".
func FindAvatar(personaDir string) string {
	dir := filepath.Join(personaDir, "avatar")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestN := "", -1
	for _, e := range entries {
		if m := avatarRe.FindStringSubmatch(e.Name()); m != nil {
			if n, _ := strconv.Atoi(m[1]); n > bestN {
				best, bestN = e.Name(), n
			}
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(dir, best)
}

// JobMemo is what RunningJobs remembers about a job between polls.
type JobMemo struct {
	Next, Last string
	Running    bool
}

func parseMs(s string) (float64, bool) {
	if s == "" {
		return math.NaN(), false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return math.NaN(), false
	}
	return float64(t.UnixMilli()), true
}

func jobList(jobs any) []any {
	m, ok := jobs.(map[string]any)
	if !ok {
		return nil
	}
	l, _ := m["jobs"].([]any)
	return l
}

// RunningJobs tells which of an agent's scheduled jobs are running, from Hermes' `/api/jobs`. It
// has no running flag, but its scheduler (v0.21) moves `next_run_at` forward when a run starts and
// sets `last_run_at` when it finishes. So a job is running while (a) for an interval job, its last
// finish is earlier than the start implied by `next_run_at - interval`, or (b) we saw `next_run_at`
// move with no new `last_run_at`. (a) also covers a core started mid-run; (b) covers cron
// expressions and first runs. memo carries what (b) needs between polls. now is epoch ms.
func RunningJobs(jobs any, memo map[string]*JobMemo, now float64) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range jobList(jobs) {
		j, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, ok := j["id"].(string)
		if !ok || j["enabled"] == false {
			continue
		}
		seen[id] = true
		next, _ := j["next_run_at"].(string)
		last, _ := j["last_run_at"].(string)
		prev := memo[id]
		running := prev != nil && prev.Running
		if prev != nil && last != prev.Last {
			running = false // finished
		} else if prev != nil && next != "" && next != prev.Next && last == prev.Last {
			running = true // started
		}
		if sch, ok := j["schedule"].(map[string]any); ok && sch["kind"] == "interval" {
			if minutes, ok := sch["minutes"].(float64); ok && next != "" && last != "" {
				nextMs, _ := parseMs(next)
				lastMs, _ := parseMs(last)
				startedAt := nextMs - minutes*60_000
				running = lastMs < startedAt-5_000 && startedAt <= now // an unparsable time is NaN, which compares false
			}
		}
		memo[id] = &JobMemo{Next: next, Last: last, Running: running}
		name := ""
		if n, ok := j["name"].(string); ok {
			r := []rune(strings.TrimSpace(jobName.ReplaceAllString(n, "")))
			if len(r) > 60 {
				r = r[:60]
			}
			name = string(r)
		}
		if running && name != "" {
			out = append(out, name)
		}
	}
	for id := range memo {
		if !seen[id] {
			delete(memo, id)
		}
	}
	return out
}

// LastJobRun is the latest `last_run_at` among an agent's scheduled jobs, or "".
func LastJobRun(jobs any) string {
	best, found := 0.0, false
	for _, raw := range jobList(jobs) {
		j, _ := raw.(map[string]any)
		s, _ := j["last_run_at"].(string)
		if t, ok := parseMs(s); ok && (!found || t > best) {
			best, found = t, true
		}
	}
	if !found {
		return ""
	}
	return loop.IsoMillis(time.UnixMilli(int64(best)))
}

// ActivityFrom reads Hermes' `/health/detailed` and `/api/sessions` bodies; nil if not usable.
func ActivityFrom(health, sessions any, now time.Time, jobs []string) *Activity {
	h, ok := health.(map[string]any)
	if !ok {
		return nil
	}
	if jobs == nil {
		jobs = []string{}
	}
	a := &Activity{RunningJobs: jobs, Platforms: map[string]Platform{}, PolledAt: loop.IsoMillis(now)}
	if ps, ok := h["platforms"].(map[string]any); ok {
		for name, v := range ps {
			p, ok := v.(map[string]any)
			if !sourceRe.MatchString(name) || !ok {
				continue
			}
			state := "unknown"
			if s, ok := p["state"].(string); ok && sourceRe.MatchString(s) {
				state = s
			}
			a.Platforms[name] = Platform{State: state, NeedsAttention: p["needs_attention"] == true}
		}
	}
	var newest map[string]any
	if s, ok := sessions.(map[string]any); ok {
		list, _ := s["data"].([]any)
		for _, raw := range list {
			x, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			la, ok := x["last_active"].(float64)
			if !ok {
				continue
			}
			if newest == nil || la > newest["last_active"].(float64) {
				newest = x
			}
		}
	}
	if n, ok := h["active_agents"].(float64); ok {
		a.ActiveAgents = int(n)
	}
	a.GatewayBusy = h["gateway_busy"] == true
	if newest != nil {
		if src, ok := newest["source"].(string); ok && sourceRe.MatchString(src) {
			a.Source = src
		}
		a.LastActive = loop.IsoMillis(time.UnixMilli(int64(newest["last_active"].(float64) * 1000)))
	}
	return a
}

type apiKeys struct {
	mtime  time.Time
	loaded bool
	at     time.Time
	values map[string]string
}

// Fleet polls the instance's agents; safe for concurrent use.
type Fleet struct {
	o        FleetOptions
	root     string
	reposDir string

	mu       sync.Mutex
	snap     FleetSnapshot
	activity map[string]*Activity
	jobMemo  map[string]map[string]*JobMemo
	naps     map[string]*loop.Nap
	events   []NapEvent
	avatars  map[string]string
	keys     apiKeys

	pollMu   sync.Mutex
	running  chan struct{}
	actMu    sync.Mutex
	actBusy  bool
	stop     chan struct{}
	stopOnce sync.Once
}

// NewFleet builds a registry; call Start for the timers or Refresh / PollActivity by hand.
func NewFleet(o FleetOptions) *Fleet {
	f := &Fleet{o: o, root: o.Inst.Root, activity: map[string]*Activity{}, jobMemo: map[string]map[string]*JobMemo{},
		naps: map[string]*loop.Nap{}, avatars: map[string]string{}, stop: make(chan struct{}),
		snap: FleetSnapshot{Units: []FleetUnit{}, Agents: []FleetAgent{}}}
	f.reposDir = o.ReposDir
	if f.reposDir == "" {
		f.reposDir = os.Getenv("SWARM_REPOS_DIR")
	}
	if f.reposDir == "" {
		f.reposDir = filepath.Dir(f.root)
	}
	if f.o.Ps == nil {
		f.o.Ps = ComposePs
	}
	if f.o.Interval == 0 {
		f.o.Interval = 10 * time.Second
	}
	if f.o.ActivityInterval == 0 {
		f.o.ActivityInterval = 3 * time.Second
	}
	return f
}

// Start polls in the background until Stop.
func (f *Fleet) Start() *Fleet {
	go f.Refresh()
	go func() {
		poll := time.NewTicker(f.o.Interval)
		act := time.NewTicker(f.o.ActivityInterval)
		defer poll.Stop()
		defer act.Stop()
		for {
			select {
			case <-f.stop:
				return
			case <-poll.C:
				go f.Refresh()
			case <-act.C:
				go f.PollActivity()
			}
		}
	}()
	return f
}

func (f *Fleet) Stop() { f.stopOnce.Do(func() { close(f.stop) }) }

// Snapshot is the cached registry with the latest activity.
func (f *Fleet) Snapshot() FleetSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := FleetSnapshot{PolledAt: f.snap.PolledAt, Units: append([]FleetUnit{}, f.snap.Units...), Agents: make([]FleetAgent, len(f.snap.Agents))}
	for i, a := range f.snap.Agents {
		a.Activity = f.activity[a.ID]
		out.Agents[i] = a
	}
	return out
}

// APIKey is the agent's API_SERVER_KEY (its engine API), for the core's own calls. Never sent on.
func (f *Fleet) APIKey(id string) string { return f.apiKey(id) }

// apiKey is API_SERVER_KEY per agent from secrets.local.yaml, reread when the file changes. Never
// leaves the core.
func (f *Fleet) apiKey(id string) string {
	if f.o.APIKey != nil {
		return f.o.APIKey(id)
	}
	path := secrets.Path(f.root)
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	f.mu.Lock()
	_, have := f.keys.values[id]
	// Reload when the file changes, and every 30 s while an agent has no key (its manifest may
	// have been mid-edit, or it joined the fleet after the last load).
	reload := !f.keys.loaded || !st.ModTime().Equal(f.keys.mtime) || (!have && time.Since(f.keys.at) > 30*time.Second)
	f.mu.Unlock()
	if reload {
		values := map[string]string{}
		if file, err := secrets.Load(path); err == nil {
			for _, aid := range manifest.AgentIDs(f.root) {
				a, err := manifest.Load(f.root, aid, f.o.Inst.Names.Secret)
				if err != nil {
					continue // this agent's manifest does not load right now; the others still get keys
				}
				if k, _ := secrets.Resolve(file, a, manifest.Local).Values.Get("API_SERVER_KEY"); k != "" {
					values[aid] = k
				}
			}
		} // unreadable or invalid secrets file: no activity until it is fixed
		f.mu.Lock()
		f.keys = apiKeys{mtime: st.ModTime(), loaded: true, at: time.Now(), values: values}
		f.mu.Unlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys.values[id]
}

func (f *Fleet) fetchJSON(ctx context.Context, url, key string) (any, error) {
	fetch := f.o.ActivityFetch
	if fetch == nil {
		fetch = func(ctx context.Context, url string, h http.Header) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			req.Header = h
			return http.DefaultClient.Do(req)
		}
	}
	res, err := fetch(ctx, url, http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, nil
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// PollActivity is one activity round over the agents the last compose poll saw running.
func (f *Fleet) PollActivity() {
	f.actMu.Lock()
	if f.actBusy {
		f.actMu.Unlock()
		return
	}
	f.actBusy = true
	f.actMu.Unlock()
	defer func() { f.actMu.Lock(); f.actBusy = false; f.actMu.Unlock() }()

	agents := f.Snapshot().Agents
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a FleetAgent) {
			defer wg.Done()
			set := func(act *Activity) {
				f.mu.Lock()
				defer f.mu.Unlock()
				if act == nil {
					delete(f.activity, a.ID)
				} else {
					f.activity[a.ID] = act
				}
			}
			if a.State != "running" && a.State != "starting" {
				set(nil)
				return
			}
			key := f.apiKey(a.ID)
			// Hermes only serves its API with a key of 16+ characters.
			if len(key) < 16 {
				set(nil)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var h, s, j any
			var herr, serr error
			var inner sync.WaitGroup
			inner.Add(3)
			go func() { defer inner.Done(); h, herr = f.fetchJSON(ctx, a.Endpoint+"/health/detailed", key) }()
			go func() { defer inner.Done(); s, serr = f.fetchJSON(ctx, a.Endpoint+"/api/sessions?limit=5", key) }()
			// Scheduled jobs (no_agent scripts run outside any agent turn): optional, never fatal.
			go func() { defer inner.Done(); j, _ = f.fetchJSON(ctx, a.Endpoint+"/api/jobs", key) }()
			inner.Wait()
			if herr != nil || serr != nil {
				set(nil) // down, starting or timed out; never log the body
				return
			}
			f.mu.Lock()
			memo := f.jobMemo[a.ID]
			if memo == nil {
				memo = map[string]*JobMemo{}
				f.jobMemo[a.ID] = memo
			}
			jobs := RunningJobs(j, memo, float64(time.Now().UnixMilli()))
			f.mu.Unlock()
			act := ActivityFrom(h, s, time.Now(), jobs)
			if act != nil {
				act.LastJobAt = LastJobRun(j)
			}
			set(act)
		}(a)
	}
	wg.Wait()
}

// TakeNapEvents returns new naps since the last call (the first nap seen for an agent, e.g. after
// a core restart, is not one).
func (f *Fleet) TakeNapEvents() []NapEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.events
	f.events = nil
	if out == nil {
		out = []NapEvent{}
	}
	return out
}

// AvatarPath is the absolute path of an agent's avatar PNG, only for agents in the roster.
func (f *Fleet) AvatarPath(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.avatars[id]
}

// Refresh runs one poll; concurrent callers share the one in flight. A failed poll keeps the last
// snapshot and is only logged.
func (f *Fleet) Refresh() {
	f.pollMu.Lock()
	if f.running != nil {
		ch := f.running
		f.pollMu.Unlock()
		<-ch
		return
	}
	ch := make(chan struct{})
	f.running = ch
	f.pollMu.Unlock()
	defer func() {
		f.pollMu.Lock()
		f.running = nil
		f.pollMu.Unlock()
		close(ch)
	}()
	if err := f.poll(); err != nil {
		log.Printf("fleet poll failed: %s", strings.SplitN(err.Error(), "\n", 2)[0])
	}
}

func (f *Fleet) poll() error {
	root := f.root
	for _, d := range []string{"units", "agents"} {
		if _, err := os.ReadDir(filepath.Join(root, d)); err != nil {
			return err
		}
	}
	units := []FleetUnit{}
	ids := manifest.UnitIDs(root)
	sort.Strings(ids)
	for _, id := range ids {
		if u, err := manifest.LoadUnit(root, id); err == nil {
			units = append(units, FleetUnit{ID: u.ID, Name: u.Name, Description: u.Description})
		} else {
			units = append(units, FleetUnit{ID: id, Name: id})
		}
	}
	store := ops.LocalStore(root)
	agentIDs := manifest.AgentIDs(root)
	results := make([]*FleetAgent, len(agentIDs))
	var wg sync.WaitGroup
	for i, id := range agentIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			results[i] = f.pollAgent(id, store)
		}(i, id)
	}
	wg.Wait()
	agents := []FleetAgent{}
	for _, a := range results {
		if a != nil {
			agents = append(agents, *a)
		}
	}
	now := loop.IsoMillis(time.Now())
	f.mu.Lock()
	f.snap = FleetSnapshot{PolledAt: &now, Units: units, Agents: agents}
	f.mu.Unlock()
	return nil
}

func (f *Fleet) pollAgent(id string, store loop.Store) *FleetAgent {
	root := f.root
	m, err := manifest.Load(root, id, f.o.Inst.Names.Secret)
	if err != nil {
		log.Printf("fleet: %s: %s", id, strings.SplitN(err.Error(), "\n", 2)[0])
		return nil
	}
	s, err := f.o.Ps(id)
	if err != nil {
		s = place.LocalState{State: "unknown", Detail: err.Error()}
	}
	nap, _ := loop.Latest(store, id)
	ledger, _ := learning.LoadLedger(root, id)
	skills, _ := loop.ListSkillProposals(root, id)

	// A persona in this instance (personas/<slug>) resolves against the root, whatever the checkout is called.
	personaDir := ""
	if m.Persona != nil {
		if manifest.PersonaInInstance(*m.Persona, f.o.Inst.Names.Resource) {
			personaDir = filepath.Join(root, m.Persona.Path)
		} else {
			personaDir = filepath.Join(f.reposDir, m.Persona.Repo, m.Persona.Path)
		}
	}
	avatar := ""
	var desk *Desk
	var sprite *Sprite
	if personaDir != "" {
		avatar = FindAvatar(personaDir)
		if md, err := os.ReadFile(filepath.Join(personaDir, "persona.md")); err == nil {
			desk, sprite = ReadDesk(string(md)), ReadSprite(string(md))
		}
	}
	f.mu.Lock()
	if nap != nil {
		if seen := f.naps[id]; seen != nil && seen.ID != nap.ID {
			e := DiffNaps(seen, nap)
			e.Agent = id
			f.events = append(f.events, e)
		}
		f.naps[id] = nap
	}
	if avatar != "" {
		f.avatars[id] = avatar
	} else {
		delete(f.avatars, id)
	}
	f.mu.Unlock()

	channels, optional := []string{}, []string{}
	for _, c := range m.Channels {
		channels = append(channels, c.Kind)
		for _, n := range manifest.ChannelSecrets[c.Kind] {
			gated := false
			for _, o := range m.OptionalSecrets {
				if o.Name == n {
					gated = true
				}
			}
			if gated {
				optional = append(optional, c.Kind)
				break
			}
		}
	}
	endpoint := ""
	if port, err := local.Port(root, id); err == nil {
		endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	a := &FleetAgent{
		ID: id, Name: m.Name, Unit: m.Unit, Role: m.Role, Model: m.Engine.Model,
		Channels: channels, OptionalChannels: optional, Avatar: avatar != "", Desk: desk, Sprite: sprite,
		State: s.State, Health: s.Health, Detail: s.Detail, Endpoint: endpoint,
		NapIntervalSeconds: m.Learning.NapIntervalSeconds, PendingSkills: len(skills),
	}
	if m.Engine.Local != nil {
		a.LocalModel = m.Engine.Local.Model
	}
	if nap != nil {
		a.LastNap = nap.TakenAt
	}
	for _, e := range ledger {
		if e.Status == learning.Proposed {
			a.PendingLearnings++
		}
	}
	return a
}
