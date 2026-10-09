// Package local runs agents on this machine: one Docker Compose project per agent (OrbStack,
// Docker Desktop or Docker Engine on Linux), the twin of the ECS task: same three containers, same
// dependency and stop order (rehydrate → nap → agent). Differences from ECS, on purpose:
//   - the sidecars run the engine's sidecar image with the instance's files (stormo.yaml, units,
//     this agent, its baseline) bind-mounted read-only, so a manifest or skill edit needs no image
//     build, only `stormo restart`;
//   - naps go to .swarm/store (a directory store), also the dream's default local store;
//   - the shared space is workdir/<layer> (shared.LocalDir; moved from .swarm/shared once);
//   - secrets come from .swarm/env/<agent>.env, rendered from secrets.local.yaml with the `local`
//     overlay; the sidecars never see it;
//   - the baseline is the `local` build (.swarm/local/<agent>/baseline: `engine.local` model first);
//   - with `engine.local.via: core` the agent calls the core's model gateway on the host.
package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/shared"
)

// PortBase is the first host port handed to an agent's API server.
const PortBase = 18642

// Paths for one agent's local run.
type Paths struct {
	Root        string // the instance
	BaselineDir string // the local build of the agent's baseline
	EnvFile     string
	StoreDir    string
	SharedDir   string
}

func ProjectName(a *manifest.Agent) string { return "swarm-" + a.ID }

func ComposePath(root string, a *manifest.Agent) string {
	return filepath.Join(root, ".swarm", "compose", a.ID+".yaml")
}

type service struct {
	Image       string            `yaml:"image"`
	WorkingDir  string            `yaml:"working_dir,omitempty"`
	Command     []string          `yaml:"command,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Environment map[string]string `yaml:"environment,omitempty"`
	EnvFile     []string          `yaml:"env_file,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Healthcheck map[string]any    `yaml:"healthcheck,omitempty"`
	DependsOn   map[string]any    `yaml:"depends_on,omitempty"`
	StopGrace   string            `yaml:"stop_grace_period,omitempty"`
	Restart     string            `yaml:"restart,omitempty"`
	ExtraHosts  []string          `yaml:"extra_hosts,omitempty"`
}

// Spec is a compose file.
type Spec struct {
	Name     string             `yaml:"name"`
	Services map[string]service `yaml:"services"`
	Volumes  map[string]any     `yaml:"volumes"`
}

// Render builds the compose project for an agent; sidecarImage is the engine's sidecar image.
func Render(a *manifest.Agent, eng engine.Engine, p Paths, port, napInterval int, sidecarImage string) Spec {
	ro := func(rel string) string { return filepath.Join(p.Root, rel) + ":/app/" + rel + ":ro" }
	sidecar := service{
		Image:      sidecarImage,
		WorkingDir: "/app",
		// Exactly what the ECS instance layer carries: stormo.yaml, units, this agent and its baseline.
		Volumes: []string{
			ro(instance.File), ro("units"), ro("agents/" + a.ID),
			p.BaselineDir + ":/app/dist/" + a.ID + "/baseline:ro",
			"home:/data", p.StoreDir + ":/store",
		},
		Environment: map[string]string{"SWARM_AGENT": a.ID, "SWARM_STORE": "/store", "SWARM_HOME": "/data"},
	}
	rehydrate := sidecar
	rehydrate.Command = []string{"rehydrate"}
	rehydrate.Restart = "no"
	nap := sidecar
	nap.Command = []string{"nap", "--loop"}
	nap.Environment = map[string]string{"SWARM_AGENT": a.ID, "SWARM_STORE": "/store", "SWARM_HOME": "/data", "SWARM_NAP_INTERVAL": fmt.Sprint(napInterval)}
	nap.DependsOn = map[string]any{"rehydrate": map[string]string{"condition": "service_completed_successfully"}}
	nap.StopGrace = "100s"

	volumes := []string{"home:" + eng.Layout().Home}
	for _, l := range shared.Layers(a) {
		volumes = append(volumes, filepath.Join(p.SharedDir, l.Layer)+":"+l.Path)
	}
	agent := service{
		Image:       eng.Image(a),
		Command:     eng.Command(a),
		Environment: eng.Env(a, manifest.Local).Map(),
		EnvFile:     []string{p.EnvFile},
		Volumes:     volumes,
		Ports:       []string{fmt.Sprintf("127.0.0.1:%d:%d", port, eng.Port())},
		Healthcheck: map[string]any{"test": eng.HealthCheck(), "interval": "30s", "timeout": "5s", "retries": 3, "start_period": "120s"},
		// Compose stops dependents first: the agent stops before nap takes the final snapshot.
		DependsOn: map[string]any{
			"rehydrate": map[string]string{"condition": "service_completed_successfully"},
			"nap":       map[string]string{"condition": "service_started"},
		},
		StopGrace: "60s",
		Restart:   "unless-stopped",
	}
	if a.Engine.Local != nil && a.Engine.Local.Via == "core" {
		// OrbStack / Docker Desktop resolve host.docker.internal already; plain Docker Engine needs this.
		agent.ExtraHosts = []string{"host.docker.internal:host-gateway"}
	}
	return Spec{Name: ProjectName(a), Services: map[string]service{"rehydrate": rehydrate, "nap": nap, "agent": agent}, Volumes: map[string]any{"home": map[string]any{}}}
}

// ComposeCommand is `docker compose` (v2 plugin) with a `docker-compose` fallback.
func ComposeCommand() ([]string, error) {
	if exec.Command("docker", "compose", "version").Run() == nil {
		return []string{"docker", "compose"}, nil
	}
	if _, err := exec.LookPath("docker-compose"); err == nil {
		return []string{"docker-compose"}, nil
	}
	return nil, errors.New("docker compose not found: install Docker with compose v2 (OrbStack, Docker Desktop or Docker Engine)")
}

// portMu serialises Port: the core asks for every agent's port at once, and two allocations
// reading the file together would hand out one port twice and lose an entry.
var portMu sync.Mutex

// Port is the agent's stable host port, kept in .swarm/ports.json: an agent keeps its port once it
// has one, and a new agent takes the lowest free port from PortBase. The file is replaced
// atomically, so a reader never sees half of it.
func Port(root, agentID string) (int, error) {
	portMu.Lock()
	defer portMu.Unlock()
	path := filepath.Join(root, ".swarm", "ports.json")
	ports := map[string]int{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &ports); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
	}
	if p, ok := ports[agentID]; ok && p != 0 {
		return p, nil
	}
	used := map[int]bool{}
	for _, p := range ports {
		used[p] = true
	}
	port := PortBase
	for used[port] {
		port++
	}
	ports[agentID] = port
	ids := make([]string, 0, len(ports))
	for id := range ports {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ports[ids[i]] < ports[ids[j]] })
	body := "{"
	for i, id := range ids {
		if i > 0 {
			body += ","
		}
		k, _ := json.Marshal(id)
		body += fmt.Sprintf("\n  %s: %d", k, ports[id])
	}
	body += "\n}\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ports-*.json")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	return port, os.Rename(tmp.Name(), path)
}
