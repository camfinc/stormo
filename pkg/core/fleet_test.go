package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/place"
	"github.com/camfinc/stormo/pkg/version"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture is a tmp instance (acme) with two agents, a persona in a sibling repo and one in the instance.
func fixture(t *testing.T) (dir string, inst *instance.Instance) {
	t.Helper()
	t.Setenv("SWARM_SECRETS_FILE", "")
	t.Setenv("SWARM_REPOS_DIR", "")
	dir = t.TempDir()
	root := filepath.Join(dir, "acme-swarm")
	write(t, filepath.Join(root, "stormo.yaml"), "name: Acme Swarm\norg: Acme\nslug: acme\noffice:\n  clocks:\n    - { city: Lisbon, tz: Europe/Lisbon }\n  units:\n    sales: {hue: \"#2dd4bf\", wall: map, floor: wood, desk: {app: records, props: [globe, mug]}}\n")
	for _, u := range [][2]string{{"group", "Acme Group"}, {"sales", "Acme Sales"}, {"media", "Acme Media"}} {
		write(t, filepath.Join(root, "units", u[0], "unit.yaml"), fmt.Sprintf("id: %s\nname: %s\ndescription: %s desc\n", u[0], u[1], u[1]))
	}
	agent := func(id, unit, extra string) string {
		return fmt.Sprintf("id: %s\nname: %s\nunit: %s\nrole: Does %s things.\n", id, strings.ToUpper(id[:1])+id[1:], unit, id) +
			"engine:\n  kind: hermes\n  version: 0.21.5\n  model: openai/gpt-6-luna\n  local:\n    via: core\n    model: gpt-6-luna\n" +
			"learning:\n  nap_interval_seconds: 600\n" + extra
	}
	write(t, filepath.Join(root, "agents/atlas/agent.yaml"), agent("atlas", "sales", "persona:\n  repo: brand-assets\n  path: bots/personas/atlas\n"+
		"channels:\n  - kind: slack\n  - kind: telegram\nsecrets: [SLACK_BOT_TOKEN, SLACK_APP_TOKEN]\noptional_secrets:\n  - name: TELEGRAM_BOT_TOKEN\n"))
	write(t, filepath.Join(root, "agents/atlas/SOUL.md"), "# Atlas\n")
	write(t, filepath.Join(root, "agents/nova/agent.yaml"), agent("nova", "media", "persona:\n  path: personas/nova\n"))
	write(t, filepath.Join(root, "personas/nova/persona.md"), "---\ntype: bot-persona\nbrand: acme\ndesk:\n  app: design\n  screens: 2\n"+
		"  props: [tablet, camera, \"Bad Name\", \"x;}body{display:none\", swatches, a, b, c, d, e]\n  side: ring-light\n---\n\n# Nova\n\n---\n\ndesk: not-frontmatter\n")
	write(t, filepath.Join(root, "agents/nova/SOUL.md"), "# Nova\n")
	ledger := func(status string, n int) string {
		return fmt.Sprintf(`{"id":"l%d","kind":"memory","scope":"agent","agent":"atlas","unit":"sales","text":"x","pii":[],"status":"%s"}`, n, status)
	}
	write(t, filepath.Join(root, "agents/atlas/learnings/ledger.jsonl"), ledger("proposed", 1)+"\n"+ledger("accepted", 2)+"\n"+ledger("proposed", 3)+"\n")
	write(t, filepath.Join(root, "agents/atlas/learnings/proposals/skills/quote-check/.proposal.json"), `{"skill":"quote-check"}`)
	avatars := filepath.Join(dir, "brand-assets/bots/personas/atlas/avatar")
	write(t, filepath.Join(avatars, "atlas-avatar-v1.png"), "v1")
	write(t, filepath.Join(avatars, "atlas-avatar-v2.png"), "v2-newest")
	write(t, filepath.Join(avatars, "atlas-avatar-v10.prompt.txt"), "not an image")
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return dir, inst
}

