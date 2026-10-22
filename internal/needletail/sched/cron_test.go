package sched

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/tools"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04 Mon", s)
	if err != nil {
		panic(err)
	}
	return t
}

// The expressions agents' schedules use, and the cron corners: steps, ranges, names, Sunday as
// 7, the day-of-month/day-of-week OR, a month without the day.
func TestCronNext(t *testing.T) {
	for _, c := range []struct{ expr, after, want string }{
		{"0 9 * * 1-5", "2026-10-23 09:00 Fri", "2026-10-26 09:00 Mon"}, // weekdays: Friday's run is past, Monday next
		{"0 9 * * 1-5", "2026-10-21 08:59 Wed", "2026-10-21 09:00 Wed"},
		{"30 9 * * 1", "2026-10-21 10:00 Wed", "2026-10-26 09:30 Mon"},
		{"0 10 * * 1", "2026-10-26 10:00 Mon", "2026-11-02 10:00 Mon"}, // strictly after
		{"0 8 * * *", "2026-12-31 23:59 Thu", "2027-01-01 08:00 Fri"},
		{"*/15 * * * *", "2026-10-21 10:07 Wed", "2026-10-21 10:15 Wed"},
		{"5/20 * * * *", "2026-10-21 10:46 Wed", "2026-10-21 11:05 Wed"},
		{"0 0 1 * *", "2026-10-21 10:00 Wed", "2026-11-01 00:00 Sun"},
		{"0 12 * JAN,jul SUN", "2026-10-21 10:00 Wed", "2027-01-03 12:00 Sun"},
		{"0 12 * * 7", "2026-10-21 10:00 Wed", "2026-10-25 12:00 Sun"},
		{"0 12 13 * 5", "2026-10-21 10:00 Wed", "2026-10-23 12:00 Fri"}, // 13th OR Friday
		{"0 12 13 * *", "2026-10-21 10:00 Wed", "2026-11-13 12:00 Fri"},
		{"0 0 29 2 *", "2026-10-21 10:00 Wed", "2028-02-29 00:00 Tue"},
		{"0-10/5 9-17/4 * * mon-fri", "2026-10-21 17:06 Wed", "2026-10-21 17:10 Wed"},
		{"0-10/5 9-17/4 * * mon-fri", "2026-10-23 17:10 Fri", "2026-10-26 09:00 Mon"},
		{"0 9 ? * 1-5", "2026-10-24 09:00 Sat", "2026-10-26 09:00 Mon"},
	} {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := cr.Next(at(c.after), time.UTC); !got.Equal(at(c.want)) {
			t.Errorf("%s after %s = %s, want %s", c.expr, c.after, got.Format("2006-01-02 15:04 Mon"), c.want)
		}
	}
	never, _ := ParseCron("0 0 30 2 *")
	if got := never.Next(at("2026-10-21 10:00 Wed"), time.UTC); !got.IsZero() {
		t.Errorf("Feb 30 = %s", got)
	}
}

// Cron runs on the wall clock of its location; a time a DST change skips is skipped.
func TestCronLocation(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	cr, _ := ParseCron("0 9 * * *")
	got := cr.Next(time.Date(2026, 3, 7, 15, 0, 0, 0, time.UTC), ny) // the day before spring forward
	if want := time.Date(2026, 3, 8, 9, 0, 0, 0, ny); !got.Equal(want) || got.UTC().Hour() != 13 {
		t.Errorf("next = %s (%s UTC)", got, got.UTC())
	}
	gap, _ := ParseCron("30 2 * * *") // 02:30 does not exist on 2026-03-08 in New York
	if got := gap.Next(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), ny); !got.Equal(time.Date(2026, 3, 9, 2, 30, 0, 0, ny)) {
		t.Errorf("gap next = %s", got)
	}
}

func TestCronRefusals(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "1,,2 * * * *", "* * * FOO *"} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("%q parsed", expr)
		}
	}
}

func TestParseSchedule(t *testing.T) {
	for in, want := range map[string]string{
		"30m":                   "interval 30 every 30m",
		"2h":                    "interval 120 every 120m",
		"1d":                    "interval 1440 every 1440m",
		"every 2h":              "interval 120 every 120m",
		"every hour":            "interval 60 every 60m",
		"Every 90 minutes":      "interval 90 every 90m",
		"0 9 * * 1-5":           "cron 0 9 * * 1-5 0 9 * * 1-5",
		"0 9 * * MON-FRI":       "cron 0 9 * * MON-FRI 0 9 * * MON-FRI",
		"every monday 9am":      "cron 0 9 * * 1 every monday 9am",
		"weekdays at 9:30am":    "cron 30 9 * * 1-5 weekdays at 9:30am",
		"every day at 14:00":    "cron 0 14 * * * every day at 14:00",
		"every mon, wed at 6pm": "cron 0 18 * * 1,3 every mon, wed at 6pm",
		"every weekend noon":    "cron 0 12 * * 0,6 every weekend noon",
	} {
		s, err := ParseSchedule(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		got := s.Kind + " "
		if s.Kind == "interval" {
			got += strconv.Itoa(s.Minutes)
		} else {
			got += s.Expr
		}
		if got += " " + s.Display; got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"in 30m", "2026-11-01T09:00:00", "2026-11-01"} {
		if _, err := ParseSchedule(in); !errors.Is(err, ErrOneShot) {
			t.Errorf("%q: %v, want ErrOneShot", in, err)
		}
	}
	for _, in := range []string{"", "soon", "every", "every 0m", "every monday 25:00", "61 * * * *"} {
		if _, err := ParseSchedule(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}

// The minimum gap holds across every daylight-saving change of the year ahead, not only the
// ones of the first week: in Europe/Rome `0,59 1,3 * * *` made in October is a minute apart on
// 2027-03-28 (01:59+01:00, then 03:00+02:00).
func TestMinGapAcrossLaterDSTChanges(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 21, 9, 0, 0, 0, rome)
	for expr, want := range map[string]time.Duration{"0,59 1,3 * * *": time.Minute, "0 1,3 * * *": time.Hour, "0 9 * * *": 23 * time.Hour,
		"30 2 * * *": 24 * time.Hour} {
		c, err := ParseCron(expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.MinGap(from, rome); got != want {
			t.Errorf("%s: gap %s, want %s", expr, got, want)
		}
	}
	r := bareRig(t, from)
	r.s.o.Location = rome
	reg := tools.NewRegistry(r.s.Tool(nil))
	if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"create","schedule":"0,59 1,3 * * *","prompt":"x"}`); !failed {
		t.Errorf("a schedule a minute apart in March accepted: %s", out)
	}
}
