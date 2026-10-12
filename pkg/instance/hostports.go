package instance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Several instances can run on one host, each with its own core. Every instance records what it
// holds in a claim file under the host's state directory (~/.stormo/instances/<id>.json), so a new
// instance never takes a port another one uses, running or not. The core's port, once picked, is
// kept in the instance (.swarm/core/port): agents are built against it.

// DefaultCorePort is the first port a core takes.
const DefaultCorePort = 18600

// HostDir is the host's Stormo state (STORMO_HOME, else ~/.stormo).
func HostDir() string {
	if d := os.Getenv("STORMO_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "stormo")
	}
	return filepath.Join(home, ".stormo")
}

// Claim is one instance's entry in the host registry.
type Claim struct {
	Root     string `json:"root"`
	Name     string `json:"name"`
	CorePort int    `json:"corePort"`
}

func claimsDir() string { return filepath.Join(HostDir(), "instances") }

// ClaimID is the claim file's name for an instance root.
func ClaimID(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	h := sha256.Sum256([]byte(filepath.Clean(abs)))
	return hex.EncodeToString(h[:])[:16]
}

// Claims are the other instances on this host that still exist (stale entries are dropped).
func Claims() []Claim {
	entries, _ := os.ReadDir(claimsDir())
	out := []Claim{}
	for _, e := range entries {
		p := filepath.Join(claimsDir(), e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c Claim
		if json.Unmarshal(b, &c) != nil || c.Root == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c.Root, "stormo.yaml")); err != nil {
			_ = os.Remove(p)
			continue
		}
		out = append(out, c)
	}
	return out
}

func writeClaim(c Claim) error {
	if err := os.MkdirAll(claimsDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	p := filepath.Join(claimsDir(), ClaimID(c.Root)+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func corePortFile(root string) string { return filepath.Join(root, ".swarm", "core", "port") }

var corePortMu sync.Mutex

// CorePort is the port this instance's core listens on: SWARM_CORE_PORT, else stormo.yaml
// core.port, else the one it picked before, else the first free port from 18600 that no other
// instance claims (then kept). The pick is recorded in the host registry either way.
func (i *Instance) CorePort() int {
	if n, err := strconv.Atoi(os.Getenv("SWARM_CORE_PORT")); err == nil && n > 0 {
		return n
	}
	corePortMu.Lock()
	defer corePortMu.Unlock()
	port := i.CorePortSetting
	if port == 0 {
		if b, err := os.ReadFile(corePortFile(i.Root)); err == nil {
			port, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if port == 0 {
		port = i.pickCorePort()
		if err := os.MkdirAll(filepath.Dir(corePortFile(i.Root)), 0o700); err == nil {
			_ = os.WriteFile(corePortFile(i.Root), []byte(strconv.Itoa(port)+"\n"), 0o600)
		}
	}
	_ = writeClaim(Claim{Root: i.Root, Name: i.Name, CorePort: port})
	return port
}

// pickCorePort: the first port from DefaultCorePort that another instance neither claims nor
// serves a core on, and that is free now. Ports go up in steps of one.
func (i *Instance) pickCorePort() int {
	mine := ClaimID(i.Root)
	taken := map[int]bool{}
	for _, c := range Claims() {
		if ClaimID(c.Root) != mine {
			taken[c.CorePort] = true
		}
	}
	for p := DefaultCorePort; p < DefaultCorePort+500; p++ {
		if taken[p] {
			continue
		}
		if root, ok := coreRootOn(p); ok {
			if ClaimID(root) == mine {
				return p
			}
			continue
		}
		if free(p) {
			return p
		}
	}
	return DefaultCorePort
}

// coreRootOn asks a core on the port which instance it serves (older cores without a claim).
func coreRootOn(port int) (string, bool) {
	c := http.Client{Timeout: 700 * time.Millisecond}
	r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/core", port))
	if err != nil {
		return "", false
	}
	defer r.Body.Close()
	var v struct {
		Service  string `json:"service"`
		Instance struct {
			Root string `json:"root"`
		} `json:"instance"`
	}
	if json.NewDecoder(r.Body).Decode(&v) != nil || v.Service != "swarm-core" {
		return "", true // something else answers there: not ours to take
	}
	return v.Instance.Root, true
}

func free(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// OtherAgentPorts are the ports other instances on this host hold: their cores and their agents
// (.swarm/ports.json).
func OtherAgentPorts(root string) map[int]bool {
	mine := ClaimID(root)
	out := map[int]bool{}
	for _, c := range Claims() {
		if ClaimID(c.Root) == mine {
			continue
		}
		out[c.CorePort] = true
		b, err := os.ReadFile(filepath.Join(c.Root, ".swarm", "ports.json"))
		if err != nil {
			continue
		}
		ports := map[string]int{}
		if json.Unmarshal(b, &ports) == nil {
			for _, p := range ports {
				out[p] = true
			}
		}
	}
	return out
}

// PortFree reports whether nothing listens on the loopback port now.
func PortFree(port int) bool { return free(port) }