func ps(id string) (place.LocalState, error) {
	if id == "atlas" {
		return place.LocalState{State: "running", Health: "healthy"}, nil
	}
	return place.LocalState{State: "stopped"}, nil
}

func find(s FleetSnapshot, id string) *FleetAgent {
	for i := range s.Agents {
		if s.Agents[i].ID == id {
			return &s.Agents[i]
		}
	}
	return nil
}

func TestFleetRegistry(t *testing.T) {
	dir, inst := fixture(t)
	f := NewFleet(FleetOptions{Inst: inst, ReposDir: dir, Ps: ps})
	if f.Snapshot().PolledAt != nil {
		t.Error("polled before refresh")
	}
	f.Refresh()
	s := f.Snapshot()
	if s.PolledAt == nil {
		t.Fatal("not polled")
	}
	units := []string{}
	for _, u := range s.Units {
		units = append(units, u.ID)
	}
	if !slices.Equal(units, []string{"group", "media", "sales"}) {
		t.Errorf("units = %v", units)
	}
	a := find(s, "atlas")
	if a.Name != "Atlas" || a.Unit != "sales" || a.State != "running" || a.Health != "healthy" || a.Model != "openai/gpt-6-luna" || a.LocalModel != "gpt-6-luna" ||
		!a.Avatar || a.NapIntervalSeconds != 600 || a.PendingLearnings != 2 || a.PendingSkills != 1 ||
		!slices.Equal(a.Channels, []string{"slack", "telegram"}) || !slices.Equal(a.OptionalChannels, []string{"telegram"}) || a.Desk != nil {
		t.Errorf("atlas = %+v", a)
	}
	if !regexp.MustCompile(`^http://127\.0\.0\.1:\d+$`).MatchString(a.Endpoint) {
		t.Errorf("endpoint = %s", a.Endpoint)
	}
	// In-instance persona (no repo) resolves against the root; desk names are whitelisted.
	l := find(s, "nova")
	want := &Desk{App: "design", Screens: 2, Side: "ring-light", Props: []string{"tablet", "camera", "swatches", "a", "b", "c"}}
	if l.State != "stopped" || l.Avatar || l.PendingLearnings != 0 || !reflect.DeepEqual(l.Desk, want) {
		t.Errorf("nova = %+v desk %+v", l, l.Desk)
	}
	if !strings.HasSuffix(f.AvatarPath("atlas"), "atlas-avatar-v2.png") || f.AvatarPath("nova") != "" || f.AvatarPath("../etc") != "" {
		t.Error("avatar paths")
	}
}

func TestFailingPollerMarksUnknown(t *testing.T) {
	dir, inst := fixture(t)
	f := NewFleet(FleetOptions{Inst: inst, ReposDir: dir, Ps: func(string) (place.LocalState, error) { return place.LocalState{}, fmt.Errorf("compose exploded") }})
	f.Refresh()
	if a := find(f.Snapshot(), "atlas"); a.State != "unknown" || a.Detail != "compose exploded" {
		t.Errorf("atlas = %+v", a)
	}
}

func TestPollThatFailsKeepsLastSnapshot(t *testing.T) {
	dir, inst := fixture(t)
	missing := *inst
	missing.Root = filepath.Join(dir, "no-such-repo")
	f := NewFleet(FleetOptions{Inst: &missing, ReposDir: dir, Ps: ps})
	f.Refresh()
	s := f.Snapshot()
	if s.PolledAt != nil || len(s.Units) != 0 || len(s.Agents) != 0 {
		t.Errorf("s = %+v", s)
	}
}

func TestConcurrentRefreshesShareOnePoll(t *testing.T) {
	dir, inst := fixture(t)
	var calls atomic.Int32
	f := NewFleet(FleetOptions{Inst: inst, ReposDir: dir, Ps: func(id string) (place.LocalState, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return ps(id)
	}})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.Refresh() }()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Errorf("calls = %d", calls.Load())
	}
}

