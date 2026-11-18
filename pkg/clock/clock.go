// Package clock reads wall-clock times in a time zone and counts working time on a weekly schedule.
package clock

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Wall is a wall-clock time in loc (the date normalised, as time.Date does: the 32nd is the next
// month's 1st). The instants that read as it are those its zone's offsets around it give (an
// offset 6 hours before, at and 6 hours after time.Date's answer: one change at most in that
// span). None: a daylight-saving change skips it, and it moves forward by the change, whichever
// side of UTC the zone is (time.Date leaves the choice unspecified, and moves it backward west of
// UTC). Two: the change repeats it, and it is the first, unless the first is not after now and the
// second is (now zero: the first).
func Wall(y int, mo time.Month, d, h, m, s int, loc *time.Location, now time.Time) time.Time {
	// the civil date first, hours past 23 included ("24:00" is the next day's midnight), never
	// through loc's clock, which a daylight-saving change could move to another day
	if h >= 24 {
		d, h = d+h/24, h%24
	}
	y, mo, d = time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Date()
	t := time.Date(y, mo, d, h, m, s, 0, loc)
	u := time.Date(y, mo, d, h, m, s, 0, time.UTC) // the wall time as if it were UTC
	var found []time.Time
	for _, probe := range []time.Time{t.Add(-6 * time.Hour), t, t.Add(6 * time.Hour)} {
		_, off := probe.Zone()
		c := u.Add(-time.Duration(off) * time.Second).In(loc)
		cy, cmo, cd := c.Date()
		if cy == y && cmo == mo && cd == d && c.Hour() == h && c.Minute() == m && c.Second() == s && !slices.ContainsFunc(found, c.Equal) {
			found = append(found, c)
		}
	}
	slices.SortFunc(found, func(a, b time.Time) int { return a.Compare(b) })
	switch {
	case len(found) == 0: // skipped: the offset from before the change, which reads it later by the change
		_, before := t.Add(-6 * time.Hour).Zone()
		return u.Add(-time.Duration(before) * time.Second).In(loc)
	case len(found) > 1 && !now.IsZero() && !found[0].After(now) && found[1].After(now):
		return found[1]
	}
	return found[0]
}

// Schedule is a week of working hours: entries "HH:MM-HH:MM <days>", days a weekday ("mon"), a
// range ("mon-sat", wrapping: "fri-mon") or "daily", comma-separated ("08:00-19:00 mon-sat,
// 09:00-13:00 sun"). An entry whose end is not after its start runs past midnight into the next day
// ("22:00-06:00 daily"). Its wall times are read in the zone it is used with, by Wall: a start or
// an end a daylight-saving change skips moves forward, one it repeats is the first. Working time
// is the elapsed time inside the entries (a repeated hour inside them counts as it passes).
type Schedule struct {
	raw     string
	entries []entry
}

type entry struct {
	days       [7]bool // by time.Weekday
	start, end int     // minutes after midnight
}

// ParseSchedule reads a schedule. An empty one is refused: no schedule at all is the caller's
// "every time is working time".
func ParseSchedule(s string) (*Schedule, error) {
	out := &Schedule{raw: strings.TrimSpace(s)}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		f := strings.Fields(part)
		if len(f) != 2 {
			return nil, fmt.Errorf(`schedule entry %q: want "HH:MM-HH:MM <days>", e.g. "08:00-19:00 mon-sat"`, part)
		}
		span := strings.SplitN(f[0], "-", 2)
		if len(span) != 2 {
			return nil, fmt.Errorf("schedule entry %q: want a span HH:MM-HH:MM", part)
		}
		st, err1 := minutes(span[0])
		en, err2 := minutes(span[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("schedule entry %q: times are HH:MM (00:00 to 24:00)", part)
		}
		if st == en {
			return nil, fmt.Errorf("schedule entry %q: an empty span", part)
		}
		e := entry{start: st, end: en}
		if err := e.setDays(f[1]); err != nil {
			return nil, fmt.Errorf("schedule entry %q: %w", part, err)
		}
		out.entries = append(out.entries, e)
	}
	if len(out.entries) == 0 {
		return nil, fmt.Errorf("an empty schedule: leave it out for every time to count")
	}
	return out, nil
}

var dayNames = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

func (e *entry) setDays(s string) error {
	if s == "daily" {
		for i := range e.days {
			e.days[i] = true
		}
		return nil
	}
	a, b, isRange := strings.Cut(s, "-")
	from, ok1 := dayNames[a]
	if !ok1 {
		return fmt.Errorf("days %q: a weekday (mon…sun), a range (mon-sat) or daily", s)
	}
	to := from
	if isRange {
		var ok2 bool
		if to, ok2 = dayNames[b]; !ok2 {
			return fmt.Errorf("days %q: a weekday (mon…sun), a range (mon-sat) or daily", s)
		}
	}
	for d := from; ; d = (d + 1) % 7 {
		e.days[d] = true
		if d == to {
			break
		}
	}
	return nil
}

func minutes(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || len(s) != 5 || h < 0 || m < 0 || m > 59 || h > 24 || h == 24 && m != 0 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return h*60 + m, nil
}

// interval is one entry on one day, as instants.
type interval struct{ from, to time.Time }

// date is a civil date; its arithmetic is UTC's, never a zone's clock.
type date struct {
	y int
	m time.Month
	d int
}

func dateOf(t time.Time, loc *time.Location) date {
	y, m, d := t.In(loc).Date()
	return date{y, m, d}
}

func (d date) add(n int) date {
	y, m, dd := time.Date(d.y, d.m, d.d+n, 12, 0, 0, 0, time.UTC).Date()
	return date{y, m, dd}
}

func (d date) weekday() time.Weekday {
	return time.Date(d.y, d.m, d.d, 12, 0, 0, 0, time.UTC).Weekday()
}

// intervals are the schedule's working intervals that touch the civil day day (an entry running
// past midnight from the day before included), merged and in order.
func (s *Schedule) intervals(day date, loc *time.Location) []interval {
	var out []interval
	for back := -1; back <= 0; back++ {
		d := day.add(back)
		for _, e := range s.entries {
			if !e.days[d.weekday()] {
				continue
			}
			from := Wall(d.y, d.m, d.d, e.start/60, e.start%60, 0, loc, time.Time{})
			endDay := d.d
			if e.end <= e.start {
				endDay++ // past midnight
			}
			to := Wall(d.y, d.m, endDay, e.end/60, e.end%60, 0, loc, time.Time{})
			if to.After(from) {
				out = append(out, interval{from, to})
			}
		}
	}
	slices.SortFunc(out, func(a, b interval) int { return a.from.Compare(b.from) })
	merged := out[:0]
	for _, iv := range out {
		if n := len(merged); n > 0 && !iv.from.After(merged[n-1].to) {
			if iv.to.After(merged[n-1].to) {
				merged[n-1].to = iv.to
			}
			continue
		}
		merged = append(merged, iv)
	}
	return merged
}

// maxWalk bounds a walk in days (about 55 years): beyond it AddWorking answers the zero time,
// which callers read as never (a schedule of minutes a week asked for days of working time).
const maxWalk = 20000

// AddWorking is t plus d of working time on the schedule in loc. A nil schedule counts every time
// (t.Add(d)). d ≤ 0 is the first working instant at or after t.
func (s *Schedule) AddWorking(t time.Time, d time.Duration, loc *time.Location) time.Time {
	if s == nil {
		return t.Add(max(d, 0))
	}
	if loc == nil {
		loc = time.UTC
	}
	left := max(d, 0)
	cur := t
	day := dateOf(t, loc)
	for i := 0; ; i++ {
		for _, iv := range s.intervals(day, loc) {
			if !iv.to.After(cur) {
				continue
			}
			from := iv.from
			if cur.After(from) {
				from = cur
			}
			if avail := iv.to.Sub(from); avail >= left {
				return from.Add(left)
			} else {
				left -= avail
				cur = iv.to
			}
		}
		// the next civil day, from its midnight (a day may be 23 or 25 hours long)
		day = day.add(1)
		if next := Wall(day.y, day.m, day.d, 0, 0, 0, loc, time.Time{}); next.After(cur) {
			cur = next
		}
		if i > maxWalk {
			return time.Time{}
		}
	}
}

// Working is whether t is inside the schedule in loc (a nil schedule: always).
func (s *Schedule) Working(t time.Time, loc *time.Location) bool {
	if s == nil {
		return true
	}
	if loc == nil {
		loc = time.UTC
	}
	for _, iv := range s.intervals(dateOf(t, loc), loc) {
		if !t.Before(iv.from) && t.Before(iv.to) {
			return true
		}
	}
	return false
}

// String is the schedule as it was written.
func (s *Schedule) String() string {
	if s == nil {
		return ""
	}
	return s.raw
}

// WeeklyMinutes is how much working time a week of the schedule holds (a nil schedule: all of it).
func (s *Schedule) WeeklyMinutes() int {
	if s == nil {
		return 7 * 24 * 60
	}
	total := 0
	for _, e := range s.entries {
		span := e.end - e.start
		if span <= 0 {
			span += 24 * 60
		}
		for _, on := range e.days {
			if on {
				total += span
			}
		}
	}
	return total
}
