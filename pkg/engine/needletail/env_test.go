package needletail

import (
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/manifest"
)

func TestEnv(t *testing.T) {
	inst, a := atlas(t)
	n := New(inst)
	get := func(target manifest.Target) map[string]string {
		return n.Env(a, target).Map()
	}
	e := get(manifest.Local)
	for k, want := range map[string]string{"AGENT_HOME": "/opt/data", "SWARM_AGENT": "atlas", "SWARM_UNIT": "sales",
		"SWARM_SHARED_DIR": "/shared", "SWARM_CORE_URL": manifest.Core.URL, "NEEDLETAIL_API_PORT": "8642",
		"NEWS_RADAR_HOME": "/opt/data/acme-state/news-radar", "SLACK_HOME_CHANNEL": "C0000000001",
		"SLACK_ALLOWED_USERS": "U0000000001", "SLACK_ALLOW_BOTS": "mentions"} {
		if e[k] != want {
			t.Errorf("%s = %q, want %q", k, e[k], want)
		}
	}
	for k := range e {
		if strings.HasPrefix(k, "HERMES_") {
			t.Errorf("%s set", k)
		}
	}
	if _, ok := get(manifest.AWS)["SWARM_CORE_URL"]; ok {
		t.Error("SWARM_CORE_URL on AWS")
	}
}
