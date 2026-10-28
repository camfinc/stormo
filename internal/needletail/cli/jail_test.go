package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/jail"
)

// The jail is on whenever the split is and the kernel has Landlock; without Landlock (Fargate's
// kernels) the agent runs unconfined, said loudly once and reported in the activity.
func TestJailFor(t *testing.T) {
	spec := jail.Spec{Home: "/opt/data", Shared: []string{"/shared/group", "/shared/sales"}}
	abi := func(n int, err error) func() (int, error) { return func() (int, error) { return n, err } }
	self := func() (string, error) { return "/usr/local/bin/needletail", nil }
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == JailEnv {
				return v
			}
			return ""
		}
	}
	for _, c := range []struct {
		name  string
		split bool
		abi   func() (int, error)
		self  func() (string, error)
		env   string
		on    bool
		state string
		log   string
	}{
		{"jailed", true, abi(10, nil), self, "", true, "landlock ABI 10", "confined by Landlock (ABI 10)"},
		{"ABI 1", true, abi(1, nil), self, "", true, "landlock ABI 1", "ABI 1"},
		{"no Landlock", true, abi(0, errors.New("the kernel has no Landlock (before 5.13)")), self, "", false,
			"off: the kernel has no Landlock (before 5.13)", "JAIL OFF: the kernel has no Landlock"},
		{"split off", false, abi(10, nil), self, "", false, "off: the uid split is off", ""},
		{"turned off", true, abi(10, nil), self, "off", false, "off: NEEDLETAIL_JAIL=off", "development only"},
		{"no binary", true, abi(10, nil), func() (string, error) { return "", errors.New("readlink") }, "", false,
			"off: the runtime's binary is not found", "JAIL OFF"},
	} {
		var logs []string
		js := jailFor(c.split, spec, c.abi, c.self, env(c.env), func(s string) { logs = append(logs, s) })
		if (js.launcher != nil) != c.on || js.state != c.state {
			t.Errorf("%s: launcher %v state %q, want on=%v %q", c.name, js.launcher, js.state, c.on, c.state)
		}
		if js.launcher != nil && (js.launcher.Self != "/usr/local/bin/needletail" || js.launcher.Spec.Home != spec.Home || len(js.launcher.Spec.Shared) != 2) {
			t.Errorf("%s: launcher %+v", c.name, js.launcher)
		}
		if c.log == "" && len(logs) != 0 || c.log != "" && (len(logs) != 1 || !strings.Contains(logs[0], c.log)) {
			t.Errorf("%s: logs %q, want one with %q", c.name, logs, c.log)
		}
	}
}
