package needletail

import (
	"strconv"
	"strings"

	"github.com/camfinc/stormo/pkg/env"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/shared"
)

// Env is the container's non-secret env. The Slack settings use the same names as Hermes, so the
// core (which reads SLACK_HOME_CHANNEL) treats both engines alike.
func (n *Needletail) Env(a *manifest.Agent, target manifest.Target) *env.Env {
	e := env.New()
	e.Merge(a.Env) // the manifest's non-secret settings; everything below wins over them
	e.Set("AGENT_HOME", home)
	e.Set("NEEDLETAIL_API_PORT", strconv.Itoa(port))
	e.Set("SWARM_AGENT", a.ID)
	e.Set("SWARM_UNIT", a.Unit)
	e.Set("SWARM_SHARED_DIR", shared.Root)
	if onCore(a, target) {
		e.Set("SWARM_CORE_URL", manifest.Core.URL)
	}
	for _, s := range a.State {
		e.Set(s.Env, home+"/"+n.layout.StateRoot+"/"+s.Name)
	}
	e.Merge(slackEnv(a, target))
	return e
}

// slackEnv is the Slack channel's settings (tokens come from secrets), as Hermes names them.
func slackEnv(a *manifest.Agent, target manifest.Target) *env.Env {
	e := env.New()
	for _, c := range a.Channels {
		if c.Kind != "slack" {
			continue
		}
		s := c.SlackSettings
		if target == manifest.Local && c.Local != nil {
			s = overlay(s, *c.Local)
		}
		if s.AllowAllUsers != nil && *s.AllowAllUsers {
			e.Set("SLACK_ALLOW_ALL_USERS", "true")
		}
		if len(s.AllowedUsers) > 0 {
			e.Set("SLACK_ALLOWED_USERS", strings.Join(s.AllowedUsers, ","))
		}
		if s.HomeChannel != "" {
			e.Set("SLACK_HOME_CHANNEL", s.HomeChannel)
		}
		if s.HomeChannelName != "" {
			e.Set("SLACK_HOME_CHANNEL_NAME", s.HomeChannelName)
		}
		bots := s.AllowBots
		if bots == "" {
			bots = "mentions"
		}
		e.Set("SLACK_ALLOW_BOTS", bots)
	}
	return e
}

// overlay is base with the fields set locally replacing it.
func overlay(base, local manifest.SlackSettings) manifest.SlackSettings {
	if local.AllowedUsers != nil {
		base.AllowedUsers = local.AllowedUsers
	}
	if local.AllowAllUsers != nil {
		base.AllowAllUsers = local.AllowAllUsers
	}
	if local.HomeChannel != "" {
		base.HomeChannel = local.HomeChannel
	}
	if local.HomeChannelName != "" {
		base.HomeChannelName = local.HomeChannelName
	}
	if local.AllowBots != "" {
		base.AllowBots = local.AllowBots
	}
	return base
}
