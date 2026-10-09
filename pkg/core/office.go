package core

import (
	"math/rand/v2"
	"sync"
)

// The office simulation behind the UI (docs/core.md §3). The core decides what each agent's
// character is doing (at the desk, getting coffee, on the sofa, walking in or out) and every viewer
// plays the same phases back by time, so two people watching the office see the same thing.
// Whether an agent is working comes from its engine and the gateway; the rest is flavour, decided
// here once with shared timings. State is in memory only: a restarted core seats everyone again.

// OfficeActivity: away | arrive | desk | relax | stretch | window | coffee | water | sofa | leave.
type OfficeActivity = string

// OfficePhase: seated | going | there | returning.
type OfficePhase = string

// OfficeState is what viewers play back for one agent. Times are epoch ms.
type OfficeState struct {
	Activity OfficeActivity `json:"activity"`
	Phase    OfficePhase    `json:"phase"`
	// The phase started, and ends (walks and dwells; null while seated or away).
	Start int64  `json:"start"`
	End   *int64 `json:"end"`
	// For `leave`: where the walk out starts (desk or the spot the agent was at).
	From OfficeActivity `json:"from,omitempty"`
	// Agents doing the same thing at once stand side by side: 0, 1, …
	Slot int `json:"slot"`
	// What the name tag says after the status, e.g. "getting coffee".
	Label string `json:"label"`
	// Working: an engine turn or a model call in flight, held a few seconds across tool gaps.
	Working bool `json:"working"`
	// Bumped on every change, so a viewer knows when to start a new walk.
	Seq int `json:"seq"`
}

// OfficeAgent is what a tick needs to know about an agent.
type OfficeAgent struct {
	ID       string
	State    string
	Activity *Activity
	// A tool call is in flight (the agent's hooks).
	Busy bool
}

type span struct{ lo, hi int64 }

// OfficeTimings are shared by every viewer: the page fits its paths to them.
var OfficeTimings = struct {
	HoldMs                                                       int64
	WalkLobbyMs, WalkRoomMs, WalkInMs, HurryLobbyMs, HurryRoomMs int64
	DwellMs, RelaxMs                                             span
	// Idle time at the desk before the next thing to do.
	FirstIdleMs, IdleGapMs span
	// At most this many agents away from their offices at once.
	LobbyCap int
}{
	HoldMs:      8_000,
	WalkLobbyMs: 7_000, WalkRoomMs: 2_000, WalkInMs: 8_000, HurryLobbyMs: 4_000, HurryRoomMs: 1_200,
	DwellMs: span{10_000, 22_000}, RelaxMs: span{9_000, 18_000},
	FirstIdleMs: span{15_000, 30_000}, IdleGapMs: span{25_000, 55_000},
	LobbyCap: 2,
}

var (
	lobby  = map[string]bool{"coffee": true, "water": true, "sofa": true}
	trips  = map[string]bool{"coffee": true, "water": true, "sofa": true, "window": true, "stretch": true}
	labels = map[string]string{
		"away": "", "arrive": "on the way in", "desk": "", "relax": "leaning back", "stretch": "stretching",
		"window": "looking outside", "coffee": "getting coffee", "water": "at the water cooler", "sofa": "on the sofa", "leave": "on the way out",
	}
	choices = []struct {
		activity string
		weight   float64
	}{{"relax", 3}, {"coffee", 3}, {"water", 2}, {"sofa", 2}, {"window", 1}, {"stretch", 1}}
	present = map[string]bool{"running": true, "starting": true, "restarting": true, "unknown": true}
)

// RobotNote is what the robot wrote down at a desk: one nap (or several merged on its way).
type RobotNote struct {
	Agent   string     `json:"agent"`
	TakenAt string     `json:"takenAt"`
	Reason  string     `json:"reason"`
	Changed NapChanges `json:"changed"`
	Removed int        `json:"removed"`
	Naps    int        `json:"naps"`
	// When the robot wrote it (epoch ms).
	At int64 `json:"at"`
}

