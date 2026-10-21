// Package local runs agents on this machine: one Docker Compose project per agent (OrbStack,
// Docker Desktop or Docker Engine on Linux), the twin of the ECS task: same three containers, same
// dependency and stop order (rehydrate → nap → agent). Differences from ECS, on purpose:
//   - the sidecars run the engine's sidecar image with the instance's files (stormo.yaml, units,
//     this agent, its baseline) bind-mounted read-only, so a manifest or skill edit needs no image
//     build, only `stormo restart`;
//   - naps go to the agent's folder, agents/<id>/data/store (loop.LocalStore, also the dream's
//     default local store), mounted into the sidecars as /store/<id>;
//   - the shared space is workdir/<layer> (shared.LocalDir; moved from .swarm/shared once);
//   - secrets come from agents/<id>/data/agent.env, rendered from the secrets with the `local`
//     overlay; the sidecars never see it (nor anything else in data/);
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
	"runtime"
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
	// StoreDir is the agent's own local store (loop.LocalStoreDir).
	StoreDir  string
	SharedDir string
	// CoreURL is where the agent's sidecar dials its core (manifest.CoreFor(inst, Local).URL).
	CoreURL string
	// LinkEnvFile holds the sidecar's link secrets (secrets.WriteLinkEnv); "" leaves the link off.
	LinkEnvFile string
	// Placement this host runs agents for ("" is local).
	Placement string
}

func ProjectName(a *manifest.Agent) string { return ProjectPrefix + a.ID }

// ProjectPrefix starts every agent's compose project name.
const ProjectPrefix = "swarm-"

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
	Tmpfs       []string          `yaml:"tmpfs,omitempty"`
	SecurityOpt []string          `yaml:"security_opt,omitempty"`
	CapDrop     []string          `yaml:"cap_drop,omitempty"`
}

// hardening applies to every container of an agent. Engine images may boot as root and drop to
// their runtime user themselves (an init that chowns the home, then setuid), so the user and the
// capabilities that needs (CHOWN, DAC_OVERRIDE, FOWNER, SETUID, SETGID, KILL) stay; nothing in
// the task needs raw sockets, device nodes, chroot, file capabilities or ports below 1024, and
// no process may gain privileges through a setuid binary.
var (
	securityOpt = []string{"no-new-privileges:true"}
	capDrop     = []string{"NET_RAW", "MKNOD", "SYS_CHROOT", "SETFCAP", "AUDIT_WRITE", "NET_BIND_SERVICE"}
)

// Spec is a compose file.
type Spec struct {
	Name     string             `yaml:"name"`
	Services map[string]service `yaml:"services"`
	Volumes  map[string]any     `yaml:"volumes"`
}

