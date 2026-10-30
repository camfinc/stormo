package link

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A service capability's worker is a long-lived container: booted on its first job, kept awake
// while jobs come, put to sleep (stopped and removed) after its idle minutes. Its cache volume
// (/cache: models, browsers) outlives the sleep, so the next boot is quick. Services runs them on
// one host (the core's, or a runner's) and calls them over HTTP on loopback.
//
// The worker's contract (docs/capabilities.md): it listens on $PORT, answers 200 on its health path
// once ready, and takes POST /run {job, agent, input, files, out} with the job's answer as JSON
// (non-2xx: the job failed, the body says why). files are the job's input paths under /jobs and out
// the directory it writes files to.

// ServiceSpec is how a service runs.
type ServiceSpec struct {
	Name    string // the capability
	Image   string
	Command []string
	Port    int
	Health  string
	// Env holds plain settings and secret values: handed over through docker's environment, never argv.
	Env      map[string]string
	MemoryMB int
	ShmMB    int
	CPUs     float64
	// JobsDir is the host directory mounted at /jobs ("": none).
	JobsDir string
	// Cache is the volume mounted at /cache (default: the container's name + "-cache").
	Cache string
	Idle  time.Duration
	Boot  time.Duration
}

// ServiceState is how a service is now.
type ServiceState struct {
	State    string `json:"state"` // asleep | booting | awake
	Since    int64  `json:"since,omitempty"`
	LastUsed int64  `json:"lastUsed,omitempty"`
	Running  int    `json:"running"` // jobs in it now
	Error    string `json:"error,omitempty"`
}

// Services runs one host's service containers.
type Services struct {
	// Scope names and labels the containers (stormo-<scope>-cap-<name>), so a restarted core or
	// runner finds the ones it left behind.
	Scope string
	// Docker runs docker with extra environment and gives its stdout (tests replace it).
	Docker func(ctx context.Context, env []string, args ...string) ([]byte, error)
	HTTP   *http.Client
	Now    func() time.Time
	Logf   func(string, ...any)

	mu sync.Mutex
	m  map[string]*service
}

type service struct {
	boot    sync.Mutex // one boot at a time
	hash    string
	state   string
	url     string
	since   time.Time
	last    time.Time
	running int
	err     string
	idle    time.Duration
}

// NewServices makes a manager for scope.
func NewServices(scope string) *Services {
	return &Services{Scope: scope, Docker: dockerRun, HTTP: &http.Client{}, Now: time.Now, Logf: func(string, ...any) {}, m: map[string]*service{}}
}

func dockerRun(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limited{&out, MaxOutput}, &limited{&errb, 16 << 10}
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

var unsafeName = regexp.MustCompile(`[^a-z0-9-]+`)

// Container is the container name of a service.
func (s *Services) Container(name string) string {
	return "stormo-" + unsafeName.ReplaceAllString(s.Scope, "-") + "-cap-" + unsafeName.ReplaceAllString(name, "-")
}

func (s *Services) get(name string) *service {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.m[name]
	if v == nil {
		v = &service{state: "asleep"}
		s.m[name] = v
	}
	return v
}

func specHash(sp ServiceSpec) string {
	b, _ := json.Marshal(sp)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// Ensure boots the service unless it is awake with this spec, and gives its base URL.
func (s *Services) Ensure(ctx context.Context, sp ServiceSpec) (string, error) {
	v := s.get(sp.Name)
	v.boot.Lock()
	defer v.boot.Unlock()
	hash := specHash(sp)
	s.mu.Lock()
	awake, same, busy := v.state == "awake", v.hash == hash, v.running
	url := v.url
	v.last = s.Now()
	v.idle = sp.Idle
	s.mu.Unlock()
	if awake && same && s.alive(ctx, sp.Name) {
		return url, nil
	}
	if awake && !same && busy > 0 {
		return url, nil // changed while jobs run in it: those finish first, the next idle boot takes the change
	}
	return s.boot(ctx, sp, v, hash)
}

func (s *Services) alive(ctx context.Context, name string) bool {
	out, err := s.Docker(ctx, nil, "inspect", "-f", "{{.State.Running}}", s.Container(name))
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func (s *Services) set(v *service, state, url, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.state != state {
		v.since = s.Now()
	}
	v.state, v.url, v.err = state, url, msg
}

func (s *Services) boot(ctx context.Context, sp ServiceSpec, v *service, hash string) (string, error) {
	name := s.Container(sp.Name)
	s.set(v, "booting", "", "")
	_, _ = s.Docker(ctx, nil, "rm", "-f", name)
	args := []string{"run", "-d", "--name", name, "--init",
		"--label", "stormo.scope=" + s.Scope, "--label", "stormo.capability=" + sp.Name,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--network", "bridge",
		"-p", "127.0.0.1::" + strconv.Itoa(sp.Port), "-e", "PORT=" + strconv.Itoa(sp.Port),
		"-v", cmp.Or(sp.Cache, name+"-cache") + ":/cache"}
	if uid := os.Getuid(); uid > 0 {
		// Files it writes to /jobs belong to whoever runs the core; images run as any user.
		args = append(args, "--user", fmt.Sprintf("%d:%d", uid, os.Getgid()))
	}
	var env []string
	keys := make([]string, 0, len(sp.Env))
	for k := range sp.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k)
		env = append(env, k+"="+sp.Env[k])
	}
	if sp.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", sp.MemoryMB))
	}
	if sp.ShmMB > 0 {
		args = append(args, "--shm-size", fmt.Sprintf("%dm", sp.ShmMB))
	}
	if sp.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(sp.CPUs, 'f', -1, 64))
	}
	if sp.JobsDir != "" {
		args = append(args, "-v", sp.JobsDir+":/jobs")
	}
	args = append(append(args, sp.Image), sp.Command...)
	fail := func(err error) (string, error) {
		if logs, _ := s.Docker(context.Background(), nil, "logs", "--tail", "20", name); len(logs) > 0 {
			err = fmt.Errorf("%w; its last lines: %s", err, clipTail(string(logs), 1500))
		}
		_, _ = s.Docker(context.Background(), nil, "rm", "-f", name)
		s.set(v, "asleep", "", err.Error())
		s.Logf("capabilities: %s did not boot: %v", sp.Name, err)
		return "", err
	}
	if _, err := s.Docker(ctx, env, args...); err != nil {
		return fail(err)
	}
	out, err := s.Docker(ctx, nil, "port", name, strconv.Itoa(sp.Port)+"/tcp")
	if err != nil {
		return fail(err)
	}
	addr := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if addr == "" {
		return fail(errors.New("docker published no port"))
	}
	addr = strings.Replace(addr, "0.0.0.0:", "127.0.0.1:", 1)
	url := "http://" + addr
	deadline := s.Now().Add(sp.Boot)
	for i := 0; ; i++ {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if s.Now().After(deadline) {
			return fail(fmt.Errorf("not ready within %s", sp.Boot))
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+sp.Health, nil)
		if res, err := s.HTTP.Do(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		if i%6 == 5 && !s.alive(ctx, sp.Name) {
			return fail(errors.New("it stopped while booting"))
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	s.mu.Lock()
	v.hash, v.last = hash, s.Now()
	s.mu.Unlock()
	s.set(v, "awake", url, "")
	s.Logf("capabilities: %s awake at %s", sp.Name, url)
	return url, nil
}

func clipTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// Call runs one job in the service (booting it first when asleep) and gives its answer.
func (s *Services) Call(ctx context.Context, sp ServiceSpec, body []byte) ([]byte, error) {
	v := s.get(sp.Name)
	// Ensure marks it used and holds the boot lock while booting, which keeps the reaper off it.
	url, err := s.Ensure(ctx, sp)
	if err != nil {
		return nil, fmt.Errorf("the worker did not boot: %w", err)
	}
	s.mu.Lock()
	v.running++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		v.running--
		v.last = s.Now()
		s.mu.Unlock()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.HTTP.Do(req)
	if err != nil {
		if ctx.Err() == nil && !s.alive(context.Background(), sp.Name) {
			s.set(v, "asleep", "", "it stopped during a job")
		}
		return nil, err
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, MaxOutput+1))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("the worker answered %d: %s", res.StatusCode, clipTail(string(out), 1500))
	}
	return out, nil
}