// RobotState is the core's sync robot. Agents' nap sidecars save their homes on their own; when a
// new nap lands, the robot rolls from its dock to that agent's desk, writes down what changed, then
// goes on to the next desk or back to the dock. It shows the sync; it does not cause it.
type RobotState struct {
	Phase string `json:"phase"` // docked | going | there | returning
	// "dock" or an agent id.
	From   string  `json:"from"`
	Target *string `json:"target"`
	Start  int64   `json:"start"`
	End    *int64  `json:"end"`
	Seq    int     `json:"seq"`
	// The note being written (while there) or last written.
	Note *RobotNote `json:"note"`
	// The last notes, newest first.
	Log []RobotNote `json:"log"`
	// Agents with a visit queued.
	Queue []string `json:"queue"`
}

// RobotTimings are the robot's moves.
var RobotTimings = struct {
	TransitMs, HopMs, WriteMs int64
	LogSize                   int
}{7_000, 6_000, 5_000, 8}

type officeAgent struct {
	OfficeState
	workAt     float64 // -Inf until the first sign of work
	nextIdleAt int64
}

// Office simulates the floor; safe for concurrent use.
type Office struct {
	mu     sync.Mutex
	agents map[string]*officeAgent
	bot    RobotState
	visits []RobotNote
	random func() float64
}

// NewOffice builds an office; random nil uses math/rand.
func NewOffice(random func() float64) *Office {
	if random == nil {
		random = rand.Float64
	}
	return &Office{agents: map[string]*officeAgent{}, bot: RobotState{Phase: "docked", From: "dock", Log: []RobotNote{}, Queue: []string{}}, random: random}
}

func (o *Office) queueIDs() []string {
	q := make([]string, 0, len(o.visits))
	for _, v := range o.visits {
		q = append(q, v.Agent)
	}
	return q
}

// NoteNap queues a visit, or folds the nap into the visit already queued for that agent.
func (o *Office) NoteNap(e NapEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range o.visits {
		v := &o.visits[i]
		if v.Agent != e.Agent {
			continue
		}
		v.Changed.Learning += e.Changed.Learning
		v.Changed.State += e.Changed.State
		v.Changed.Raw += e.Changed.Raw
		v.Removed += e.Removed
		v.Naps++
		v.TakenAt, v.Reason = e.TakenAt, e.Reason
		o.bot.Queue = o.queueIDs()
		return
	}
	o.visits = append(o.visits, RobotNote{Agent: e.Agent, TakenAt: e.TakenAt, Reason: e.Reason, Changed: e.Changed, Removed: e.Removed, Naps: 1})
	o.bot.Queue = o.queueIDs()
}

// Robot is a copy of the robot's state.
func (o *Office) Robot() RobotState {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.bot
	r.Log = append([]RobotNote{}, o.bot.Log...)
	r.Queue = append([]string{}, o.bot.Queue...)
	if o.bot.Note != nil {
		n := *o.bot.Note
		r.Note = &n
	}
	return r
}

func ptr[T any](v T) *T { return &v }

func (o *Office) moveBot(phase, from string, target *string, now int64, ms *int64, note *RobotNote) {
	o.bot.Phase, o.bot.From, o.bot.Target, o.bot.Start = phase, from, target, now
	o.bot.End = nil
	if ms != nil {
		o.bot.End = ptr(now + *ms)
	}
	o.bot.Seq++
	o.bot.Note = note
}

func (o *Office) nextVisit() *RobotNote {
	// Visits to agents that left the fleet are dropped.
	for len(o.visits) > 0 {
		if _, ok := o.agents[o.visits[0].Agent]; ok {
			break
		}
		o.visits = o.visits[1:]
	}
	if len(o.visits) == 0 {
		o.bot.Queue = []string{}
		return nil
	}
	v := o.visits[0]
	o.visits = o.visits[1:]
	o.bot.Queue = o.queueIDs()
	return &v
}

