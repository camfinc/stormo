package core

import (
	"slices"
	"sort"
	"testing"
)

var (
	idleAct = &Activity{RunningJobs: []string{}, Platforms: map[string]Platform{}}
	busyAct = &Activity{ActiveAgents: 1, RunningJobs: []string{}, Platforms: map[string]Platform{}}
)

func oa(id, state string, act *Activity) OfficeAgent {
	return OfficeAgent{ID: id, State: state, Activity: act}
}
func run(id string) OfficeAgent { return oa(id, "running", idleAct) }

// script returns the given values in turn (then 0).
func script(values ...float64) func() float64 {
	return func() float64 {
		if len(values) == 0 {
			return 0
		}
		v := values[0]
		values = values[1:]
		return v
	}
}

// With random 0: the shortest delays, and the first choice (relax) unless a weight pushes past it.
// Weights: relax 3, coffee 3, water 2, sofa 2, window 1, stretch 1 (total 12).
const coffee = 4.0 / 12

func end(s *OfficeState) int64 {
	if s.End == nil {
		return -1
	}
	return *s.End
}

func TestOfficeFirstSight(t *testing.T) {
	o := NewOffice(script())
	o.Tick([]OfficeAgent{run("atlas"), oa("scout", "stopped", nil)}, nil, 1_000)
	if s := o.State("atlas"); s.Activity != "desk" || s.Phase != "seated" || s.Working {
		t.Errorf("atlas = %+v", s)
	}
	if s := o.State("scout"); s.Activity != "away" {
		t.Errorf("scout = %+v", s)
	}
	if o.State("nobody") != nil {
		t.Error("unknown agent has a state")
	}
}

func TestOfficeIdleTrip(t *testing.T) {
	T := OfficeTimings
	o := NewOffice(script(0, coffee, 0, 0))
	m := []OfficeAgent{run("atlas")}
	var now int64
	o.Tick(m, nil, now)
	now += T.FirstIdleMs.lo - 1
	o.Tick(m, nil, now)
	if o.State("atlas").Activity != "desk" {
		t.Fatal("left the desk early")
	}
	now++
	o.Tick(m, nil, now)
	if s := o.State("atlas"); s.Activity != "coffee" || s.Phase != "going" || s.Start != now || end(s) != now+T.WalkLobbyMs || s.Label != "getting coffee" || s.Slot != 0 {
		t.Errorf("going = %+v", s)
	}
	now += T.WalkLobbyMs
	o.Tick(m, nil, now)
	if s := o.State("atlas"); s.Phase != "there" || end(s) != now+T.DwellMs.lo {
		t.Errorf("there = %+v", s)
	}
	now += T.DwellMs.lo
	o.Tick(m, nil, now)
	if s := o.State("atlas"); s.Phase != "returning" || end(s) != now+T.WalkLobbyMs {
		t.Errorf("returning = %+v", s)
	}
	now += T.WalkLobbyMs
	o.Tick(m, nil, now)
	if s := o.State("atlas"); s.Activity != "desk" || s.Phase != "seated" || s.Label != "" {
		t.Errorf("back = %+v", s)
	}
}

func TestOfficeWorkMidTrip(t *testing.T) {
	T := OfficeTimings
	o := NewOffice(script(0, coffee))
	var now int64
	o.Tick([]OfficeAgent{run("atlas")}, nil, now)
	now += T.FirstIdleMs.lo
	o.Tick([]OfficeAgent{run("atlas")}, nil, now)
	now += 3_000
	o.Tick([]OfficeAgent{run("atlas")}, nil, now)
	before := o.State("atlas")
	if before.Activity != "coffee" || before.Phase != "going" {
		t.Fatalf("before = %+v", before)
	}
	now += 500
	o.Tick([]OfficeAgent{oa("atlas", "running", busyAct)}, nil, now)
	after := o.State("atlas")
	if after.Phase != "returning" || !after.Working || end(after) != now+T.HurryLobbyMs || after.Seq <= before.Seq {
		t.Errorf("after = %+v", after)
	}
	now += T.HurryLobbyMs
	o.Tick([]OfficeAgent{oa("atlas", "running", busyAct)}, nil, now)
	if s := o.State("atlas"); s.Activity != "desk" || s.Phase != "seated" || !s.Working {
		t.Errorf("seated = %+v", s)
	}
}

