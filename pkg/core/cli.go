package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/camfinc/stormo/pkg/core/llm"
	"github.com/camfinc/stormo/pkg/instance"
)

// Usage is the core section of the CLI usage text.
const Usage = `  core (the core service on this machine, docs/core.md):
  core up | down | status                start in the background / stop / show login, plan limit, usage
  core serve                             run in the foreground (logs to stdout)
  core login | logout                    Continue with ChatGPT (browser sign-in, ChatGPT plan usage) held only by the core`

func base() string { return fmt.Sprintf("http://127.0.0.1:%d", CorePort()) }

func pidPath(root string) string { return filepath.Join(CoreDir(root), "core.pid") }
func logPath(root string) string { return filepath.Join(CoreDir(root), "core.log") }

type healthBody struct {
	Login            string  `json:"login"`
	PlanLimitedUntil *string `json:"planLimitedUntil"`
}

func health() *healthBody {
	c := http.Client{Timeout: 1500 * time.Millisecond}
	r, err := c.Get(base() + "/health")
	if err != nil {
		return nil
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil
	}
	var h healthBody
	if json.NewDecoder(r.Body).Decode(&h) != nil {
		return nil
	}
	return &h
}

// readPid is the pid in the pid file, if that process is alive.
func readPid(root string) int {
	b, err := os.ReadFile(pidPath(root))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	if syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

func up(inst *instance.Instance) error {
	root := inst.Root
	if health() != nil {
		fmt.Printf("core already running on %s\n", base())
		return nil
	}
	if err := os.MkdirAll(CoreDir(root), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath(root), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "core", "serve")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "STORMO_INSTANCE="+root)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives this shell
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	if err := os.WriteFile(pidPath(root), []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		return err
	}
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if h := health(); h != nil {
			fmt.Printf("core up on %s (pid %d, log %s); login: %s\n", base(), pid, logPath(root), h.Login)
			if h.Login != string(llm.LoginOK) {
				fmt.Println("sign in with: stormo core login")
			}
			return nil
		}
	}
	return fmt.Errorf("core did not come up; see %s", logPath(root))
}

// down stops only the process named in the pid file `core up` wrote.
func down(inst *instance.Instance) error {
	root := inst.Root
	pid := readPid(root)
	if pid == 0 {
		_ = os.Remove(pidPath(root))
		if health() != nil {
			fmt.Printf("core answers on %s but was not started by `core up` (no pid file)\n", base())
		} else {
			fmt.Println("core is not running")
		}
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	_ = os.Remove(pidPath(root))
	fmt.Printf("core stopped (pid %d)\n", pid)
	return nil
}

func status(inst *instance.Instance) error {
	c := http.Client{Timeout: 1500 * time.Millisecond}
	r, err := c.Get(base() + "/api/gateway")
	if err != nil {
		state := llm.NewChatGPTAuth(llm.AuthOptions{Paths: llm.Paths(AuthDir(inst.Root))}).State()
		fmt.Printf("core: not running (%s); ChatGPT sign-in on disk: %s\n", base(), state)
		return nil
	}
	defer r.Body.Close()
	var s llm.Status
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		return err
	}
	fmt.Printf("core: running on %s\n", base())
	if s.Login == llm.LoginOK {
		account := ""
		if s.Account != nil {
			account = " (" + *s.Account + ")"
		}
		fmt.Printf("Using ChatGPT plan%s. Manage usage: %s\n", account, s.ManageUsageURL)
	} else {
		fmt.Printf("ChatGPT sign-in: %s (run: stormo core login)\n", s.Login)
	}
	if s.PlanLimitedUntil != nil {
		fmt.Printf("ChatGPT plan usage limit reached; agents use their fallback until %s. Manage usage: %s\n", *s.PlanLimitedUntil, s.ManageUsageURL)
	}
	ids := []string{}
	for _, m := range s.Models {
		ids = append(ids, m.ID)
	}
	fmt.Printf("llm: %d/%d in flight, %d queued; models %s\n", s.Inflight, s.Concurrency, s.Queued, strings.Join(ids, ", "))
	if len(s.Usage) == 0 {
		fmt.Println("usage: no calls yet")
		return nil
	}
	fmt.Println("agent         reqs   ok  429  err   in_tok  cached  out_tok  last")
	agents := make([]string, 0, len(s.Usage))
	for a := range s.Usage {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	for _, a := range agents {
		u := s.Usage[a]
		fmt.Printf("%-12s %5d %4d %4d %4d %8d %7d %8d  %s\n", a, u.Requests, u.OK, u.RateLimited, u.Errors, u.InputTokens, u.CachedTokens, u.OutputTokens, u.LastAt)
	}
	return nil
}

func serve(inst *instance.Instance) error {
	c, err := StartCore(CoreOptions{Inst: inst, Port: CorePort()})
	if err != nil {
		return err
	}
	fmt.Printf("%s swarm core listening on http://%s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), c.Addr)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	return c.Close()
}

// Command runs `stormo core <sub>`.
func Command(inst *instance.Instance, sub string, args []string) error {
	switch sub {
	case "serve":
		return serve(inst)
	case "up":
		return up(inst)
	case "down":
		return down(inst)
	case "", "status":
		return status(inst)
	case "login":
		port := llm.SIWC.DefaultLoginPort
		if n, err := strconv.Atoi(os.Getenv("SWARM_CORE_LOGIN_PORT")); err == nil && n > 0 {
			port = n
		}
		paths := llm.Paths(AuthDir(inst.Root))
		conn, err := llm.SignIn(context.Background(), llm.SignInOptions{Paths: paths, AppName: inst.Name, Port: &port, Print: func(l string) { fmt.Println(l) }})
		if err != nil {
			return err
		}
		email := ""
		if conn.Email != "" {
			email = " (" + conn.Email + ")"
		}
		fmt.Printf("\nYou're using your ChatGPT plan%s.\n", email)
		fmt.Printf("Eligible usage in %s uses your ChatGPT plan. Manage usage in your ChatGPT settings: %s\n", inst.Name, llm.SIWC.ManageUsageURL)
		fmt.Printf("Tokens are stored only on this machine (%s, 0600); a running core picks them up on its next call.\n", AuthDir(inst.Root))
		return nil
	case "logout":
		ok, err := llm.SignOut(llm.Paths(AuthDir(inst.Root)))
		if err != nil {
			return err
		}
		if ok {
			fmt.Println("signed out of ChatGPT (local tokens removed)")
		} else {
			fmt.Println("not signed in")
		}
		return nil
	}
	return errors.New(`unknown core command "` + sub + "\"\n\n" + Usage)
}
