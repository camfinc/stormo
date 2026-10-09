package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/camfinc/stormo/pkg/aws"
	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/place"
	"github.com/camfinc/stormo/pkg/secrets"
	"github.com/camfinc/stormo/pkg/shared"
	"go.yaml.in/yaml/v3"
)

func compose(root string, a *manifest.Agent, quiet bool, args ...string) (string, error) {
	cc, err := ComposeCommand()
	if err != nil {
		return "", err
	}
	argv := append(append(cc, "-p", ProjectName(a), "-f", ComposePath(root, a)), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	var out bytes.Buffer
	if quiet {
		cmd.Stdout = &out
	} else {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s exited: %w", strings.Join(argv, " "), err)
	}
	return out.String(), nil
}

// UpOptions for Up.
type UpOptions struct {
	// Vouch that ECS is stopped when it cannot be checked (no AWS access).
	AllowProdToken bool
	// ECS state already established by the caller (handoff), skipping the check.
	Other          place.SideState
	KeepHome       bool
	NapInterval    int
	RebuildSidecar bool
	Log            func(string)
}

// Up builds, renders and (re)starts one agent locally. By default the agent's home is discarded
// and rebuilt by rehydrate from the latest local nap, exactly like an ECS redeploy; KeepHome skips
// that for the fastest possible restart.
func Up(inst *instance.Instance, id string, o UpOptions) (int, []manifest.Off, error) {
	if o.Log == nil {
		o.Log = func(string) {}
	}
	root := inst.Root
	a, err := manifest.Load(root, id, inst.Names.Secret)
	if err != nil {
		return 0, nil, err
	}
	eng, err := engines.Get(a.Engine.Kind, inst)
	if err != nil {
		return 0, nil, err
	}
	file, err := secrets.Load(secrets.Path(root))
	if err != nil {
		return 0, nil, err
	}
	resolved := secrets.Resolve(file, a, manifest.Local)
	if len(resolved.Missing) > 0 {
		return 0, nil, fmt.Errorf("%s: missing secrets %s in %s (stormo secrets init / check)", id, strings.Join(resolved.Missing, ", "), secrets.Path(root))
	}
	// One place at a time: on the production Slack app, start only if ECS is not running it (a
	// separate dev app under local.agents.<id> may run alongside).
	sharedTokens := len(secrets.ProdTokenClashes(file, a)) > 0
	other := o.Other
	if other == "" {
		other = place.Unknown
		if sharedTokens {
			other = place.RemoteSide(inst, id, aws.Exec, deploy.DefaultTarget(inst))
		}
	}
	if g := place.GuardStart(id, place.Local, other, sharedTokens, o.AllowProdToken); !g.OK {
		return 0, nil, fmt.Errorf("%s", g.Reason)
	}

	// Optional secrets without a value leave out what they gate (skills, actions, channels).
	r, err := build.Agent(inst, id, build.Options{Target: manifest.Local, Present: resolved.Values.Keys()})
	if err != nil {
		return 0, nil, err
	}
	envFile, _, err := secrets.WriteLocalEnv(root, a)
	if err != nil {
		return 0, nil, err
	}
	// The agent's naps move into its own folder the first time it starts after format 0.
	if moved, err := loop.MigrateLocalStore(root, id); err != nil {
		return 0, nil, err
	} else if moved {
		o.Log(fmt.Sprintf("%s: moved its naps from .swarm/store/%s to agents/%s/data/store", id, id, id))
	}
	if _, err := manifest.EnsureDataDir(root, id); err != nil {
		return 0, nil, err
	}
	storeDir := loop.LocalStoreDir(root, id)
	sharedDir := shared.LocalDir(root)
	moved, conflicts, err := shared.MigrateLocal(root)
	if err != nil {
		return 0, nil, fmt.Errorf("moving .swarm/shared to workdir/: %w", err)
	}
	{
		for _, l := range moved {
			o.Log(fmt.Sprintf("shared space: moved .swarm/shared/%s to workdir/%s", l, l))
		}
		for _, l := range conflicts {
			o.Log(fmt.Sprintf("WARNING: .swarm/shared/%s and workdir/%s both exist; agents now mount workdir/%s, merge the old one by hand", l, l, l))
		}
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return 0, nil, err
	}
	for _, l := range shared.Layers(a) {
		if err := os.MkdirAll(filepath.Join(sharedDir, l.Layer), 0o755); err != nil {
			return 0, nil, err
		}
	}
	image, err := EnsureSidecar(o.RebuildSidecar, o.Log)
	if err != nil {
		return 0, nil, err
	}
	port, err := Port(root, id)
	if err != nil {
		return 0, nil, err
	}
	// The manifest's learning.nap_interval_seconds, unless --nap-interval overrides it.
	interval := o.NapInterval
	if interval == 0 {
		interval = r.Agent.Learning.NapIntervalSeconds
	}
	if interval == 0 {
		interval = 900
	}
	spec := Render(r.Agent, eng, Paths{Root: root, BaselineDir: r.Out, EnvFile: envFile, StoreDir: storeDir, SharedDir: sharedDir}, port, interval, image)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(spec); err != nil {
		return 0, nil, err
	}
	if err := secrets.WritePrivate(ComposePath(root, a), append([]byte("# GENERATED by `stormo start`. Do not edit.\n"), b.Bytes()...)); err != nil {
		return 0, nil, err
	}
	// Stop (agent first, then nap's final snapshot), drop the home volume, start fresh.
	down := []string{"down", "--timeout", "100"}
	if !o.KeepHome {
		down = append(down, "--volumes")
	}
	if _, err := compose(root, a, false, down...); err != nil {
		return 0, nil, err
	}
	if _, err := compose(root, a, false, "up", "-d", "--force-recreate"); err != nil {
		return 0, nil, err
	}
	return port, r.Off, nil
}

func load(inst *instance.Instance, id string) (*manifest.Agent, error) {
	return manifest.Load(inst.Root, id, inst.Names.Secret)
}

// Down stops an agent (agent first, then nap's final snapshot) and drops its home volume.
func Down(inst *instance.Instance, id string) error {
	a, err := load(inst, id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(ComposePath(inst.Root, a)); os.IsNotExist(err) {
		return nil // never started here
	}
	_, err = compose(inst.Root, a, false, "down", "--timeout", "100", "--volumes")
	return err
}

func Logs(inst *instance.Instance, id string, follow bool) error {
	a, err := load(inst, id)
	if err != nil {
		return err
	}
	args := []string{"logs", "--tail", "200"}
	if follow {
		args = append(args, "-f")
	}
	_, err = compose(inst.Root, a, false, args...)
	return err
}

// NapNow takes a nap right now (instead of waiting for the interval), e.g. before a dream.
func NapNow(inst *instance.Instance, id string) error {
	a, err := load(inst, id)
	if err != nil {
		return err
	}
	_, err = compose(inst.Root, a, false, "exec", "-T", "nap", "stormo", "nap")
	return err
}

// Conversations is the agent's chat surface on its local engine API (engine.Runtime.Conversations),
// keyed with its API key from the local secrets.
func Conversations(inst *instance.Instance, id string) (engine.Conversations, error) {
	a, err := load(inst, id)
	if err != nil {
		return nil, err
	}
	rt, err := engines.Runtime(a.Engine.Kind)
	if err != nil {
		return nil, err
	}
	file, err := secrets.Load(secrets.Path(inst.Root))
	if err != nil {
		return nil, err
	}
	key, _ := secrets.Resolve(file, a, manifest.Local).Values.Get(rt.APIKeyName())
	if key == "" {
		return nil, fmt.Errorf("%s: %s missing in %s", id, rt.APIKeyName(), secrets.Path(inst.Root))
	}
	port, err := Port(inst.Root, id)
	if err != nil {
		return nil, err
	}
	return rt.Conversations(apiCall(id, fmt.Sprintf("http://127.0.0.1:%d", port), key)), nil
}

// ErrNotRunning is an agent whose engine API does not answer here.
var ErrNotRunning = errors.New("not running locally")

// apiCall sends requests to an agent's API. No client timeout: a turn can run for minutes; the
// caller's context ends it.
func apiCall(id, endpoint, key string) engine.APICall {
	return func(ctx context.Context, method, path string, body any) (int, []byte, error) {
		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return 0, nil, err
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint+path, rd)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				return 0, nil, fmt.Errorf("%s: %w (start it first)", id, ErrNotRunning)
			}
			return 0, nil, fmt.Errorf("%s: %w", id, err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		return res.StatusCode, raw, err
	}
}
