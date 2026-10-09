package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/camfinc/stormo/pkg/out"
)

// Usage is the core section of the CLI usage text.
const Usage = `  core (the core service on this machine, docs/core.md):
  core up | down | status                start in the background / stop / show login, plan limit, usage
  core serve                             run in the foreground (logs to stdout)
  core login | logout                    Continue with ChatGPT (browser sign-in, ChatGPT plan usage) held only by the core
                                         login --no-open: print the sign-in page, do not open a browser
  core activity <agent> [n]              the agent's last n hook events with previews (files, commands; default 40)`

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

// UpResult is `core up`'s outcome.
type UpResult struct {
	Started bool   `json:"started"` // false: a core already answered
	PID     int    `json:"pid,omitempty"`
	Port    int    `json:"port"`
	Log     string `json:"log,omitempty"`
	Login   string `json:"login"`
}

func up(inst *instance.Instance, o *out.Writer) error {
	root := inst.Root
	if h := health(); h != nil {
		o.Result(UpResult{Port: CorePort(), Login: h.Login}, func(w io.Writer) { fmt.Fprintf(w, "core already running on %s\n", base()) })
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
	o.Step("starting the core (pid %d)", pid)
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if h := health(); h != nil {
			o.Result(UpResult{true, pid, CorePort(), logPath(root), h.Login}, func(w io.Writer) {
				fmt.Fprintf(w, "core up on %s (pid %d, log %s); login: %s\n", base(), pid, logPath(root), h.Login)
				if h.Login != string(llm.LoginOK) {
					fmt.Fprintln(w, "sign in with: stormo core login")
				}
			})
			return nil
		}
	}
	return fmt.Errorf("core did not come up; see %s", logPath(root))
}