func TestOfficeWorkingHeldAndGatewayCounts(t *testing.T) {
	o := NewOffice(script())
	o.Tick([]OfficeAgent{run("atlas")}, map[string]int{"atlas": 1}, 0)
	if !o.State("atlas").Working {
		t.Error("gateway call not working")
	}
	o.Tick([]OfficeAgent{run("atlas")}, nil, OfficeTimings.HoldMs-1)
	if !o.State("atlas").Working {
		t.Error("not held")
	}
	o.Tick([]OfficeAgent{run("atlas")}, nil, OfficeTimings.HoldMs)
	if o.State("atlas").Working {
		t.Error("held too long")
	}
}

func TestOfficeScheduledScriptCounts(t *testing.T) {
	o := NewOffice(script())
	o.Tick([]OfficeAgent{oa("scout", "running", &Activity{RunningJobs: []string{"SLA watchdog"}})}, nil, 0)
	if !o.State("scout").Working {
		t.Error("running job not working")
	}
}

func TestOfficeNeverWandersWhileWorking(t *testing.T) {
	o := NewOffice(script())
	for now := int64(0); now < 120_000; now += 1_000 {
		o.Tick([]OfficeAgent{oa("atlas", "running", busyAct)}, nil, now)
	}
	if s := o.State("atlas"); s.Activity != "desk" || !s.Working {
		t.Errorf("s = %+v", s)
	}
}

func TestOfficeStopAndStart(t *testing.T) {
	T := OfficeTimings
	o := NewOffice(script(0, coffee))
	var now int64
	o.Tick([]OfficeAgent{run("atlas")}, nil, now)
	now += T.FirstIdleMs.lo
	o.Tick([]OfficeAgent{run("atlas")}, nil, now)
	now += 1_000
	o.Tick([]OfficeAgent{oa("atlas", "exited", nil)}, nil, now)
	if s := o.State("atlas"); s.Activity != "leave" || s.Phase != "going" || s.From != "coffee" || s.Label != "on the way out" {
		t.Errorf("leave = %+v", s)
	}
	now += T.WalkInMs
	o.Tick([]OfficeAgent{oa("atlas", "exited", nil)}, nil, now)
	if o.State("atlas").Activity != "away" {
		t.Error("not away")
	}
	now += 1_000
	o.Tick([]OfficeAgent{oa("atlas", "starting", nil)}, nil, now)
	if s := o.State("atlas"); s.Activity != "arrive" || s.Phase != "going" || end(s) != now+T.WalkInMs {
		t.Errorf("arrive = %+v", s)
	}
	now += T.WalkInMs
	o.Tick([]OfficeAgent{run("atlas")}, nil, now)
	if s := o.State("atlas"); s.Activity != "desk" || s.Phase != "seated" {
		t.Errorf("seated = %+v", s)
	}
}

func TestOfficeLobbyCap(t *testing.T) {
	names := []string{"a", "b", "c"}
	o := NewOffice(func() float64 { return coffee })
	agents := []OfficeAgent{run("a"), run("b"), run("c")}
	o.Tick(agents, nil, 0)
	o.Tick(agents, nil, OfficeTimings.FirstIdleMs.hi)
	inLobby, coffeeSlots := 0, []int{}
	for _, n := range names {
		s := o.State(n)
		if lobby[s.Activity] {
			inLobby++
		}
		if s.Activity == "coffee" {
			coffeeSlots = append(coffeeSlots, s.Slot)
		}
	}
	sort.Ints(coffeeSlots)
	if inLobby != OfficeTimings.LobbyCap || !slices.Equal(coffeeSlots, []int{0, 1}) {
		t.Errorf("lobby %d slots %v", inLobby, coffeeSlots)
	}
	if a := o.State("c").Activity; !slices.Contains([]string{"relax", "window", "stretch"}, a) {
		t.Errorf("third agent: %s", a)
	}
}