// Sleep stops a service now; refused while jobs run in it.
func (s *Services) Sleep(name string) error {
	v := s.get(name)
	s.mu.Lock()
	running := v.running
	s.mu.Unlock()
	if running > 0 {
		return fmt.Errorf("%s has %d job(s) running: it sleeps once idle", name, running)
	}
	v.boot.Lock()
	defer v.boot.Unlock()
	return s.stop(name, v)
}

func (s *Services) stop(name string, v *service) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.Docker(ctx, nil, "stop", "-t", "10", s.Container(name))
	_, err := s.Docker(ctx, nil, "rm", "-f", s.Container(name))
	s.set(v, "asleep", "", "")
	s.Logf("capabilities: %s asleep", name)
	return err
}

// Reap puts to sleep the services idle for their idle minutes.
func (s *Services) Reap() {
	now := s.Now()
	s.mu.Lock()
	var idle []string
	for name, v := range s.m {
		if v.state == "awake" && v.running == 0 && v.idle > 0 && now.Sub(v.last) >= v.idle {
			idle = append(idle, name)
		}
	}
	s.mu.Unlock()
	for _, name := range idle {
		v := s.get(name)
		if !v.boot.TryLock() {
			continue // booting: it is about to be used
		}
		s.mu.Lock()
		still := v.state == "awake" && v.running == 0 && now.Sub(v.last) >= v.idle
		s.mu.Unlock()
		if still {
			_ = s.stop(name, v)
		}
		v.boot.Unlock()
	}
}

// Clean removes containers of this scope left by an earlier run (a core or runner that stopped
// without putting them to sleep).
func (s *Services) Clean() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := s.Docker(ctx, nil, "ps", "-aq", "--filter", "label=stormo.scope="+s.Scope)
	if err != nil {
		return
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		_, _ = s.Docker(ctx, nil, append([]string{"rm", "-f"}, ids...)...)
	}
}

// StopAll puts every service to sleep (the core or runner is stopping).
func (s *Services) StopAll() {
	s.mu.Lock()
	names := []string{}
	for name, v := range s.m {
		if v.state != "asleep" {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.stop(n, s.get(n))
		}()
	}
	wg.Wait()
}

// Run reaps idle services every interval until stop, then puts them all to sleep.
func (s *Services) Run(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			s.StopAll()
			return
		case <-t.C:
			s.Reap()
		}
	}
}

// States are the services this manager has seen.
func (s *Services) States() map[string]ServiceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]ServiceState{}
	for name, v := range s.m {
		st := ServiceState{State: v.state, Running: v.running, Error: v.err}
		if !v.since.IsZero() {
			st.Since = v.since.UnixMilli()
		}
		if !v.last.IsZero() {
			st.LastUsed = v.last.UnixMilli()
		}
		out[name] = st
	}
	return out
}

// State is one service's state (asleep when never booted).
func (s *Services) State(name string) ServiceState {
	if st, ok := s.States()[name]; ok {
		return st
	}
	return ServiceState{State: "asleep"}
}
