package manifest

import (
	"time"

	"github.com/camfinc/stormo/pkg/clock"
)

// Returns is how the agent's requests sent from a person's conversation come back to that person
// (agent.yaml bus: {returns: …}; docs/core.md §5, "Returns"): when the core brings the agent back to
// tell them where it stands, and in which hours. Engine-neutral. Nil: off.
type Returns struct {
	// UpdateAfter is how long a person waits with no update before the agent is brought back.
	UpdateAfter time.Duration `json:"update_after"`
	// EscalateAfter is when the owner is told it is overdue (and the prompts stop).
	EscalateAfter time.Duration `json:"escalate_after"`
	// Hours is a working-hours schedule (pkg/clock), "" every time; Timezone its IANA zone, "" UTC.
	Hours    string `json:"hours,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

// Defaults and bounds of bus.returns.
const (
	DefaultUpdateAfter   = 45 * time.Minute
	DefaultEscalateAfter = 2 * time.Hour
	MinUpdateAfter       = 5 * time.Minute
	MaxUpdateAfter       = 24 * time.Hour
	MaxEscalateAfter     = 7 * 24 * time.Hour
	// MinWeeklyHours: a schedule must hold at least this much working time a week, so every
	// deadline is reached.
	MinWeeklyHours = 1
)

// Schedule is the returns' working hours and their zone (a nil schedule: every time).
func (r *Returns) Schedule() (*clock.Schedule, *time.Location) {
	loc := time.UTC
	if r.Timezone != "" {
		if l, err := time.LoadLocation(r.Timezone); err == nil {
			loc = l
		}
	}
	if r.Hours == "" {
		return nil, loc
	}
	s, err := clock.ParseSchedule(r.Hours)
	if err != nil {
		return nil, loc
	}
	return s, loc
}

// returnsOf reads bus:, nil when the manifest has none or returns are off.
func returnsOf(m raw, where string) (*Returns, error) {
	if m.Bus == nil {
		return nil, nil
	}
	for k := range m.Bus {
		if err := req(k == "returns", "%s: bus: unknown key %q (returns)", where, k); err != nil {
			return nil, err
		}
	}
	x, ok := m.Bus["returns"]
	if !ok || x == nil {
		return nil, nil
	}
	j, isMap := x.(map[string]any)
	if err := req(isMap, "%s: bus.returns is a map (on, update_after, escalate_after, hours, timezone)", where); err != nil {
		return nil, err
	}
	known := map[string]bool{"on": true, "update_after": true, "escalate_after": true, "hours": true, "timezone": true}
	for k := range j {
		if err := req(known[k], "%s: bus.returns: unknown key %q", where, k); err != nil {
			return nil, err
		}
	}
	okBools := true
	on := optBool(j["on"], &okBools)
	if err := req(okBools, "%s: bus.returns.on is true or false", where); err != nil {
		return nil, err
	}
	if on == nil || !*on {
		return nil, nil
	}
	r := &Returns{UpdateAfter: DefaultUpdateAfter, EscalateAfter: DefaultEscalateAfter, Hours: str(j["hours"]), Timezone: str(j["timezone"])}
	for key, dst := range map[string]*time.Duration{"update_after": &r.UpdateAfter, "escalate_after": &r.EscalateAfter} {
		v, present := j[key]
		if !present || v == nil {
			continue
		}
		d, err := time.ParseDuration(str(v))
		if err := req(err == nil && d > 0, "%s: bus.returns.%s is a duration (45m, 2h)", where, key); err != nil {
			return nil, err
		}
		*dst = d
	}
	checks := []error{
		req(r.UpdateAfter >= MinUpdateAfter && r.UpdateAfter <= MaxUpdateAfter, "%s: bus.returns.update_after must be %s to %s", where, MinUpdateAfter, MaxUpdateAfter),
		req(r.EscalateAfter > r.UpdateAfter && r.EscalateAfter <= MaxEscalateAfter, "%s: bus.returns.escalate_after must be more than update_after and at most %s", where, MaxEscalateAfter),
	}
	for _, e := range checks {
		if e != nil {
			return nil, e
		}
	}
	if r.Timezone != "" {
		_, err := time.LoadLocation(r.Timezone)
		if err := req(err == nil, "%s: bus.returns.timezone %q is not an IANA time zone (America/Panama)", where, r.Timezone); err != nil {
			return nil, err
		}
	}
	if r.Hours != "" {
		s, err := clock.ParseSchedule(r.Hours)
		if err != nil {
			return nil, req(false, "%s: bus.returns.hours: %v", where, err)
		}
		if err := req(s.WeeklyMinutes() >= MinWeeklyHours*60, "%s: bus.returns.hours holds less than %d hour a week", where, MinWeeklyHours); err != nil {
			return nil, err
		}
	}
	return r, nil
}