func TestDeskFrontmatter(t *testing.T) {
	if d := ReadDesk("---\nbrand: x\ndesk:\n  app: 3\n  screens: 5\n  props: oops\n  side: [x]\n---\n"); !reflect.DeepEqual(d, &Desk{Props: []string{}}) {
		t.Errorf("bad types = %+v", d)
	}
	if ReadDesk("---\nbrand: x\n---\n") != nil {
		t.Error("no desk")
	}
	if d := ReadDesk("---\ndesk:\n  app: [inbox, \"Bad!\", charts, code]\n---\n"); !reflect.DeepEqual(d, &Desk{Props: []string{}, App: "inbox", Apps: []string{"inbox", "charts"}}) {
		t.Errorf("apps = %+v", d)
	}
	if ReadDesk("# no frontmatter\n\n---\ndesk:\n  app: code\n---\n") != nil || ReadDesk("---\ndesk: [unclosed\n---\n") != nil {
		t.Error("frontmatter")
	}
}

func TestSpriteFrontmatter(t *testing.T) {
	md := "---\nbrand: x\nsprite:\n  skin: \"#C08A62\"\n  hair: red\n  shirt: \"#1e3a8a;background:url(x)\"\n  pants: \"#111827\"\n  hair_style: fade\n  accessory: crown\n---\n"
	if s := ReadSprite(md); !reflect.DeepEqual(s, &Sprite{Skin: "#c08a62", Pants: "#111827", HairStyle: "fade"}) {
		t.Errorf("sprite = %+v", s)
	}
	if ReadSprite("---\nbrand: x\n---\n") != nil {
		t.Error("no sprite")
	}
}

func decode(t *testing.T, s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestActivityFrom(t *testing.T) {
	now := time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)
	health := decode(t, `{"active_agents":1,"gateway_busy":true,"platforms":{"slack":{"state":"connected","needs_attention":false},"telegram":{"state":"fatal error!","needs_attention":true},"Bad Name":{}}}`)
	sessions := decode(t, `{"data":[{"source":"cron","last_active":1791337000,"title":"SECRET-TITLE","preview":"SECRET-PREVIEW client Juan"},{"source":"slack","last_active":1791337630.5,"title":"SECRET-TITLE","preview":"SECRET-PREVIEW"}]}`)
	a := ActivityFrom(health, sessions, now, nil)
	want := &Activity{ActiveAgents: 1, GatewayBusy: true, Source: "slack", RunningJobs: []string{}, LastActive: loop.IsoMillis(time.UnixMilli(1791337630500)),
		Platforms: map[string]Platform{"slack": {"connected", false}, "telegram": {"unknown", true}}, PolledAt: loop.IsoMillis(now)}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("a = %+v", a)
	}
	if b, _ := json.Marshal(a); strings.Contains(string(b), "SECRET") {
		t.Error("leaked")
	}
	if ActivityFrom(nil, sessions, now, nil) != nil {
		t.Error("nil health")
	}
	if a := ActivityFrom(decode(t, `{"active_agents":0}`), decode(t, `{"data":[]}`), now, nil); a.ActiveAgents != 0 || a.GatewayBusy || a.Source != "" {
		t.Errorf("empty = %+v", a)
	}
}

type fakeResp struct {
	status int
	body   string
}

func respond(r fakeResp) *http.Response {
	return &http.Response{StatusCode: r.status, Body: io.NopCloser(strings.NewReader(r.body)), Header: http.Header{}}
}

