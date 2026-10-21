package sched

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func compact(t *testing.T, b []byte) string {
	t.Helper()
	var c bytes.Buffer
	if err := json.Compact(&c, b); err != nil {
		t.Fatal(err)
	}
	return c.String()
}

// The file round-trips: the adapter's render comes back byte for byte, a live Hermes-shaped file
// (keys the scheduler does not model, a state key needletail adds) keeps every key, value and
// order, and a bare list stays a list.
func TestFileRoundTrip(t *testing.T) {
	rendered, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "engine", "needletail", "testdata", "home", "cron", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := f.Encode(); string(out) != string(rendered) {
		t.Errorf("render round trip:\n%s\nwant\n%s", out, rendered)
	}
	live, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "engine", "needletail", "testdata", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err = Parse(live)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := f.Encode()
	if compact(t, out) != compact(t, live) {
		t.Errorf("live round trip:\n%s", out)
	}
	if len(f.Jobs) != 2 || f.Jobs[0].Str("monitor_url") != "" || !f.Jobs[0].Has("monitor_url") || f.Jobs[1].Str("prompt") != "Ping <me>" {
		t.Errorf("jobs = %+v", f.Jobs)
	}
	j := f.Jobs[0]
	if j.ID() != "a1b2c3d4e5f6" || j.Int("failure_streak") != 2 || !j.Bool("running") || j.Schedule().Expr != "0 8 * * 1-5" {
		t.Errorf("job 0 read as %s %d %v %+v", j.ID(), j.Int("failure_streak"), j.Bool("running"), j.Schedule())
	}
	if times, done := f.Jobs[1].Repeat(); times != 3 || done != 1 {
		t.Errorf("repeat = %d %d", times, done)
	}
	f.Jobs[1].SetCompleted(2)
	f.Jobs[1].Set("last_status", "ok")
	f.Jobs[1].Set("brand_new", map[string]string{"a": "<b>"})
	out, _ = f.Encode()
	again, _ := Parse(out)
	if again.Jobs[1].Str("last_status") != "ok" || again.Jobs[1].keys[len(again.Jobs[1].keys)-1] != "brand_new" || !bytes.Contains(out, []byte(`"a": "<b>"`)) {
		t.Errorf("after set:\n%s", out)
	}
	if _, done := again.Jobs[1].Repeat(); done != 2 || !bytes.Contains(out, []byte("\"times\": 3,\n")) {
		t.Errorf("repeat after set:\n%s", out)
	}

	list := []byte(`[{"id":"x","skills":null,"skill":"one"},{"id":7,"skills":"two"}]`)
	f, err = Parse(list)
	if err != nil {
		t.Fatal(err)
	}
	out, _ = f.Encode()
	if compact(t, out) != string(list) {
		t.Errorf("list round trip: %s", out)
	}
	if got := f.Jobs[0].Skills(); len(got) != 1 || got[0] != "one" {
		t.Errorf("legacy skill = %v", got)
	}
	if got := f.Jobs[1].Skills(); f.Jobs[1].ID() != "7" || len(got) != 1 || got[0] != "two" {
		t.Errorf("job 7 skills = %v", got)
	}
	for _, bad := range []string{`{"jobs": 3}`, `"x"`, `{"jobs":[1]}`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
	empty, err := ReadFile(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || len(empty.Jobs) != 0 {
		t.Fatal(empty, err)
	}
	if out, _ := empty.Encode(); string(out) != "{\n  \"jobs\": []\n}\n" {
		t.Errorf("empty = %q", out)
	}
}
