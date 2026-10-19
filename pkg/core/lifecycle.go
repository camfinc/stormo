package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/link"
	"github.com/camfinc/stormo/pkg/ops"
	"github.com/camfinc/stormo/pkg/place"
)

// Lifecycle through the core (docs/remote-core.md §6a): start, stop and restart an agent in a
// placement, whoever asks (the app, a remote client) and wherever the agent runs. local runs
// docker compose on the core's host; remote (ECS) calls the AWS API with the core's credentials
// and needs an explicit confirmation of the exact plan; a runner placement goes to the runner
// serving it. Every call needs the operate scope and lands in the audit log.

var lifecyclePath = regexp.MustCompile(`^/api/agents/([a-z][a-z0-9-]*)/(start|stop|restart)$`)

// LifecycleOps run lifecycle actions; nil fields take the defaults (pkg/ops).
type LifecycleOps struct {
	// Local acts on the core's own host.
	Local func(action, id string, log func(string)) error
	// Remote acts on ECS; confirm sees the plan (false: only show it).
	Remote func(action, id string, confirm func(plan []string) bool, log func(string)) error
}

func defaultLifecycle(inst *instance.Instance, restart func(id string) error) LifecycleOps {
	deps := func(log func(string), confirm func([]string) bool) *ops.Deps {
		d := ops.DefaultDeps(inst, true)
		d.Log = log
		if confirm != nil {
			d.Confirm = confirm
		}
		return d
	}
	return LifecycleOps{
		Local: func(action, id string, log func(string)) error {
			switch action {
			case "start":
				return ops.Start(place.Local, id, deps(log, nil))
			case "stop":
				return ops.Stop(place.Local, id, deps(log, nil))
			}
			return restart(id)
		},
		Remote: func(action, id string, confirm func([]string) bool, log func(string)) error {
			d := deps(log, confirm)
			switch action {
			case "start":
				return ops.Start(place.Remote, id, d)
			case "stop":
				return ops.Stop(place.Remote, id, d)
			}
			return ops.Restart(place.Remote, id, d)
		},
	}
}

// LifecycleResult is what a lifecycle call did.
type LifecycleResult struct {
	Action    string   `json:"action"`
	Agent     string   `json:"agent"`
	Placement string   `json:"placement"`
	Log       []string `json:"log"`
	State     string   `json:"state,omitempty"`
}

type lineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLog) add(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (c *Core) handleLifecycle(w http.ResponseWriter, r *http.Request, who Caller, id, action string, roster func() []BusAgent) {
	var req struct {
		Placement string `json:"placement"`
		Confirm   bool   `json:"confirm"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSONBody(w, 400, errBody("bad_request", "body is {placement, confirm}"))
			return
		}
	}
	known := false
	var running bool
	for _, a := range roster() {
		if a.ID == id {
			known, running = true, a.Running
		}
	}
	if !known {
		writeJSONBody(w, 404, errBody("not_found", "no agent "+id+" in this instance"))
		return
	}
	// Where: as asked, else where its link says it runs, else this host.
	if req.Placement == "" {
		req.Placement = "local"
		if i := c.Links.Info(id); i != nil && i.Placement != "" {
			req.Placement = i.Placement
		}
	}
	p, ok := c.inst.PlacementNamed(req.Placement)
	if !ok {
		writeJSONBody(w, 400, errBody("bad_request", "no placement "+req.Placement+" (stormo.yaml placements:)"))
		return
	}
	res := LifecycleResult{Action: action, Agent: id, Placement: p.Name, Log: []string{}}
	log := &lineLog{}
	var err error
	switch p.Kind {
	case instance.PlacementLocal:
		if action == "restart" && !running {
			writeJSONBody(w, 409, errBody("conflict", id+" is not running here; it picks up decisions when it starts"))
			return
		}
		err = c.lifecycle.Local(action, id, log.add)
	case instance.PlacementECS:
		var plan []string
		confirm := func(pl []string) bool { plan = pl; return req.Confirm }
		err = c.lifecycle.Remote(action, id, confirm, log.add)
		if err == nil && !req.Confirm && plan != nil {
			// Nothing ran: show the exact plan and wait for an explicit confirm.
			writeJSONBody(w, 428, map[string]any{"error": map[string]string{"code": "confirm", "message": "confirm this production change"}, "plan": plan})
			return
		}
	case instance.PlacementRunner:
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		var body json.RawMessage
		body, err = c.Runners.Call(ctx, p.Name, link.TypeOrder, link.Order{Action: action, Agent: id})
		if errors.Is(err, ErrNoLink) {
			writeJSONBody(w, 503, errBody("unavailable", "no runner serves "+p.Name+" now (stormo runner --placement "+p.Name+")"))
			return
		}
		var or link.OrderResult
		if json.Unmarshal(body, &or) == nil {
			for _, l := range or.Log {
				log.add(l)
			}
			res.State = or.State
		}
	}
	res.Log = append(res.Log, log.lines...)
	if err != nil {
		c.DB.Audit(who.Actor(), "agent."+action, id, p.Name+": failed: "+err.Error())
		writeJSONBody(w, 500, map[string]any{"error": map[string]string{"code": "failed", "message": clip(err.Error(), 500)}, "log": res.Log})
		return
	}
	c.DB.Audit(who.Actor(), "agent."+action, id, p.Name)
	if action == "restart" || action == "start" {
		c.Learner.markApplied(id)
	}
	writeJSONBody(w, 200, res)
}

// PlacementView is a placement as GET /api/placements shows it.
type PlacementView struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Available: local on a core that runs agents itself, remote with an AWS target, a runner
	// while it is connected.
	Available bool `json:"available"`
}

func (c *Core) placements() []PlacementView {
	ps := c.inst.Placements
	if ps == nil {
		ps = []instance.Placement{{Name: "local", Kind: instance.PlacementLocal}, {Name: "remote", Kind: instance.PlacementECS}}
	}
	out := []PlacementView{}
	for _, p := range ps {
		v := PlacementView{Name: p.Name, Kind: p.Kind}
		switch p.Kind {
		case instance.PlacementLocal:
			v.Available = true
		case instance.PlacementECS:
			v.Available = c.inst.Aws.Cluster != "" && c.inst.Aws.Cluster != instance.Unset
		case instance.PlacementRunner:
			v.Available = c.Runners.Connected(p.Name)
		}
		out = append(out, v)
	}
	return out
}

// handleRunner accepts a runner's link: a person's operate token, for a runner placement.
func (c *Core) handleRunner(w http.ResponseWriter, r *http.Request, who Caller) {
	if !who.Can(ScopeOperate) {
		writeJSONBody(w, 401, errBody("unauthorized", "a runner dials with an operate token"))
		return
	}
	c.Runners.Accept(w, r, who.Actor(), func(h link.Hello) (string, bool) {
		p, ok := c.inst.PlacementNamed(h.Placement)
		return p.Name, ok && p.Kind == instance.PlacementRunner
	})
}