func TestActivityPoll(t *testing.T) {
	dir, inst := fixture(t)
	const key = "api-server-key-0123456789"
	var mu sync.Mutex
	calls := []string{}
	f := NewFleet(FleetOptions{Inst: inst, ReposDir: dir, Ps: ps,
		APIKey: func(id string) string {
			if id == "atlas" {
				return key
			}
			return ""
		},
		ActivityFetch: func(_ context.Context, url string, h http.Header) (*http.Response, error) {
			mu.Lock()
			calls = append(calls, url)
			mu.Unlock()
			if h.Get("Authorization") != "Bearer "+key {
				t.Errorf("auth = %q", h.Get("Authorization"))
			}
			if strings.HasSuffix(url, "/health/detailed") {
				return respond(fakeResp{200, `{"active_agents":1,"gateway_busy":false,"platforms":{}}`}), nil
			}
			return respond(fakeResp{200, `{"data":[{"source":"slack","last_active":1791337630,"preview":"SECRET-PREVIEW"}]}`}), nil
		}})
	f.Refresh()
	f.PollActivity()
	paths := []string{}
	for _, u := range calls {
		if !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Errorf("url %s", u)
		}
		paths = append(paths, regexp.MustCompile(`^http://127\.0\.0\.1:\d+`).ReplaceAllString(u, ""))
	}
	sort.Strings(paths)
	if !slices.Equal(paths, []string{"/api/jobs", "/api/sessions?limit=5", "/health/detailed"}) { // nova is stopped
		t.Errorf("paths = %v", paths)
	}
	s := f.Snapshot()
	if a := find(s, "atlas").Activity; a == nil || a.ActiveAgents != 1 || a.Source != "slack" {
		t.Errorf("atlas activity = %+v", a)
	}
	if find(s, "nova").Activity != nil {
		t.Error("nova has activity")
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), key) {
		t.Error("leaked")
	}
}

func TestActivityPollFailureClears(t *testing.T) {
	dir, inst := fixture(t)
	f := NewFleet(FleetOptions{Inst: inst, ReposDir: dir, Ps: ps, APIKey: func(string) string { return "api-server-key-0123456789" },
		ActivityFetch: func(context.Context, string, http.Header) (*http.Response, error) {
			return nil, fmt.Errorf("ECONNREFUSED")
		}})
	f.Refresh()
	f.PollActivity()
	if find(f.Snapshot(), "atlas").Activity != nil {
		t.Error("activity kept")
	}
}

func TestRunningJobs(t *testing.T) {
	// Observed on Hermes 0.21.5: the SLA watchdog (every 2 min) ran 02:50:34-02:52:47.
	job := func(next string, last any) any {
		l, _ := json.Marshal(last)
		return decode(t, fmt.Sprintf(`{"jobs":[{"id":"5f3c216ed926","name":"SLA watchdog <b>","enabled":true,"schedule":{"kind":"interval","minutes":2},"next_run_at":%q,"last_run_at":%s}]}`, next, l))
	}
	at := func(hms string) float64 {
		tm, _ := time.Parse(time.RFC3339, "2026-10-07T"+hms+"Z")
		return float64(tm.UnixMilli())
	}
	memo := map[string]*JobMemo{}
	check := func(got []string, want ...string) {
		t.Helper()
		if want == nil {
			want = []string{}
		}
		if !slices.Equal(got, want) {
			t.Errorf("got %v want %v", got, want)
		}
	}
	// first run after a restart: no last_run_at, so only a move of next_run_at tells
	check(RunningJobs(job("2026-10-07T02:49:34+00:00", nil), memo, at("02:49:00")))
	check(RunningJobs(job("2026-10-07T02:52:34+00:00", nil), memo, at("02:50:41")), "SLA watchdog b")
	check(RunningJobs(job("2026-10-07T02:54:47+00:00", "2026-10-07T02:52:47+00:00"), memo, at("02:52:48")))
	// a core started mid-run: the interval rule alone sees it
	check(RunningJobs(job("2026-10-07T02:56:47+00:00", "2026-10-07T02:52:47+00:00"), map[string]*JobMemo{}, at("02:55:10")), "SLA watchdog b")
	check(RunningJobs(job("2026-10-07T02:56:47+00:00", "2026-10-07T02:52:47+00:00"), map[string]*JobMemo{}, at("02:54:00")))
	if got := LastJobRun(decode(t, `{"jobs":[{"last_run_at":"2026-10-07T02:52:35+00:00"},{"last_run_at":"2026-10-07T02:52:47+00:00"},{"last_run_at":null}]}`)); got != "2026-10-07T02:52:47.000Z" {
		t.Errorf("last = %s", got)
	}
	if LastJobRun(nil) != "" {
		t.Error("last of nil")
	}
	// disabled jobs and junk are ignored
	check(RunningJobs(decode(t, `{"jobs":[{"id":"x","enabled":false,"name":"off"},null,3]}`), map[string]*JobMemo{}, 0))
	check(RunningJobs(nil, map[string]*JobMemo{}, 0))
}

