package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// The adapter's golden home is what the runtime reads: the two must agree on needletail.yaml.
func TestLoadGoldenHome(t *testing.T) {
	home := filepath.Join("..", "..", "..", "pkg", "engine", "needletail", "testdata", "home")
	c, err := Load(home, env(map[string]string{"SWARM_UNIT": "sales", "NEEDLETAIL_API_PORT": "9000"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "atlas" || c.Model.Primary.KeyEnv != "OPENROUTER_API_KEY" || c.Model.Primary.Model != "openai/gpt-6-luna" {
		t.Errorf("config = %+v", c.Config)
	}
	if c.Limits.Turns != DefaultTurns || c.Limits.CommandTimeout != DefaultCommandTimeout || c.Limits.LuauTimeout != DefaultLuauTimeout ||
		c.Limits.ScriptTimeout != DefaultScriptTimeout || c.Memory.MemoryCharLimit != 2200 {
		t.Errorf("defaults = %+v %+v", c.Limits, c.Memory)
	}
	if c.Port != 9000 || c.SharedDir != "/shared" || len(c.SharedRoots()) != 2 || c.SharedRoots()[1] != "/shared/sales" {
		t.Errorf("env = %d %s %v", c.Port, c.SharedDir, c.SharedRoots())
	}
	if len(c.Endpoints()) != 1 || len(c.Luau.Grants) != 2 {
		t.Errorf("endpoints %v grants %v", c.Endpoints(), c.Luau.Grants)
	}
}

func TestLoadRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key": "version: 1\nagent: a\nmodel: {primary: {base_url: http://x, model: m}}\nsurprise: 1\n",
		"version 2":   "version: 2\nagent: a\nmodel: {primary: {base_url: http://x, model: m}}\n",
		"no model":    "version: 1\nagent: a\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, File), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir, env(nil)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, File), []byte("version: 1\nmodel: {primary: {base_url: http://x, model: m}}\n"), 0o644)
	if _, err := Load(dir, env(map[string]string{"NEEDLETAIL_API_PORT": "http"})); err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("bad port: %v", err)
	}
	if c, err := Load("", env(map[string]string{"AGENT_HOME": dir})); err != nil || c.Home != dir {
		t.Errorf("AGENT_HOME: %v", err)
	}
}

// The wall clock schedules run on: needletail.yaml's timezone, else $TZ, else UTC.
func TestLocation(t *testing.T) {
	dir := t.TempDir()
	base := "version: 1\nmodel: {primary: {base_url: http://x, model: m}}\n"
	for _, c := range []struct{ yaml, tz, want string }{
		{"timezone: America/New_York\n", "Europe/Lisbon", "America/New_York"},
		{"", "Europe/Lisbon", "Europe/Lisbon"},
		{"", "", "UTC"},
		{"", "Not/AZone", "UTC"},
	} {
		_ = os.WriteFile(filepath.Join(dir, File), []byte(base+c.yaml), 0o644)
		cfg, err := Load(dir, env(map[string]string{"TZ": c.tz}))
		if err != nil || cfg.Location == nil || cfg.Location.String() != c.want {
			t.Errorf("%q TZ=%q: %v %v", c.yaml, c.tz, cfg.Location, err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, File), []byte(base+"timezone: Not/AZone\n"), 0o644)
	if _, err := Load(dir, env(nil)); err == nil {
		t.Error("a bad timezone was accepted")
	}
}