func (o *Office) stepRobot(now int64) {
	b := &o.bot
	if b.Phase == "docked" {
		if v := o.nextVisit(); v != nil {
			v.At = now
			o.moveBot("going", "dock", ptr(v.Agent), now, ptr(RobotTimings.TransitMs), v)
		}
		return
	}
	if b.End == nil || now < *b.End {
		return
	}
	target := "dock"
	if b.Target != nil {
		target = *b.Target
	}
	switch b.Phase {
	case "going":
		note := *b.Note
		note.At = now
		b.Log = append([]RobotNote{note}, b.Log...)
		if len(b.Log) > RobotTimings.LogSize {
			b.Log = b.Log[:RobotTimings.LogSize]
		}
		o.moveBot("there", b.From, b.Target, now, ptr(RobotTimings.WriteMs), &note)
	case "there":
		if v := o.nextVisit(); v != nil {
			v.At = now
			o.moveBot("going", target, ptr(v.Agent), now, ptr(RobotTimings.HopMs), v)
		} else {
			o.moveBot("returning", target, ptr("dock"), now, ptr(RobotTimings.TransitMs), b.Note)
		}
	case "returning":
		o.moveBot("docked", "dock", nil, now, nil, b.Note)
	}
}

func (o *Office) between(s span) float64 { return float64(s.lo) + o.random()*float64(s.hi-s.lo) }

// State is what viewers play back, or nil for an agent the office has not seen.
func (o *Office) State(id string) *OfficeState {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.agents[id]
	if !ok {
		return nil
	}
	out := s.OfficeState
	return &out
}

func (o *Office) set(s *officeAgent, activity, phase string, now int64, duration *float64, slot *int, from string) {
	s.Activity, s.Phase, s.Start = activity, phase, now
	s.End = nil
	if duration != nil {
		s.End = ptr(now + int64(*duration))
	}
	s.Label = labels[activity]
	s.Seq++
	s.From = from
	if slot != nil {
		s.Slot = *slot
	}
	if activity == "desk" || activity == "away" {
		s.Slot = 0
	}
}

func dur(ms int64) *float64 { return ptr(float64(ms)) }

func (o *Office) seat(s *officeAgent, now int64, gap span) {
	o.set(s, "desk", "seated", now, nil, nil, "")
	s.nextIdleAt = now + int64(o.between(gap))
}

// outings counts agents out of their office right now (lobby trips; walks in or out excluded).
func (o *Office) outings(except string) int {
	n := 0
	for id, s := range o.agents {
		if id != except && lobby[s.Activity] {
			n++
		}
	}
	return n
}

func (o *Office) slotFor(id, activity string) int {
	taken := map[int]bool{}
	for other, s := range o.agents {
		if other != id && s.Activity == activity {
			taken[s.Slot] = true
		}
	}
	slot := 0
	for taken[slot] {
		slot++
	}
	return slot
}

func (o *Office) pick(id string) string {
	type choice struct {
		activity string
		weight   float64
	}
	options := []choice{}
	total := 0.0
	for _, c := range choices {
		if !lobby[c.activity] || o.outings(id) < OfficeTimings.LobbyCap {
			options = append(options, choice{c.activity, c.weight})
			total += c.weight
		}
	}
	r := o.random() * total
	for _, c := range options {
		if r -= c.weight; r < 0 {
			return c.activity
		}
	}
	return options[0].activity
}