func TestSyncRobot(t *testing.T) {
	R := RobotTimings
	o := NewOffice(script())
	nap := func(agent string, learning int) NapEvent {
		return NapEvent{Agent: agent, TakenAt: agent + "-nap", Reason: "interval", Changed: NapChanges{Learning: learning, State: 1}}
	}
	fleet := []OfficeAgent{run("atlas"), run("scout")}
	var now int64
	o.Tick(fleet, nil, now)
	if r := o.Robot(); r.Phase != "docked" || r.Target != nil || len(r.Log) != 0 {
		t.Fatalf("r = %+v", r)
	}
	o.NoteNap(nap("atlas", 2))
	o.NoteNap(nap("scout", 1))
	o.NoteNap(nap("atlas", 3)) // merged into atlas's queued visit
	if q := o.Robot().Queue; !slices.Equal(q, []string{"atlas", "scout"}) {
		t.Errorf("queue = %v", q)
	}
	now++
	o.Tick(fleet, nil, now)
	if r := o.Robot(); r.Phase != "going" || r.From != "dock" || *r.Target != "atlas" || *r.End != now+R.TransitMs || !slices.Equal(r.Queue, []string{"scout"}) {
		t.Errorf("going = %+v", r)
	}
	now += R.TransitMs
	o.Tick(fleet, nil, now)
	there := o.Robot()
	if there.Phase != "there" || *there.Target != "atlas" || there.Note.Agent != "atlas" || there.Note.Changed != (NapChanges{5, 2, 0}) || there.Note.Naps != 2 || there.Log[0].Agent != "atlas" {
		t.Errorf("there = %+v", there)
	}
	now += R.WriteMs
	o.Tick(fleet, nil, now)
	if r := o.Robot(); r.Phase != "going" || r.From != "atlas" || *r.Target != "scout" || *r.End != now+R.HopMs {
		t.Errorf("hop = %+v", r)
	}
	now += R.HopMs
	o.Tick(fleet, nil, now)
	now += R.WriteMs
	o.Tick(fleet, nil, now)
	if r := o.Robot(); r.Phase != "returning" || r.From != "scout" || *r.Target != "dock" {
		t.Errorf("returning = %+v", r)
	}
	now += R.TransitMs
	o.Tick(fleet, nil, now)
	done := o.Robot()
	agents := []string{}
	for _, n := range done.Log {
		agents = append(agents, n.Agent)
	}
	if done.Phase != "docked" || done.Target != nil || !slices.Equal(agents, []string{"scout", "atlas"}) || done.Seq <= there.Seq {
		t.Errorf("done = %+v", done)
	}
}

func TestSyncRobotSkipsAgentsThatLeft(t *testing.T) {
	o := NewOffice(script())
	o.Tick([]OfficeAgent{run("atlas"), run("gone")}, nil, 0)
	o.NoteNap(NapEvent{Agent: "gone", TakenAt: "x", Reason: "interval", Changed: NapChanges{Learning: 1}})
	o.NoteNap(NapEvent{Agent: "atlas", TakenAt: "y", Reason: "shutdown", Changed: NapChanges{State: 1}})
	o.Tick([]OfficeAgent{run("atlas")}, nil, 1)
	if r := o.Robot(); r.Phase != "going" || *r.Target != "atlas" || r.Note.Reason != "shutdown" {
		t.Errorf("r = %+v", r)
	}
}

func TestOfficeForgetsAgentsThatLeave(t *testing.T) {
	o := NewOffice(script())
	o.Tick([]OfficeAgent{run("atlas")}, nil, 0)
	o.Tick(nil, nil, 1_000)
	if o.State("atlas") != nil {
		t.Error("still known")
	}
}
