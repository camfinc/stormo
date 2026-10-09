package manifest

import (
	"fmt"
	"slices"
	"strings"

	"github.com/camfinc/stormo/pkg/instance"
)

// CheckConnections checks that the agent's model routes through connections the instance has
// (stormo.yaml connections:): model.provider an API one whose key the agent declares, model.local
// a ChatGPT sign-in on the core; a schedule's provider likewise.
func CheckConnections(a *Agent, inst *instance.Instance) error {
	where := fmt.Sprintf("agents/%s/agent.yaml", a.ID)
	names := func(api bool) string {
		out := []string{}
		for _, c := range inst.Connections {
			if c.API() == api {
				out = append(out, c.Name)
			}
		}
		return strings.Join(out, ", ")
	}
	api := func(name, field string) error {
		c, ok := inst.Connection(name)
		if !ok || !c.API() {
			return &Error{fmt.Sprintf("%s: %s %q is not an API connection of this instance (%s; stormo.yaml connections:)", where, field, name, names(true))}
		}
		if !slices.Contains(a.Secrets, c.Key) && !slices.ContainsFunc(a.OptionalSecrets, func(o OptionalSecret) bool { return o.Name == c.Key }) {
			return &Error{fmt.Sprintf("%s: %s %q is keyed by %s; add it under secrets:", where, field, name, c.Key)}
		}
		return nil
	}
	if err := api(a.Engine.Provider, "model.provider"); err != nil {
		return err
	}
	if l := a.Engine.Local; l != nil {
		name := l.ConnectionName()
		if c, ok := inst.Connection(name); !ok || c.API() {
			return &Error{fmt.Sprintf("%s: model.local.connection %q is not a ChatGPT connection of this instance (%s)", where, name, names(false))}
		}
	}
	for _, s := range a.Schedules {
		if s.Provider != "" {
			if err := api(s.Provider, fmt.Sprintf("schedule %q provider", s.ID)); err != nil {
				return err
			}
		}
	}
	return nil
}
