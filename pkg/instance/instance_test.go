package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mk(t *testing.T, dir, yaml string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, File), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFindOrder(t *testing.T) {
	tmp := t.TempDir()
	a := mk(t, filepath.Join(tmp, "a"), "slug: acme\n")
	b := mk(t, filepath.Join(tmp, "b"), "slug: acme\n")
	deep := filepath.Join(a, "agents", "x")
	os.MkdirAll(deep, 0o755)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	bin := filepath.Join(b, "stormo", "bin", "stormo") // an engine built inside instance b
	cases := []struct {
		name string
		cwd  string
		env  map[string]string
		args []string
		exe  string
		want string
	}{
		{"flag wins", deep, map[string]string{"STORMO_INSTANCE": b}, []string{"start", "--instance", a}, "", a},
		{"flag=", deep, nil, []string{"--instance=" + b}, "", b},
		{"env next", deep, map[string]string{"STORMO_INSTANCE": b}, nil, "", b},
		{"walk up from cwd", deep, nil, nil, bin, a},
		{"then above the binary", tmp, nil, nil, bin, b},
		{"nothing", tmp, nil, nil, "", ""},
	}
	for _, c := range cases {
		if got := Find(c.cwd, env(c.env), c.exe, c.args); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDefaultsAndPins(t *testing.T) {
	tmp := t.TempDir()
	i, err := Load(mk(t, filepath.Join(tmp, "d"), "slug: acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	if i.Name != "acme Stormo" || i.Org != "acme" || i.Author != "acme" || i.Aws.Bucket != Unset || i.Aws.Efs.FileSystemID != Unset {
		t.Errorf("%+v", i)
	}
	if (i.Names != Names{"acme-stormo", "acme/stormo", "acme-state", "acme-knowledge", "acme-shared-docs"}) {
		t.Errorf("names = %+v", i.Names)
	}
	p, err := Load(mk(t, filepath.Join(tmp, "p"), "slug: acme\nnames: {resource: acme-swarm, state_dir: acme-data}\ndeploy: {aws: {account: 123456789012}}\n"))
	if err != nil || p.Names.Resource != "acme-swarm" || p.Names.StateDir != "acme-data" || p.Names.Secret != "acme/stormo" || p.Aws.Account != "123456789012" {
		t.Errorf("%+v %v", p, err)
	}
	for _, bad := range []string{"name: X\n", "slug: Acme\n"} {
		if _, err := Load(mk(t, filepath.Join(tmp, "bad"), bad)); err == nil || !strings.Contains(err.Error(), "slug is required") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestCoreLearning(t *testing.T) {
	tmp := t.TempDir()
	off, err := Load(mk(t, filepath.Join(tmp, "off"), "slug: acme\n"))
	if err != nil || off.CoreLearning.At != "" || off.CoreLearning.StaggerMinutes != 2 || off.CoreLearning.QuietWaitMinutes != 30 {
		t.Fatalf("defaults: no schedule, stagger 2, quiet wait 30: %+v %v", off.CoreLearning, err)
	}
	on, err := Load(mk(t, filepath.Join(tmp, "on"), "slug: acme\ncore:\n  learning:\n    at: \"03:30\"\n    timezone: Europe/Lisbon\n    stagger_minutes: 0\n    agents: [atlas]\n"))
	if err != nil || on.CoreLearning.At != "03:30" || on.CoreLearning.Timezone != "Europe/Lisbon" || on.CoreLearning.StaggerMinutes != 0 || strings.Join(on.CoreLearning.Agents, ",") != "atlas" {
		t.Fatalf("%+v %v", on.CoreLearning, err)
	}
	for i, bad := range []string{"at: \"3:00\"", "at: \"24:00\"", "timezone: Mars/Olympus", "stagger_minutes: -1"} {
		if _, err := Load(mk(t, filepath.Join(tmp, fmt.Sprint("bad", i)), "slug: acme\ncore:\n  learning:\n    "+bad+"\n")); err == nil || !strings.Contains(err.Error(), "core.learning") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	ex, err := Load("../../examples/minimal")
	if err != nil || ex.CoreLearning.At != "03:00" {
		t.Errorf("the example runs the cycle at 03:00: %+v %v", ex.CoreLearning, err)
	}
}
