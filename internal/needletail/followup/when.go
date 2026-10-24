package followup

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ParseIn reads a delay as follow_up's `in` takes it: one or more <number><unit> parts ("10s",
// "90m", "2h", "3d", "1h30m", "10 seconds", "2 hours and 5 minutes"), units s, m, h and d (also
// sec(ond)s, min(ute)s, hour(s), day(s)), an optional leading "in". Days are 24 hours of elapsed
// time. Whether it is within the bounds is the caller's (MinDelay, MaxAhead).
func ParseIn(s string) (time.Duration, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.TrimSpace(strings.TrimPrefix(t, "in "))
	if !inWholeRe.MatchString(t) {
		return 0, fmt.Errorf(`%q is not a delay: use "10s", "90m", "2h", "3d" (or "1h30m")`, s)
	}
	secs := 0.0
	for _, p := range inRe.FindAllStringSubmatch(t, -1) {
		n, err := strconv.ParseFloat(p[1], 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a delay", s)
		}
		secs += n * units[p[2]].Seconds()
	}
	if secs > 2*MaxAhead.Seconds() {
		return 0, fmt.Errorf("%q is more than %d days ahead", s, int(MaxAhead/(24*time.Hour)))
	}
	return time.Duration(secs * float64(time.Second)), nil
}

const unitAlt = `seconds|second|secs|sec|s|minutes|minute|mins|min|m|hours|hour|hrs|hr|h|days|day|d`

var (
	inRe      = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(` + unitAlt + `)`)
	inWholeRe = regexp.MustCompile(`^(?:\s*(?:and\s+|,\s*)?\d+(?:\.\d+)?\s*(?:` + unitAlt + `))+\s*$`)
	units     = map[string]time.Duration{
		"s": time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
		"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
		"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
		"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	}
)

// ParseAt reads a time as follow_up's `at` takes it, on the agent's wall clock loc: RFC 3339
// ("2026-10-25T09:00:00+02:00", its own offset), a date and time with none ("2026-10-25 09:00",
// read in loc), or a day and a clock: "today 18:00", "tomorrow 9am", "monday 14:00" (the next
// Monday, today when it is Monday and the time is still ahead), "next monday 9:30am" (never
// today), or a clock alone ("14:00", "9pm": today, or tomorrow once it has passed). Clocks are
// 9am, 9:30pm, 14:00, 14:00:30, noon and midnight. A wall time a daylight-saving change skips
// moves forward by the change in every zone (02:30 on the day clocks go from 02:00 to 03:00 is
// 03:30), and one it repeats is the first of the two, unless the first has passed and the second
// has not (now).
func ParseAt(s string, now time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	raw := strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil { // the components as written (UTC: no zone of its own)
			return wall(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), loc, now), nil
		}
	}
	words := strings.Fields(strings.ToLower(raw))
	local := now.In(loc)
	day := local
	roll := 1 // days added when the time has passed today: a clock alone is tomorrow's
	i := 0
	next := len(words) > 0 && words[0] == "next"
	if next {
		i++
	}
	if i < len(words) {
		w := words[i]
		wd, isDay := weekdays[w]
		switch {
		case (w == "today" || w == "tomorrow") && !next:
			if w == "tomorrow" {
				day = local.AddDate(0, 0, 1)
			}
			roll = 0 // the day was named: a time gone is in the past, not moved
			i++
		case isDay:
			ahead := (int(wd) - int(local.Weekday()) + 7) % 7
			if next && ahead == 0 {
				ahead = 7
			}
			day = local.AddDate(0, 0, ahead)
			roll = 7 // that weekday is today and its time has gone: next week's
			if next {
				roll = 0
			}
			i++
		case next:
			return time.Time{}, atError(s)
		}
	}
	if i < len(words) && words[i] == "at" {
		i++
	}
	if i >= len(words) {
		return time.Time{}, atError(s)
	}
	h, m, sec, ok := clock(strings.Join(words[i:], ""))
	if !ok {
		return time.Time{}, atError(s)
	}
	t := wall(day.Year(), day.Month(), day.Day(), h, m, sec, loc, now)
	if !t.After(now) && roll > 0 {
		t = wall(day.Year(), day.Month(), day.Day()+roll, h, m, sec, loc, now)
	}
	return t, nil
}

// wall is a wall-clock time in loc (the date normalised, as time.Date does: the 32nd is the next
// month's 1st). The instants that read as it are those its zone's offsets around it give (an
// offset 6 hours before, at and 6 hours after time.Date's answer: one change at most in that
// span). None: a daylight-saving change skips it, and it moves forward by the change, whichever
// side of UTC the zone is (time.Date leaves the choice unspecified, and moves it backward west of
// UTC). Two: the change repeats it, and it is the first, unless the first is not after now and the
// second is (now zero: the first).
func wall(y int, mo time.Month, d, h, m, s int, loc *time.Location, now time.Time) time.Time {
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

func atError(s string) error {
	return fmt.Errorf(`%q is not a time: use RFC 3339 ("2026-10-25T09:00:00+02:00"), "2026-10-25 09:00", "tomorrow 9am", "monday 14:00", "today 18:30" or "21:00" (your time zone)`, s)
}

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday, "monday": time.Monday, "mon": time.Monday, "tuesday": time.Tuesday, "tue": time.Tuesday,
	"tues": time.Tuesday, "wednesday": time.Wednesday, "wed": time.Wednesday, "thursday": time.Thursday, "thu": time.Thursday,
	"thurs": time.Thursday, "friday": time.Friday, "fri": time.Friday, "saturday": time.Saturday, "sat": time.Saturday,
}

var clockRe = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(?::(\d{2}))?(am|pm|a\.m\.|p\.m\.)?$`)

// clock is a time of day: 9am, 9:30pm, 14:00, 14:00:30, noon, midnight.
func clock(t string) (h, m, s int, ok bool) {
	switch t {
	case "noon", "midday":
		return 12, 0, 0, true
	case "midnight":
		return 0, 0, 0, true
	}
	g := clockRe.FindStringSubmatch(t)
	if g == nil {
		return 0, 0, 0, false
	}
	h, _ = strconv.Atoi(g[1])
	if g[2] != "" {
		m, _ = strconv.Atoi(g[2])
	}
	if g[3] != "" {
		s, _ = strconv.Atoi(g[3])
	}
	switch ampm := strings.ReplaceAll(g[4], ".", ""); ampm {
	case "am", "pm":
		if h < 1 || h > 12 {
			return 0, 0, 0, false
		}
		h %= 12
		if ampm == "pm" {
			h += 12
		}
	case "":
		if g[2] == "" { // a bare number is not a clock ("9" could be anything)
			return 0, 0, 0, false
		}
	}
	if h > 23 || m > 59 || s > 59 {
		return 0, 0, 0, false
	}
	return h, m, s, true
}

// Local is t as the agent reads it: "Sat 24 Oct 2026 14:00:10 CEST (Europe/Madrid)".
func Local(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("Mon 2 Jan 2006 15:04:05 MST") + " (" + loc.String() + ")"
}