func napOf(id string, files [][3]string) *loop.Nap {
	n := &loop.Nap{ID: id, Agent: "atlas", Unit: "sales", Engine: "hermes", Instance: "i", TakenAt: "2026-10-07T03:0" + id + ":00Z", Reason: "interval"}
	for _, f := range files {
		n.Files = append(n.Files, loop.NapFile{Path: f[0], Class: engine.FileClass(f[1]), Sha256: f[2], Size: 1})
	}
	return n
}

func TestDiffNaps(t *testing.T) {
	a := napOf("1", [][3]string{{"memories/MEMORY.md", "learning", "a"}, {"state.db", "state", "s1"}, {"sessions/SECRET-USER-u123.jsonl", "raw", "r1"}})
	b := napOf("2", [][3]string{{"memories/MEMORY.md", "learning", "a2"}, {"skills/x/SKILL.md", "learning", "k"}, {"state.db", "state", "s1"}, {"sessions/SECRET-USER-u456.jsonl", "raw", "r2"}})
	d := DiffNaps(a, b)
	if d.TakenAt != "2026-10-07T03:02:00Z" || d.Reason != "interval" || d.Changed != (NapChanges{2, 0, 1}) || d.Removed != 1 {
		t.Errorf("d = %+v", d)
	}
	if bs, _ := json.Marshal(d); strings.Contains(string(bs), "SECRET") {
		t.Error("leaked a path")
	}
}

func TestNapEvents(t *testing.T) {
	dir, inst := fixture(t)
	store := filepath.Join(inst.Root, ".swarm", "store", "atlas")
	put := func(id, sha string) {
		write(t, filepath.Join(store, "naps", id+".json"), fmt.Sprintf(`{"id":%q,"agent":"atlas","unit":"sales","engine":"hermes","instance":"i","takenAt":"t-%s","reason":"interval","baseline":null,"files":[{"path":"memories/MEMORY.md","class":"learning","sha256":%q,"size":1}]}`, id, id, sha))
		write(t, filepath.Join(store, "latest.json"), fmt.Sprintf(`{"nap":%q}`, id))
	}
	put("n1", "a")
	f := NewFleet(FleetOptions{Inst: inst, ReposDir: dir, Ps: ps})
	f.Refresh()
	if e := f.TakeNapEvents(); len(e) != 0 {
		t.Errorf("first nap is an event: %v", e)
	}
	put("n2", "b")
	f.Refresh()
	want := []NapEvent{{Agent: "atlas", TakenAt: "t-n2", Reason: "interval", Changed: NapChanges{Learning: 1}}}
	if e := f.TakeNapEvents(); !reflect.DeepEqual(e, want) {
		t.Errorf("events = %+v", e)
	}
	if e := f.TakeNapEvents(); len(e) != 0 {
		t.Error("events not taken")
	}
	f.Refresh()
	if e := f.TakeNapEvents(); len(e) != 0 {
		t.Error("same nap twice")
	}
}