// Render builds the compose project for an agent; sidecarImage is the engine's sidecar image.
func Render(a *manifest.Agent, eng engine.Engine, p Paths, port, napInterval int, sidecarImage string) Spec {
	ro := func(rel string) string { return filepath.ToSlash(filepath.Join(p.Root, rel)) + ":/app/" + rel + ":ro" }
	sidecar := service{
		Image:      sidecarImage,
		WorkingDir: "/app",
		// Exactly what the ECS instance layer carries: stormo.yaml, units, this agent and its baseline.
		Volumes: []string{
			ro(instance.File), ro("units"), ro("agents/" + a.ID),
			filepath.ToSlash(p.BaselineDir) + ":/app/dist/" + a.ID + "/baseline:ro",
			"home:/data", filepath.ToSlash(p.StoreDir) + ":/store/" + a.ID,
		},
		Environment: map[string]string{"SWARM_AGENT": a.ID, "SWARM_STORE": "/store", "SWARM_HOME": "/data"},
		// The agent's data folder (its secret values, its store) stays out of the sidecars: they get
		// the store at /store/<id> and nothing else of it.
		Tmpfs:       []string{"/app/agents/" + a.ID + "/data"},
		SecurityOpt: securityOpt,
		CapDrop:     capDrop,
	}
	rehydrate := sidecar
	rehydrate.Command = []string{"rehydrate"}
	rehydrate.Restart = "no"
	nap := sidecar
	nap.Command = []string{"nap", "--loop"}
	nap.Environment = map[string]string{"SWARM_AGENT": a.ID, "SWARM_STORE": "/store", "SWARM_HOME": "/data", "SWARM_NAP_INTERVAL": fmt.Sprint(napInterval)}
	nap.DependsOn = map[string]any{"rehydrate": map[string]string{"condition": "service_completed_successfully"}}
	nap.StopGrace = "100s"
	// Like the agent: a Docker restart (daemon, laptop sleep) brings both back, not the agent alone.
	nap.Restart = "unless-stopped"

	volumes := []string{"home:" + eng.Layout().Home}
	for _, l := range shared.Layers(a) {
		volumes = append(volumes, filepath.ToSlash(filepath.Join(p.SharedDir, l.Layer))+":"+l.Path)
	}
	environment := eng.Env(a, manifest.Local).Map()
	// The agent's own calls to its core (the workdir helper) go through the sidecar's relay.
	if _, ok := environment["SWARM_CORE_URL"]; ok {
		environment["SWARM_CORE_URL"] = manifest.RelayURL(manifest.Local)
	}
	volumesTop := map[string]any{"home": map[string]any{}}
	if p.LinkEnvFile != "" && p.CoreURL != "" {
		// The sidecar is the agent's link to the core (pkg/link): its relay, the live wire, a spool
		// of its own (never under the home, which naps snapshot).
		nap.EnvFile = []string{p.LinkEnvFile}
		nap.Environment["SWARM_CORE_URL"] = p.CoreURL
		nap.Environment["SWARM_AGENT_API"] = fmt.Sprintf("http://agent:%d", eng.Port())
		nap.Environment["SWARM_PLACEMENT"] = string(manifest.Local)
		if p.Placement != "" {
			nap.Environment["SWARM_PLACEMENT"] = p.Placement
		}
		nap.Environment["SWARM_LINK_DIR"] = "/link"
		nap.Volumes = append(append([]string{}, nap.Volumes...), "link:/link")
		nap.ExtraHosts = []string{"host.docker.internal:host-gateway"}
		volumesTop["link"] = map[string]any{}
	}
	agent := service{
		Image:       eng.Image(a),
		Command:     eng.Command(a),
		Environment: environment,
		EnvFile:     []string{filepath.ToSlash(p.EnvFile)},
		Volumes:     volumes,
		Ports:       []string{fmt.Sprintf("127.0.0.1:%d:%d", port, eng.Port())},
		Healthcheck: map[string]any{"test": eng.HealthCheck(), "interval": "30s", "timeout": "5s", "retries": 3, "start_period": "120s"},
		// Compose stops dependents first: the agent stops before nap takes the final snapshot.
		DependsOn: map[string]any{
			"rehydrate": map[string]string{"condition": "service_completed_successfully"},
			"nap":       map[string]string{"condition": "service_started"},
		},
		StopGrace:   "60s",
		Restart:     "unless-stopped",
		SecurityOpt: securityOpt,
		CapDrop:     capDrop,
	}
	if a.Engine.Local != nil && a.Engine.Local.Via == "core" && runtime.GOOS != "windows" {
		// Windows Docker Desktop supplies host.docker.internal itself. Do not replace
		// it with the Linux VM gateway. Preserve existing Unix host-gateway behavior.
		agent.ExtraHosts = []string{"host.docker.internal:host-gateway"}
	}
	return Spec{Name: ProjectName(a), Services: map[string]service{"rehydrate": rehydrate, "nap": nap, "agent": agent}, Volumes: volumesTop}
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

// portFree checks a candidate agent port (tests replace it).
var portFree = instance.PortFree

// ReleasePort frees a deleted agent's port in .swarm/ports.json.
func ReleasePort(root, agentID string) error {
	portMu.Lock()
	defer portMu.Unlock()
	path := filepath.Join(root, ".swarm", "ports.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	ports := map[string]int{}
	if err := json.Unmarshal(b, &ports); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, ok := ports[agentID]; !ok {
		return nil
	}
	delete(ports, agentID)
	return writePorts(path, ports)
}

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
	// Other instances on this host keep their own ports.json: never take a port one of them holds,
	// nor one something else listens on now.
	used := instance.OtherAgentPorts(root)
	for _, p := range ports {
		used[p] = true
	}
	port := PortBase
	for used[port] || !portFree(port) {
		port++
	}
	ports[agentID] = port
	return port, writePorts(path, ports)
}

func writePorts(path string, ports map[string]int) error {
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
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ports-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
