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

// A telegram channel: Hermes' TELEGRAM_* names (the token is a secret); the local overlay wins;
// needletail.yaml names the channel so serve starts the gateway.
func TestTelegramEnvAndChannel(t *testing.T) {
	inst, a := atlas(t)
	yes := true
	a.Channels = append(a.Channels, manifest.Channel{Kind: "telegram", SlackSettings: manifest.SlackSettings{AllowedUsers: []string{"1001", "1002"}, HomeChannel: "-1009999"},
		Local: &manifest.SlackSettings{AllowAllUsers: &yes}})
	n := New(inst)
	e := n.Env(a, manifest.Local).Map()
	for k, want := range map[string]string{"TELEGRAM_ALLOWED_USERS": "1001,1002", "TELEGRAM_HOME_CHANNEL": "-1009999", "TELEGRAM_ALLOW_ALL_USERS": "true"} {
		if e[k] != want {
			t.Errorf("%s = %q, want %q", k, e[k], want)
		}
	}
	if _, ok := n.Env(a, manifest.AWS).Map()["TELEGRAM_ALLOW_ALL_USERS"]; ok {
		t.Error("the local overlay reached AWS")
	}
	if _, ok := e["TELEGRAM_BOT_TOKEN"]; ok {
		t.Error("the token is not env")
	}
	ac := &agentConfig{}
	c := renderConfig(a, inst.Connections[0], manifest.Local, manifest.CoreEndpoints{}, ac)
	if strings.Join(c.Channels, ",") != "slack,telegram" {
		t.Errorf("channels %v", c.Channels)
	}
}
