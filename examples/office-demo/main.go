// Command office-demo serves the core's office UI for a made-up company (./instance) with
// simulated activity: every agent "running", some busy on a turn or a scheduled job, others idle,
// and a nap landing every few seconds so the sync robot makes its rounds. No container, model or
// secret is involved; it is the real core and office with the outside world faked.
//
//	go run ./examples/office-demo            # then open http://127.0.0.1:18700/
//	go run ./examples/office-demo -port 0    # any free port
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/place"
)

// What each agent is up to: busy on a turn (and from where), on a scheduled job, or idle for a while.
type mood struct {
	busy   string // session source while working: slack, api_server; "" when not
	job    string // a scheduled job running right now
	idle   time.Duration
	attend bool // a platform that needs attention
}

var moods = map[string]mood{
	"atlas": {busy: "slack"},
	"scout": {idle: 25 * time.Minute},
	"nova":  {busy: "slack"},
	"echo":  {job: "Morning ticket digest"},
	"pixel": {busy: "api_server"},
	"byte":  {idle: 2 * time.Hour},
	"sage":  {job: "Competitor price sweep", attend: true},
}

func main() {
	port := flag.Int("port", 18700, "port to serve on (127.0.0.1)")
	dir := flag.String("instance", "", "instance directory (default: ./instance next to this file)")
	flag.Parse()
	if *dir == "" {
		_, file, _, _ := runtime.Caller(0)
		*dir = filepath.Join(filepath.Dir(file), "instance")
	}
	inst, err := instance.Load(*dir)
	if err != nil {
		log.Fatal(err)
	}
	fleet := core.NewFleet(core.FleetOptions{
		Inst:             inst,
		Interval:         2 * time.Second,
		ActivityInterval: 2 * time.Second,
		Ps: func(string) (place.LocalState, error) {
			return place.LocalState{State: "running", Health: "healthy"}, nil
		},
		ActivityFetch: fakeEngine(inst),
		APIKey:        func(string) string { return "office-demo-api-key" }, // the core skips keys under 16 chars
	})
	c, err := core.StartCore(core.CoreOptions{Inst: inst, Port: *port, Fleet: fleet})
	if err != nil {
		log.Fatal(err)
	}
	fleet.Start()
	go napRounds(inst)
	fmt.Printf("office demo on http://%s/ (Ctrl-C to stop)\n", c.Addr)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fleet.Stop()
	c.Close()
}

// fakeEngine answers the engine API call the core makes per agent (needletail's GET /v1/activity),
// from its mood.
func fakeEngine(inst *instance.Instance) core.ActivityFetch {
	ports := map[string]string{}
	for _, id := range manifest.AgentIDs(inst.Root) {
		if p, err := local.Port(inst.Root, id); err == nil {
			ports[fmt.Sprintf(":%d/", p)] = id
		}
	}
	return func(_ context.Context, url string, _ http.Header) (*http.Response, error) {
		if !strings.Contains(url, "/v1/activity") {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
		}
		id := ""
		for k, v := range ports {
			if strings.Contains(url, k) {
				id = v
			}
		}
		m := moods[id]
		now := time.Now()
		a := wire.Activity{RunningJobs: []string{}, Source: m.busy, LastActive: now.Add(-m.idle).Format(time.RFC3339Nano),
			Platforms: map[string]wire.Platform{"slack": {State: "connected", NeedsAttention: m.attend}}}
		if m.busy != "" {
			a.ActiveTurns, a.Busy = 1, true
		} else {
			a.Source = "slack"
		}
		if m.job != "" {
			// An hourly job that started a minute ago; the one before finished two hours ago.
			a.Source, a.LastActive = "cron", now.Add(-time.Minute).Format(time.RFC3339Nano)
			a.RunningJobs, a.LastJobAt = []string{m.job}, now.Add(-2*time.Hour).Format(time.RFC3339Nano)
		}
		b, _ := json.Marshal(a)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	}
}

// napRounds lands a synthetic nap for one agent after another, so the robot visits their desks.
// Every agent gets one first: the core does not count the first nap it sees as news.
func napRounds(inst *instance.Instance) {
	store := loop.FsStore{Root: filepath.Join(inst.Root, ".swarm", "store")}
	ids := manifest.AgentIDs(inst.Root)
	for i := 0; ; i++ {
		id := ids[i%len(ids)]
		now := time.Now()
		files := []loop.NapFile{
			{Path: "memories/MEMORY.md", Class: engine.Learning, Sha256: fmt.Sprintf("m%d", i), Size: 1},
			{Path: "skills/notes/SKILL.md", Class: engine.Learning, Sha256: fmt.Sprintf("s%d", i/3), Size: 1},
			{Path: "sessions/demo.jsonl", Class: engine.Raw, Sha256: fmt.Sprintf("r%d", i), Size: 1},
		}
		n := loop.Nap{ID: strings.NewReplacer("-", "", ":", "", ".", "").Replace(loop.IsoMillis(now)) + fmt.Sprintf("-demo%d", i), Agent: id,
			Engine: "needletail", Instance: "demo", TakenAt: loop.IsoMillis(now), Reason: "interval", Files: files}
		body, _ := learning.MarshalIndent(n)
		_ = store.Put(loop.NapKey(id, n.ID), body)
		ptr, _ := learning.MarshalCompact(map[string]string{"nap": n.ID})
		_ = store.Put(id+"/latest.json", ptr)
		if i < len(ids) {
			continue
		}
		time.Sleep(4 * time.Second)
	}
}