func TestAPIKeysSurviveABrokenManifest(t *testing.T) {
	t.Setenv("SWARM_SECRETS_FILE", "")
	r := t.TempDir()
	write(t, filepath.Join(r, "stormo.yaml"), "slug: acme\n")
	for _, id := range []string{"group", "sales"} {
		write(t, filepath.Join(r, "units", id, "unit.yaml"), fmt.Sprintf("id: %s\nname: %s\ndescription: x\n", id, id))
	}
	write(t, filepath.Join(r, "agents/aaa/agent.yaml"), "id: aaa\nname: Aaa\nunit: nowhere\nengine: {kind: hermes, version: x, model: m}\n")
	write(t, filepath.Join(r, "agents/aaa/SOUL.md"), "#\n")
	write(t, filepath.Join(r, "agents/zed/agent.yaml"), "id: zed\nname: Zed\nunit: sales\nengine: {kind: hermes, version: x, model: m}\nsecrets: [API_SERVER_KEY]\n")
	write(t, filepath.Join(r, "agents/zed/SOUL.md"), "#\n")
	write(t, filepath.Join(r, "secrets.local.yaml"), "agents:\n  zed:\n    API_SERVER_KEY: zed-key-0123456789abcdef\n")
	inst, err := instance.Load(r)
	if err != nil {
		t.Fatal(err)
	}
	f := NewFleet(FleetOptions{Inst: inst, Ps: func(string) (place.LocalState, error) { return place.LocalState{State: "running"}, nil }})
	if k := f.apiKey("zed"); k != "zed-key-0123456789abcdef" {
		t.Errorf("zed = %q", k)
	}
	if k := f.apiKey("aaa"); k != "" {
		t.Errorf("aaa = %q", k)
	}
}

func TestNewestAvatarByNumber(t *testing.T) {
	d := filepath.Join(t.TempDir(), "persona-x")
	write(t, filepath.Join(d, "avatar/x-avatar-v9.png"), "9")
	write(t, filepath.Join(d, "avatar/x-avatar-v10.png"), "10")
	if !strings.HasSuffix(FindAvatar(d), "x-avatar-v10.png") || FindAvatar(filepath.Join(d, "nope")) != "" {
		t.Error("avatar")
	}
}

