package sched

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Cron is a parsed 5-field cron expression: minute hour day-of-month month day-of-week, each a
// comma list of `*`, `n`, `a-b`, with an optional `/step` (`*/15`, `1-30/5`, `5/10` = 5-max/10).
// Months and weekdays take names (JAN…DEC, SUN…SAT); weekday 7 is Sunday. As in Vixie cron
// (and croniter, which Hermes uses), when both day fields are restricted a day matches either;
// a field written `*` (or `?`) is unrestricted.
type Cron struct {
	minute, hour, dom, month, dow uint64 // bit n set: value n matches
	domStar, dowStar              bool
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

// ParseCron reads a 5-field expression.
func ParseCron(expr string) (*Cron, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron expression %q: want 5 fields (minute hour day month weekday), got %d", expr, len(f))
	}
	c := &Cron{}
	var err error
	if c.minute, _, err = field(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q minute: %w", expr, err)
	}
	if c.hour, _, err = field(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q hour: %w", expr, err)
	}
	if c.dom, c.domStar, err = field(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q day of month: %w", expr, err)
	}
	if c.month, _, err = field(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q month: %w", expr, err)
	}
	if c.dow, c.dowStar, err = field(f[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("cron %q weekday: %w", expr, err)
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow = c.dow&^(1<<7) | 1
	}
	return c, nil
}

// field parses one field into a bit set; star reports an unrestricted field.
func field(s string, lo, hi int, names map[string]int) (uint64, bool, error) {
	if s == "*" || s == "?" {
		return span(lo, hi, 1), true, nil
	}
	var bits uint64
	for _, item := range strings.Split(s, ",") {
		if item == "" {
			return 0, false, errors.New("empty list item")
		}
		rng, step := item, 1
		if r, st, ok := strings.Cut(item, "/"); ok {
			n, err := strconv.Atoi(st)
			if err != nil || n <= 0 {
				return 0, false, fmt.Errorf("bad step %q", st)
			}
			rng, step = r, n
		}
		a, b := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			var err error
			if a, err = value(x, names); err != nil {
				return 0, false, err
			}
			if b, err = value(y, names); err != nil {
				return 0, false, err
			}
		default:
			v, err := value(rng, names)
			if err != nil {
				return 0, false, err
			}
			a = v
			if step == 1 {
				b = v
			} // `5/10` runs from 5 to the field's maximum
		}
		if a < lo || b > hi || a > b {
			return 0, false, fmt.Errorf("%q is outside %d-%d", item, lo, hi)
		}
		bits |= span(a, b, step)
	}
	return bits, false, nil
}

func value(s string, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return n, nil
}

func span(a, b, step int) uint64 {
	var bits uint64
	for v := a; v <= b; v += step {
		bits |= 1 << v
	}
	return bits
}

func has(bits uint64, v int) bool { return bits&(1<<v) != 0 }

func (c *Cron) dayMatches(t time.Time) bool {
	dom, dow := has(c.dom, t.Day()), has(c.dow, int(t.Weekday()))
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	}
	return dom || dow
}

// Matches is whether t's wall clock (in t's own location) is one of the expression's times.
func (c *Cron) Matches(t time.Time) bool {
	return t.Second() == 0 && t.Nanosecond() == 0 && has(c.minute, t.Minute()) && has(c.hour, t.Hour()) &&
		has(c.month, int(t.Month())) && c.dayMatches(t)
}

// searchYears bounds Next: an expression that matches nothing (Feb 30) has no next run.
const searchYears = 5

// Next is the first time strictly after after whose wall clock in loc matches. A wall-clock time
// that does not exist (a DST gap) is skipped; zero when nothing matches within five years.
func (c *Cron) Next(after time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	after = after.In(loc)
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, loc)
	end := day.AddDate(searchYears, 0, 0)
	for ; day.Before(end); day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc) {
		if !has(c.month, int(day.Month())) || !c.dayMatches(day) {
			continue
		}
		for h := 0; h < 24; h++ {
			if !has(c.hour, h) {
				continue
			}
			for m := 0; m < 60; m++ {
				if !has(c.minute, m) {
					continue
				}
				t := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
				if t.Hour() != h || t.Minute() != m || t.Day() != day.Day() {
					continue // normalised out of a DST gap
				}
				if t.After(after) {
					return t
				}
			}
		}
	}
	return time.Time{}
}

// ---- schedule strings (the cronjob_manage tool's `schedule`) --------------------------------