// Tick advances every agent to now (epoch ms) from the latest fleet snapshot and the gateway's
// calls in flight per agent.
func (o *Office) Tick(agents []OfficeAgent, gatewayActive map[string]int, now int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range agents {
		seen[a.ID] = true
		isPresent := present[a.State]
		// A turn, a model call through the core, or a scheduled script (no agent turn at all).
		busy := gatewayActive[a.ID] > 0
		if a.Busy || a.Activity != nil && (a.Activity.ActiveAgents > 0 || a.Activity.GatewayBusy || len(a.Activity.RunningJobs) > 0) {
			busy = true
		}
		s, ok := o.agents[a.ID]
		if !ok {
			// First sight (also after a core restart): already at the desk or already gone, no walk.
			s = &officeAgent{OfficeState: OfficeState{Activity: "away", Phase: "seated", Start: now}, workAt: negInf}
			if busy {
				s.workAt = float64(now)
			}
			o.agents[a.ID] = s
			if isPresent {
				o.seat(s, now, OfficeTimings.FirstIdleMs)
			}
		}
		if busy {
			s.workAt = float64(now)
		}
		working := isPresent && a.State == "running" && float64(now)-s.workAt < float64(OfficeTimings.HoldMs)
		if working != s.Working {
			s.Working = working
			s.Seq++
		}
		o.step(a, s, isPresent, now)
	}
	for id := range o.agents {
		if !seen[id] {
			delete(o.agents, id)
		}
	}
	o.stepRobot(now)
}

var negInf = -1e308

func (o *Office) step(a OfficeAgent, s *officeAgent, isPresent bool, now int64) {
	due := s.End != nil && now >= *s.End
	slot := s.Slot

	if !isPresent {
		if s.Activity == "away" {
			return
		}
		if s.Activity == "leave" {
			if due {
				o.set(s, "away", "seated", now, nil, nil, "")
			}
			return
		}
		// Walk out from wherever the agent is; from the lobby the entrance is closer.
		from := s.Activity
		if from == "arrive" {
			from = "desk"
		}
		d := float64(OfficeTimings.WalkInMs)
		if lobby[from] {
			d = float64(OfficeTimings.WalkLobbyMs) * 0.6
		}
		o.set(s, "leave", "going", now, &d, nil, from)
		return
	}

	if s.Activity == "away" || s.Activity == "leave" {
		o.set(s, "arrive", "going", now, dur(OfficeTimings.WalkInMs), nil, "")
		return
	}
	if s.Activity == "arrive" {
		if due {
			o.seat(s, now, OfficeTimings.FirstIdleMs)
		}
		return
	}

	// Work comes first: anyone away from the desk hurries back.
	if s.Working && s.Activity != "desk" {
		if s.Activity == "relax" {
			o.seat(s, now, OfficeTimings.IdleGapMs)
			return
		}
		if s.Phase != "returning" {
			hurry := OfficeTimings.HurryRoomMs
			if lobby[s.Activity] {
				hurry = OfficeTimings.HurryLobbyMs
			}
			o.set(s, s.Activity, "returning", now, dur(hurry), &slot, "")
			return
		}
	}

	if s.Activity == "desk" {
		if !s.Working && a.State == "running" && now >= s.nextIdleAt {
			activity := o.pick(a.ID)
			sl := o.slotFor(a.ID, activity)
			if activity == "relax" {
				d := o.between(OfficeTimings.RelaxMs)
				o.set(s, "relax", "there", now, &d, nil, "")
			} else {
				walk := OfficeTimings.WalkRoomMs
				if lobby[activity] {
					walk = OfficeTimings.WalkLobbyMs
				}
				o.set(s, activity, "going", now, dur(walk), &sl, "")
			}
		}
		return
	}

	if !due {
		return
	}
	if s.Activity == "relax" {
		o.seat(s, now, OfficeTimings.IdleGapMs)
		return
	}
	if trips[s.Activity] {
		switch s.Phase {
		case "going":
			d := o.between(OfficeTimings.DwellMs)
			o.set(s, s.Activity, "there", now, &d, &slot, "")
		case "there":
			walk := OfficeTimings.WalkRoomMs
			if lobby[s.Activity] {
				walk = OfficeTimings.WalkLobbyMs
			}
			o.set(s, s.Activity, "returning", now, dur(walk), &slot, "")
		default:
			o.seat(s, now, OfficeTimings.IdleGapMs)
		}
	}
}