func TestOfficeRoutes(t *testing.T) {
	dir, inst := fixture(t)
	f := NewFleet(FleetOptions{Inst: inst, ReposDir: dir, Ps: ps})
	f.Refresh()
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, Fleet: f})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	get := func(p string) (*http.Response, string) {
		t.Helper()
		r, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}

	t.Run("/ serves the office, with its script and stylesheet", func(t *testing.T) {
		r, html := get("/")
		if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("status %d %s", r.StatusCode, r.Header.Get("Content-Type"))
		}
		// Named after the instance (stormo.yaml), whose clocks the page receives.
		// The office look per unit comes from stormo.yaml office.units, not from unit ids in the UI.
		if !strings.Contains(html, `"units":{"sales":{"hue":"#2dd4bf","wall":"map","floor":"wood","desk":{"app":"records","props":["globe","mug"]}}}`) {
			t.Errorf("page config lacks the office units: %s", html)
		}
		if !strings.Contains(html, "<title>Acme Swarm HQ</title>") || !strings.Contains(html, `"city":"Lisbon"`) {
			t.Error("not templated")
		}
		for _, asset := range []string{"/ui/app.js", "/ui/style.css"} {
			if !strings.Contains(html, asset) {
				t.Errorf("%s not linked", asset)
			}
			if r, _ := get(asset); r.StatusCode != 200 {
				t.Errorf("%s: %d", asset, r.StatusCode)
			}
		}
		for _, p := range []string{"/ui/../server.go", "/ui/fleet.go"} {
			if r, _ := get(p); r.StatusCode != 404 {
				t.Errorf("%s: %d", p, r.StatusCode)
			}
		}
	})

	t.Run("/api/fleet merges the registry with gateway status, without secrets", func(t *testing.T) {
		_, raw := get("/api/fleet")
		var body struct {
			Agents []struct {
				ID     string       `json:"id"`
				Office *OfficeState `json:"office"`
			} `json:"agents"`
			Units   []any          `json:"units"`
			Gateway map[string]any `json:"gateway"`
			Now     int64          `json:"now"`
			Robot   RobotState     `json:"robot"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		office := map[string]*OfficeState{}
		for _, a := range body.Agents {
			ids = append(ids, a.ID)
			office[a.ID] = a.Office
		}
		sort.Strings(ids)
		if !slices.Equal(ids, []string{"atlas", "nova"}) || len(body.Units) != 3 {
			t.Errorf("ids %v units %d", ids, len(body.Units))
		}
		g := body.Gateway
		if g["inflight"] != 0.0 || g["queued"] != 0.0 || g["concurrency"] != 3.0 || fmt.Sprint(g["usage"]) != "map[]" || fmt.Sprint(g["active"]) != "map[]" {
			t.Errorf("gateway = %v", g)
		}
		// The office simulation is the core's, shared by every viewer: server time and per-agent state.
		if d := body.Now - time.Now().UnixMilli(); d > 5000 || d < -5000 {
			t.Errorf("now off by %d", d)
		}
		if body.Robot.Phase != "docked" || body.Robot.Target != nil || len(body.Robot.Log) != 0 || len(body.Robot.Queue) != 0 {
			t.Errorf("robot = %+v", body.Robot)
		}
		if a := office["atlas"]; a == nil || a.Activity != "desk" || a.Phase != "seated" || a.Working {
			t.Errorf("atlas office = %+v", a)
		}
		if l := office["nova"]; l == nil || l.Activity != "away" {
			t.Errorf("nova office = %+v", l)
		}
		if regexp.MustCompile(`(?i)token"|refresh|access_token|SWARM_CORE_KEY`).MatchString(raw) {
			t.Error("secret-looking field in /api/fleet")
		}
		for _, k := range []string{`"polledAt"`, `"localModel"`, `"optionalChannels"`, `"napIntervalSeconds"`, `"pendingLearnings"`, `"pendingSkills"`, `"manageUsageUrl"`, `"planLimitedUntil"`} {
			if !strings.Contains(raw, k) {
				t.Errorf("missing %s", k)
			}
		}
	})

	t.Run("/api/core names the binary and the instance", func(t *testing.T) {
		r, raw := get("/api/core")
		var b coreView
		if err := json.Unmarshal([]byte(raw), &b); err != nil || r.StatusCode != 200 {
			t.Fatalf("%d %s %v", r.StatusCode, raw, err)
		}
		if b.Service != "swarm-core" || b.API != version.API || b.Version != version.String() || b.PID != os.Getpid() || b.Port != c.Addr.Port ||
			b.Exe == "" || b.StartedAt == "" || b.Instance != (coreInstance{inst.Root, "Acme Swarm", "Acme", "acme"}) {
			t.Errorf("%+v", b)
		}
	})

	t.Run("/api/instance is the page's config", func(t *testing.T) {
		_, raw := get("/api/instance")
		want := `{"name":"Acme Swarm","org":"Acme","slug":"acme","clocks":[{"city":"Lisbon","tz":"Europe/Lisbon"}],"units":{"sales":{"hue":"#2dd4bf","wall":"map","floor":"wood","desk":{"app":"records","props":["globe","mug"]}}}}`
		if raw != want {
			t.Errorf("got  %s\nwant %s", raw, want)
		}
	})

	t.Run("/avatars serves the persona's newest portrait, 404 otherwise", func(t *testing.T) {
		r, body := get("/avatars/atlas.png")
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" || body != "v2-newest" {
			t.Errorf("%d %s %q", r.StatusCode, r.Header.Get("Content-Type"), body)
		}
		for _, p := range []string{"/avatars/nova.png", "/avatars/..%2F..%2Fsecrets.png"} {
			if r, _ := get(p); r.StatusCode != 404 {
				t.Errorf("%s: %d", p, r.StatusCode)
			}
		}
	})

	t.Run("/health and unknown routes", func(t *testing.T) {
		r, body := get("/health")
		if r.StatusCode != 200 || !strings.Contains(body, `"service":"swarm-core"`) || !strings.Contains(body, `"login":"missing"`) ||
			!strings.Contains(body, fmt.Sprintf(`"api":%d`, version.API)) || !strings.Contains(body, `"version":"`) {
			t.Errorf("%d %s", r.StatusCode, body)
		}
		if strings.Contains(body, inst.Root) || strings.Contains(body, `"pid"`) {
			t.Errorf("/health reaches agent containers; no host paths in it: %s", body)
		}
		if r, body := get("/nope"); r.StatusCode != 404 || body != `{"error":{"code":"not_found","message":"GET /nope"}}` {
			t.Errorf("%d %s", r.StatusCode, body)
		}
	})
}