var (
	durationRe = regexp.MustCompile(`^(\d*)\s*(m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days)$`)
	clockRe    = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)?$`)
	cronCharRe = regexp.MustCompile(`^[A-Za-z\d*,/?-]+$`)
	dateRe     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)
	daySpecs   = map[string]string{"day": "*", "daily": "*", "everyday": "*", "weekday": "1-5", "weekdays": "1-5", "weekend": "0,6", "weekends": "0,6"}
	weekdays   = map[string]string{"sunday": "0", "sun": "0", "monday": "1", "mon": "1", "tuesday": "2", "tue": "2", "tues": "2",
		"wednesday": "3", "wed": "3", "weds": "3", "thursday": "4", "thu": "4", "thur": "4", "thurs": "4", "friday": "5", "fri": "5",
		"saturday": "6", "sat": "6"}
)

// ErrOneShot is a one-time schedule ("in 30m", a timestamp): jobs here recur.
var ErrOneShot = errors.New(`one-time schedules ("in 30m", a timestamp) are not supported: use an interval ("30m", "every 2h") or a cron expression`)

// ParseSchedule reads a schedule as Hermes' cronjob tool takes it: an interval ("30m", "2h",
// "1d", "every 2h", "every hour"), a day and time ("every monday 9am", "weekdays at 9am",
// "every day at 14:00"), or a 5-field cron expression. One-time schedules are refused
// (ErrOneShot): agent.yaml has no form for them.
func ParseSchedule(s string) (wire.JobSchedule, error) {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	every := strings.HasPrefix(lower, "every ")
	rest := lower
	if every {
		rest = strings.TrimSpace(lower[len("every "):])
	}
	if expr, ok := naturalCron(rest); ok {
		if _, err := ParseCron(expr); err != nil {
			return wire.JobSchedule{}, err
		}
		return wire.JobSchedule{Kind: "cron", Expr: expr, Display: s}, nil
	}
	if every {
		m, err := duration(rest)
		if err != nil {
			return wire.JobSchedule{}, err
		}
		return interval(m), nil
	}
	if parts := strings.Fields(s); len(parts) == 5 && all(parts, cronCharRe.MatchString) {
		if _, err := ParseCron(s); err != nil {
			return wire.JobSchedule{}, err
		}
		return wire.JobSchedule{Kind: "cron", Expr: s, Display: s}, nil
	}
	if strings.HasPrefix(lower, "in ") || strings.Contains(s, "T") || dateRe.MatchString(s) {
		return wire.JobSchedule{}, ErrOneShot
	}
	if m, err := duration(lower); err == nil {
		return interval(m), nil
	}
	return wire.JobSchedule{}, fmt.Errorf(`schedule %q: use an interval ("30m", "every 2h"), a day and time ("every monday 9am", "weekdays at 9am") or a cron expression ("0 9 * * 1-5")`, s)
}

func interval(m int) wire.JobSchedule {
	return wire.JobSchedule{Kind: "interval", Minutes: m, Display: fmt.Sprintf("every %dm", m)}
}

func all(l []string, f func(string) bool) bool {
	for _, x := range l {
		if !f(x) {
			return false
		}
	}
	return true
}

func duration(s string) (int, error) {
	m := durationRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf(`bad interval %q: use "30m", "2h", "1d" or a bare unit ("hour")`, s)
	}
	n := 1
	if m[1] != "" {
		var err error
		if n, err = strconv.Atoi(m[1]); err != nil || n > MaxIntervalMinutes {
			return 0, fmt.Errorf("bad interval %q: at most %d days", s, MaxIntervalMinutes/1440)
		}
	}
	if n <= 0 {
		return 0, fmt.Errorf("bad interval %q: it must be at least a minute", s)
	}
	minutes := n * map[byte]int{'m': 1, 'h': 60, 'd': 1440}[m[2][0]]
	if minutes > MaxIntervalMinutes {
		return 0, fmt.Errorf("bad interval %q: at most %d days", s, MaxIntervalMinutes/1440)
	}
	return minutes, nil
}

// MaxIntervalMinutes bounds an interval (366 days): a longer one is refused, and one already in
// a jobs file has no next run (it would overflow a time.Duration).
const MaxIntervalMinutes = 366 * 1440

// naturalCron is "<days> [at] <time>" as a cron expression: days a keyword (day, weekdays,
// weekend) or weekday names, time 9am, 9:30pm, 14:00, noon or midnight.
func naturalCron(rest string) (string, bool) {
	tokens := strings.Fields(strings.ReplaceAll(rest, ",", " "))
	if len(tokens) == 0 {
		return "", false
	}
	dow, idx := daySpecs[tokens[0]], 1
	if dow == "" {
		days := []string{}
		idx = len(tokens)
		for i, t := range tokens {
			if t == "and" {
				continue
			}
			d, ok := weekdays[t]
			if !ok {
				idx = i
				break
			}
			if !strings.Contains(","+strings.Join(days, ",")+",", ","+d+",") {
				days = append(days, d)
			}
		}
		if len(days) == 0 {
			return "", false
		}
		dow = strings.Join(days, ",")
	}
	rest2 := tokens[idx:]
	if len(rest2) > 0 && rest2[0] == "at" {
		rest2 = rest2[1:]
	}
	if len(rest2) == 0 {
		return "", false
	}
	h, m, ok := clock(strings.Join(rest2, ""))
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d %d * * %s", m, h, dow), true
}

func clock(t string) (int, int, bool) {
	switch t {
	case "noon", "midday":
		return 12, 0, true
	case "midnight":
		return 0, 0, true
	}
	m := clockRe.FindStringSubmatch(t)
	if m == nil {
		return 0, 0, false
	}
	h, _ := strconv.Atoi(m[1])
	min := 0
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		if h < 1 || h > 12 {
			return 0, 0, false
		}
		h = h % 12
		if m[3] == "pm" {
			h += 12
		}
	}
	if h > 23 || min > 59 {
		return 0, 0, false
	}
	return h, min, true
}