// DownResult is `core down`'s outcome. Not stopped: Reason is not_running, or not_ours when a core
// answers that `core up` did not start (no pid file), which down leaves alone.
type DownResult struct {
	Stopped bool   `json:"stopped"`
	PID     int    `json:"pid,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// down stops only the process named in the pid file `core up` wrote.
func down(inst *instance.Instance, o *out.Writer) error {
	root := inst.Root
	pid := readPid(root)
	if pid == 0 {
		_ = os.Remove(pidPath(root))
		if health() != nil {
			o.Result(DownResult{Reason: "not_ours"}, func(w io.Writer) {
				fmt.Fprintf(w, "core answers on %s but was not started by `core up` (no pid file)\n", base())
			})
		} else {
			o.Result(DownResult{Reason: "not_running"}, func(w io.Writer) { fmt.Fprintln(w, "core is not running") })
		}
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	_ = os.Remove(pidPath(root))
	o.Result(DownResult{Stopped: true, PID: pid}, func(w io.Writer) { fmt.Fprintf(w, "core stopped (pid %d)\n", pid) })
	return nil
}

// StatusResult is `core status`: whether the core answers, and its gateway when it does.
type StatusResult struct {
	Running bool        `json:"running"`
	Port    int         `json:"port"`
	Login   string      `json:"login"` // the gateway's, or the sign-in on disk when not running
	Gateway *llm.Status `json:"gateway,omitempty"`
}

func status(inst *instance.Instance, o *out.Writer) error {
	c := http.Client{Timeout: 1500 * time.Millisecond}
	r, err := c.Get(base() + "/api/gateway")
	if err != nil {
		state := llm.NewChatGPTAuth(llm.AuthOptions{Paths: llm.Paths(AuthDir(inst.Root))}).State()
		o.Result(StatusResult{Port: CorePort(), Login: string(state)}, func(w io.Writer) {
			fmt.Fprintf(w, "core: not running (%s); ChatGPT sign-in on disk: %s\n", base(), state)
		})
		return nil
	}
	defer r.Body.Close()
	var s llm.Status
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		return err
	}
	o.Result(StatusResult{true, CorePort(), string(s.Login), &s}, func(w io.Writer) { printStatus(w, s) })
	return nil
}

func printStatus(w io.Writer, s llm.Status) {
	fmt.Fprintf(w, "core: running on %s\n", base())
	if s.Login == llm.LoginOK {
		account := ""
		if s.Account != nil {
			account = " (" + *s.Account + ")"
		}
		fmt.Fprintf(w, "Using ChatGPT plan%s. Manage usage: %s\n", account, s.ManageUsageURL)
	} else {
		fmt.Fprintf(w, "ChatGPT sign-in: %s (run: stormo core login)\n", s.Login)
	}
	if s.PlanLimitedUntil != nil {
		fmt.Fprintf(w, "ChatGPT plan usage limit reached; agents use their fallback until %s. Manage usage: %s\n", *s.PlanLimitedUntil, s.ManageUsageURL)
	}
	ids := []string{}
	for _, m := range s.Models {
		ids = append(ids, m.ID)
	}
	fmt.Fprintf(w, "llm: %d/%d in flight, %d queued; models %s\n", s.Inflight, s.Concurrency, s.Queued, strings.Join(ids, ", "))
	if len(s.Usage) == 0 {
		fmt.Fprintln(w, "usage: no calls yet")
		return
	}
	fmt.Fprintln(w, "agent         reqs   ok  429  err   in_tok  cached  out_tok  last")
	agents := make([]string, 0, len(s.Usage))
	for a := range s.Usage {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	for _, a := range agents {
		u := s.Usage[a]
		fmt.Fprintf(w, "%-12s %5d %4d %4d %4d %8d %7d %8d  %s\n", a, u.Requests, u.OK, u.RateLimited, u.Errors, u.InputTokens, u.CachedTokens, u.OutputTokens, u.LastAt)
	}
}

func serve(inst *instance.Instance) error {
	c, err := StartCore(CoreOptions{Inst: inst, Port: CorePort(), Bridge: BridgeHosts()})
	if err != nil {
		return err
	}
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	fmt.Printf("%s swarm core listening on http://%s\n", at, c.Addr)
	for _, b := range c.Bridges {
		fmt.Printf("%s swarm core bridge for containers on http://%s (model gateway and /health only)\n", at, b)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	return c.Close()
}

// CommandOptions are the CLI's settings for core commands.
type CommandOptions struct {
	Out *out.Writer
	// NoOpen: login reports the sign-in page (an auth_url event) instead of opening a browser.
	NoOpen bool
}

// LoginResult is `core login` and `core logout`'s outcome.
type LoginResult struct {
	Login   string `json:"login"`
	Account string `json:"account,omitempty"`
	Changed bool   `json:"changed"` // logout: tokens were removed
}

// ownerRequest calls the running core as its owner (.swarm/core/owner.token).
func ownerRequest(root, method, path string, body io.Reader) (*http.Response, error) {
	tok := ReadOwnerToken(root)
	if tok == "" {
		return nil, fmt.Errorf("no owner token at %s: start the core with `stormo core up`", OwnerTokenPath(root))
	}
	req, err := http.NewRequest(method, base()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := http.Client{Timeout: 30 * time.Second}
	res, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the core is not answering on %s (start it with `stormo core up`): %w", base(), err)
	}
	return res, nil
}

// ownerJSON calls the core as its owner and decodes a JSON answer into v; an error body becomes the error.
func ownerJSON(root, method, path string, body io.Reader, v any) error {
	res, err := ownerRequest(root, method, path, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s (%s)", e.Error.Message, e.Error.Code)
		}
		return fmt.Errorf("core answered HTTP %d", res.StatusCode)
	}
	return json.Unmarshal(b, v)
}

// ActivityResult is `core activity`'s outcome.
type ActivityResult struct {
	Agent   string          `json:"agent"`
	Live    *Live           `json:"live"`
	Entries []TimelineEntry `json:"entries"`
}

func activity(inst *instance.Instance, args []string, o *out.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: stormo core activity <agent> [n]")
	}
	n := 40
	if len(args) > 1 {
		v, err := strconv.Atoi(args[1])
		if err != nil || v <= 0 {
			return fmt.Errorf("n must be a positive number, got %q", args[1])
		}
		n = v
	}
	var r ActivityResult
	if err := ownerJSON(inst.Root, http.MethodGet, fmt.Sprintf("/api/agents/%s/activity?limit=%d", args[0], n), nil, &r); err != nil {
		return err
	}
	o.Result(r, func(w io.Writer) {
		if r.Live != nil && r.Live.Tool != "" {
			fmt.Fprintf(w, "now: %s (since %s, %d call(s) running)\n", r.Live.Tool, r.Live.ToolSince, r.Live.Running)
		}
		if len(r.Entries) == 0 {
			fmt.Fprintf(w, "no hook events from %s yet (it posts them once it runs with the core's hooks.outbound)\n", args[0])
		}
		for i := len(r.Entries) - 1; i >= 0; i-- {
			e := r.Entries[i]
			fmt.Fprintf(w, "%s  %-22s %-24s %s\n", e.At, e.Event, e.Tool, e.Preview)
		}
	})
	return nil
}

// Command runs `stormo core <sub>`.
func Command(inst *instance.Instance, sub string, args []string, opts CommandOptions) error {
	o := opts.Out
	if o == nil {
		o = &out.Writer{W: os.Stdout}
	}
	switch sub {
	case "serve":
		return serve(inst)
	case "up":
		return up(inst, o)
	case "down":
		return down(inst, o)
	case "", "status":
		return status(inst, o)
	case "activity":
		return activity(inst, args, o)
	case "login":
		port := llm.SIWC.DefaultLoginPort
		if n, err := strconv.Atoi(os.Getenv("SWARM_CORE_LOGIN_PORT")); err == nil && n > 0 {
			port = n
		}
		paths := llm.Paths(AuthDir(inst.Root))
		var open func(string) error // nil: llm opens the default browser
		if opts.NoOpen || o.JSON {
			open = func(u string) error {
				o.AuthURL(u)
				if opts.NoOpen {
					return nil
				}
				return llm.DefaultOpenBrowser(u)
			}
		}
		conn, err := llm.SignIn(context.Background(), llm.SignInOptions{Paths: paths, AppName: inst.Name, Port: &port, Print: o.Line, OpenBrowser: open})
		if err != nil {
			return err
		}
		o.Result(LoginResult{Login: string(llm.LoginOK), Account: conn.Email, Changed: true}, func(w io.Writer) {
			email := ""
			if conn.Email != "" {
				email = " (" + conn.Email + ")"
			}
			fmt.Fprintf(w, "\nYou're using your ChatGPT plan%s.\n", email)
			fmt.Fprintf(w, "Eligible usage in %s uses your ChatGPT plan. Manage usage in your ChatGPT settings: %s\n", inst.Name, llm.SIWC.ManageUsageURL)
			fmt.Fprintf(w, "Tokens are stored only on this machine (%s, 0600); a running core picks them up on its next call.\n", AuthDir(inst.Root))
		})
		return nil
	case "logout":
		ok, err := llm.SignOut(llm.Paths(AuthDir(inst.Root)))
		if err != nil {
			return err
		}
		o.Result(LoginResult{Login: string(llm.LoginMissing), Changed: ok}, func(w io.Writer) {
			if ok {
				fmt.Fprintln(w, "signed out of ChatGPT (local tokens removed)")
			} else {
				fmt.Fprintln(w, "not signed in")
			}
		})
		return nil
	}
	return errors.New(`unknown core command "` + sub + "\"\n\n" + Usage)
}
