package clock

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustSchedule(t *testing.T, s string) *Schedule {
	t.Helper()
	sc, err := ParseSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseSchedule(t *testing.T) {
	for _, bad := range []string{"", " , ", "08:00-19:00", "8-19 mon", "08:00-19:00 funday", "08:00-08:00 mon", "25:00-26:00 mon", "08:60-09:00 mon", "08:00-24:30 mon"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"08:00-19:00 mon-sat", "08:00-19:00 mon-sat, 09:00-13:00 sun", "22:00-06:00 daily", "00:00-24:00 fri-mon", "09:00-17:00 wed"} {
		s, err := ParseSchedule(good)
		if err != nil {
			t.Errorf("%q: %v", good, err)
		}
		if s.String() != good {
			t.Errorf("String %q, want %q", s.String(), good)
		}
	}
	s := mustSchedule(t, "00:00-24:00 fri-mon")
	for d, want := range map[time.Weekday]bool{time.Friday: true, time.Saturday: true, time.Sunday: true, time.Monday: true, time.Tuesday: false} {
		if s.entries[0].days[d] != want {
			t.Errorf("fri-mon: %v is %v", d, !want)
		}
	}
}

func TestAddWorking(t *testing.T) {
	pty := mustLoc(t, "America/Panama")
	work := mustSchedule(t, "08:00-19:00 mon-sat")
	for _, c := range []struct {
		from string
		d    time.Duration
		want string
	}{
		{"2026-11-21 18:50", 45 * time.Minute, "2026-11-23 08:35"}, // Saturday evening → Monday morning
		{"2026-11-18 10:00", 45 * time.Minute, "2026-11-18 10:45"}, // inside the hours
		{"2026-11-18 06:00", 45 * time.Minute, "2026-11-18 08:45"}, // before opening
		{"2026-11-18 20:00", 0, "2026-11-19 08:00"},                // zero: the next working instant
		{"2026-11-18 18:00", 2 * time.Hour, "2026-11-19 09:00"},    // carried over the night
		{"2026-11-22 12:00", time.Hour, "2026-11-23 09:00"},        // Sunday: closed
	} {
		got := work.AddWorking(at(t, pty, c.from), c.d, pty)
		if want := at(t, pty, c.want); !got.Equal(want) {
			t.Errorf("%s + %v = %v, want %v", c.from, c.d, got.In(pty), want)
		}
	}
	if got, from := (*Schedule)(nil).AddWorking(at(t, pty, "2026-11-21 18:50"), 45*time.Minute, pty), at(t, pty, "2026-11-21 18:50"); !got.Equal(from.Add(45 * time.Minute)) {
		t.Errorf("a nil schedule counts every time: %v", got)
	}
	night := mustSchedule(t, "22:00-06:00 daily")
	if got := night.AddWorking(at(t, pty, "2026-11-18 23:00"), 2*time.Hour, pty); !got.Equal(at(t, pty, "2026-11-19 01:00")) {
		t.Errorf("past midnight: %v", got.In(pty))
	}
	if got := night.AddWorking(at(t, pty, "2026-11-19 05:00"), 2*time.Hour, pty); !got.Equal(at(t, pty, "2026-11-19 23:00")) {
		t.Errorf("an overnight span carried to the next night: %v", got.In(pty))
	}
}

func TestAddWorkingAcrossDaylightSaving(t *testing.T) {
	rome := mustLoc(t, "Europe/Rome")
	s := mustSchedule(t, "01:00-04:00 daily")
	// 2026-03-29: 02:00 → 03:00, the span 01:00-04:00 is 2 hours long
	if got := s.AddWorking(at(t, rome, "2026-03-29 01:00"), 2*time.Hour+30*time.Minute, rome); !got.Equal(at(t, rome, "2026-03-30 01:30")) {
		t.Errorf("spring forward: %v", got.In(rome))
	}
	// 2026-10-25: 03:00 → 02:00, the span is 4 hours long (the repeated hour counts as it passes)
	from := Wall(2026, time.October, 25, 1, 0, 0, rome, time.Time{})
	if got := s.AddWorking(from, 4*time.Hour, rome); !got.Equal(Wall(2026, time.October, 25, 4, 0, 0, rome, time.Time{})) {
		t.Errorf("fall back: %v", got.In(rome))
	}
	// a start a change skips moves forward: 02:30 on 2026-03-29 is 03:30
	skip := mustSchedule(t, "02:30-05:00 daily")
	if got := skip.AddWorking(at(t, rome, "2026-03-29 00:00"), 0, rome); !got.Equal(at(t, rome, "2026-03-29 03:30")) {
		t.Errorf("a skipped start: %v", got.In(rome))
	}
}

func TestWorking(t *testing.T) {
	pty := mustLoc(t, "America/Panama")
	s := mustSchedule(t, "08:00-19:00 mon-sat")
	for when, want := range map[string]bool{"2026-11-18 08:00": true, "2026-11-18 18:59": true, "2026-11-18 19:00": false, "2026-11-22 10:00": false, "2026-11-18 07:59": false} {
		if got := s.Working(at(t, pty, when), pty); got != want {
			t.Errorf("Working(%s) = %v", when, got)
		}
	}
	if !(*Schedule)(nil).Working(time.Now(), pty) {
		t.Error("a nil schedule is always working")
	}
}

func TestWallMatchesItsRules(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// 2026-03-08 02:30 does not exist in New York: forward to 03:30 (time.Date would go backward)
	if got := Wall(2026, time.March, 8, 2, 30, 0, ny, time.Time{}); got.Hour() != 3 || got.Minute() != 30 {
		t.Errorf("skipped: %v", got)
	}
	// 2026-11-01 01:30 repeats: the first, unless now is between them
	first := Wall(2026, time.November, 1, 1, 30, 0, ny, time.Time{})
	if _, off := first.Zone(); off != -4*3600 {
		t.Errorf("repeated: the first (EDT), got %v", first)
	}
	if second := Wall(2026, time.November, 1, 1, 30, 0, ny, first.Add(time.Minute)); !second.Equal(first.Add(time.Hour)) {
		t.Errorf("repeated, now after the first: %v", second)
	}
}

// A 24:00 end is the next day's midnight, and a day is never taken from a clock a change moved.
func TestMidnightsAndCivilDays(t *testing.T) {
	scl := mustLoc(t, "America/Santiago")
	// 2026-04-05 00:00: Santiago's clocks go back from 00:00 to 23:00, so 24:00 on the 4th repeats
	got := Wall(2026, time.April, 4, 24, 0, 0, scl, time.Time{})
	if want := Wall(2026, time.April, 5, 0, 0, 0, scl, time.Time{}); !got.Equal(want) {
		t.Errorf("24:00 = %v, want the next day's midnight %v", got, want)
	}
	sp := mustLoc(t, "America/Sao_Paulo")
	// 2018-11-04: São Paulo's midnight did not exist (00:00 → 01:00); a Sunday overnight span
	s := mustSchedule(t, "22:00-06:00 sun")
	mon := time.Date(2018, time.November, 5, 0, 30, 0, 0, sp)
	if !s.Working(mon, sp) {
		t.Errorf("Monday 00:30 inside Sunday's overnight span")
	}
	end := mustSchedule(t, "08:00-24:00 mon-fri")
	pty := mustLoc(t, "America/Panama")
	if got := end.AddWorking(at(t, pty, "2026-11-20 23:00"), 2*time.Hour, pty); !got.Equal(at(t, pty, "2026-11-23 09:00")) {
		t.Errorf("a 24:00 end on Friday: %v", got.In(pty))
	}
}
