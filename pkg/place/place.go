// Package place knows where an agent is running, and enforces the one-place rule. An agent runs
// locally or on ECS, never both on the same Slack app: Socket Mode would split its events between
// the two. Every start (CLI, handoff, or the core driving the local lifecycle) passes through
// GuardStart.
package place

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/camfinc/stormo/pkg/aws"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/instance"
)

// Where an agent runs.
type Where string

const (
	Local  Where = "local"
	Remote Where = "remote"
)

// SideState is what one side knows about the agent.
type SideState string

const (
	Running SideState = "running"
	Stopped SideState = "stopped"
	Missing SideState = "missing"
	Unknown SideState = "unknown"
)

// OtherSide is the place an agent is not being started on.
func OtherSide(w Where) Where {
	if w == Local {
		return Remote
	}
	return Local
}

// ServiceName is the agent's ECS service (and task family).
func ServiceName(inst *instance.Instance, id string) string {
	return inst.Names.Resource + "-" + id
}

// EcsService is the part of describe-services the swarm reads.
type EcsService struct {
	Status       string `json:"status"`
	DesiredCount int    `json:"desiredCount"`
	RunningCount int    `json:"runningCount"`
	PendingCount int    `json:"pendingCount"`
	Deployments  []struct {
		RolloutState string `json:"rolloutState,omitempty"`
		Status       string `json:"status,omitempty"`
	} `json:"deployments,omitempty"`
}

// RemoteService describes the agent's ECS service; nil when it does not exist (or is INACTIVE).
func RemoteService(inst *instance.Instance, id string, cli aws.CLI, t deploy.Target) (*EcsService, error) {
	var r struct {
		Services []EcsService `json:"services"`
	}
	if err := aws.JSON(cli, []string{"ecs", "describe-services", "--region", t.Region, "--cluster", t.Cluster, "--services", ServiceName(inst, id)}, &r); err != nil {
		return nil, err
	}
	if len(r.Services) == 0 || r.Services[0].Status == "INACTIVE" {
		return nil, nil
	}
	return &r.Services[0], nil
}

// RemoteSide: running while any task is wanted or still alive (a stopping task still holds Slack).
func RemoteSide(inst *instance.Instance, id string, cli aws.CLI, t deploy.Target) SideState {
	s, err := RemoteService(inst, id, cli, t)
	switch {
	case err != nil:
		return Unknown // no aws CLI or credentials, cluster not created, network down
	case s == nil:
		return Missing
	case s.DesiredCount > 0 || s.RunningCount > 0 || s.PendingCount > 0:
		return Running
	}
	return Stopped
}

// LocalState is the agent's container state.
type LocalState struct {
	State  string `json:"state"`
	Health string `json:"health,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type composeRow struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

func parseComposePs(out string) ([]composeRow, error) {
	t := strings.TrimSpace(out)
	if t == "" {
		return nil, nil
	}
	var rows []composeRow
	if strings.HasPrefix(t, "[") {
		return rows, json.Unmarshal([]byte(t), &rows)
	}
	for _, l := range strings.Split(t, "\n") {
		if l == "" {
			continue
		}
		var r composeRow
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// StateFromComposePs reads `compose ps --all --format json` output (pure).
func StateFromComposePs(out string) LocalState {
	rows, err := parseComposePs(out)
	if err != nil {
		return LocalState{State: "unknown", Detail: err.Error()}
	}
	var agent, nap *composeRow
	for i := range rows {
		switch rows[i].Service {
		case "agent":
			if agent == nil {
				agent = &rows[i]
			}
		case "nap":
			if nap == nil {
				nap = &rows[i]
			}
		}
	}
	if agent == nil {
		return LocalState{State: "stopped"}
	}
	s := LocalState{State: agent.State, Health: agent.Health}
	if agent.State == "running" {
		s.State = "running"
		if agent.Health == "starting" {
			s.State = "starting"
		}
	}
	if nap != nil && nap.State != "running" {
		s.Detail = "nap sidecar " + nap.State
	}
	return s
}

// ComposeCommand finds the compose CLI (`docker compose` or `docker-compose`).
type ComposeCommand func() ([]string, error)

// LocalStateOf runs `compose ps` for the agent's project.
func LocalStateOf(id string, compose ComposeCommand) LocalState {
	cmd, err := compose()
	if err != nil {
		return LocalState{State: "unknown", Detail: err.Error()}
	}
	argv := append(append([]string{}, cmd...), "-p", "swarm-"+id, "ps", "--all", "--format", "json")
	c := exec.Command(argv[0], argv[1:]...)
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return LocalState{State: "unknown", Detail: strings.SplitN(strings.TrimSpace(stderr.String()), "\n", 2)[0]}
	}
	return StateFromComposePs(string(out))
}

// LocalSide maps the container state to a side state.
func LocalSide(id string, compose ComposeCommand) SideState {
	switch LocalStateOf(id, compose).State {
	case "unknown":
		return Unknown
	case "running", "starting", "restarting":
		return Running
	}
	return Stopped
}

// Decision says whether a start may go ahead.
type Decision struct {
	OK     bool
	Reason string
}

// GuardStart: may id start on where? Only a shared Slack app makes two places conflict
// (sharedTokens: the local run would use the production bot/app tokens). A separate dev app can run
// alongside. allowProdToken vouches that ECS is down when it cannot be checked; it never overrides
// a known running side.
func GuardStart(id string, where Where, other SideState, sharedTokens, allowProdToken bool) Decision {
	if !sharedTokens {
		return Decision{OK: true}
	}
	there := OtherSide(where)
	if other == Running {
		return Decision{Reason: fmt.Sprintf("%s is running on %s with the same Slack app. Move it with `stormo handoff %s --to %s`.", id, there, id, where)}
	}
	if other == Unknown && !allowProdToken {
		return Decision{Reason: fmt.Sprintf("%s: cannot tell whether it is running on %s (no AWS access?), and both places use the same Slack app.\n", id, there) +
			fmt.Sprintf("Pass --allow-prod-token if you know it is stopped there, or give local runs a dev app under local.agents.%s.", id)}
	}
	return Decision{OK: true}
}
