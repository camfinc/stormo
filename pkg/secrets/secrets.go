// Package secrets keeps every agent's secrets in one local, never-committed file, layered so
// shared keys are written once:
//
//	shared:            # every agent that declares the name
//	  OPENROUTER_API_KEY: ...
//	units:
//	  sales:           # agents of that unit
//	    CRM_API_TOKEN: ...
//	agents:
//	  atlas:           # one agent
//	    TELEGRAM_BOT_TOKEN: ...
//	local:             # same shape again; overrides used only for local containers and the bench
//	  agents:
//	    atlas:
//	      TELEGRAM_BOT_TOKEN: <dev bot token>
//
// Precedence: agents > units > shared, and `local` on top when running locally. An agent only ever
// receives the names its agent.yaml declares, so a key sitting in `shared` never leaks to agents
// that did not ask for it. `secrets push` writes each agent's resolved set (without `local`) to its
// Secrets Manager secret as one JSON object.
package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/aws"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/env"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

const FileName = "secrets.local.yaml"

// Path is the instance's secrets file (SWARM_SECRETS_FILE overrides it).
func Path(root string) string {
	if p := os.Getenv("SWARM_SECRETS_FILE"); p != "" {
		return p
	}
	return filepath.Join(root, FileName)
}

// lookup finds name in the agent's layers, most specific first. ok is false when no layer has it.
func (ls *Layers) lookup(a *manifest.Agent, name string) (string, bool) {
	if ls == nil {
		return "", false
	}
	for _, l := range []*Layer{ls.Agents.Get(a.ID), ls.Units.Get(a.Unit), ls.Shared} {
		if l != nil {
			if v, ok := l.Get(name); ok {
				return v, true
			}
		}
	}
	return "", false
}

// Resolved is an agent's secrets for one target.
type Resolved struct {
	Values *env.Env `json:"values"`
	// Declared names without a value.
	Missing []string `json:"missing"`
	// Declared names whose value comes from the `local` overlay.
	Overridden []string `json:"overridden"`
	// Optional secrets without a value: not an error, what they gate is left out.
	Off []string `json:"off"`
}

// Names an agent receives for a target: its declared secrets, plus local-only ones (its core key).
func Names(a *manifest.Agent, target manifest.Target) []string {
	if target == manifest.Local {
		return append(append([]string{}, a.Secrets...), manifest.LocalOnlySecrets(a.Engine)...)
	}
	return a.Secrets
}

// Resolve returns values for exactly the names the agent declares, plus its optional secrets that
// have one. Empty strings count as missing; an empty optional secret is off, not missing.
func Resolve(f *File, a *manifest.Agent, target manifest.Target) Resolved {
	r := Resolved{Values: env.New(), Missing: []string{}, Overridden: []string{}, Off: []string{}}
	var local *Layers
	if target == manifest.Local {
		local = f.Local
	}
	value := func(name string) (string, bool, bool) {
		if v, ok := local.lookup(a, name); ok {
			return v, true, true
		}
		v, ok := f.Layers.lookup(a, name)
		return v, ok, false
	}
	for _, name := range Names(a, target) {
		v, _, fromLocal := value(name)
		if v == "" {
			r.Missing = append(r.Missing, name)
		} else {
			r.Values.Set(name, v)
		}
		if fromLocal {
			r.Overridden = append(r.Overridden, name)
		}
	}
	for _, o := range a.OptionalSecrets {
		v, _, fromLocal := value(o.Name)
		if v == "" {
			r.Off = append(r.Off, o.Name)
		} else {
			r.Values.Set(o.Name, v)
		}
		if fromLocal {
			r.Overridden = append(r.Overridden, o.Name)
		}
	}
	return r
}

// Present names the secrets with a value for a target, which decides the optional secrets. The aws
// layer stands for AWS (it is what `secrets push` sends). Without the file (e.g. a CI build) every
// declared name counts as present, so nothing is left out.
func Present(root string, a *manifest.Agent, target manifest.Target) ([]string, error) {
	path := Path(root)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		out := append([]string{}, a.Secrets...)
		for _, o := range a.OptionalSecrets {
			out = append(out, o.Name)
		}
		return append(out, manifest.LocalOnlySecrets(a.Engine)...), nil
	}
	f, err := Load(path)
	if err != nil {
		return nil, err
	}
	return Resolve(f, a, target).Values.Keys(), nil
}

var botToken = regexp.MustCompile(`_(BOT|APP)_TOKEN$`)

// ProdTokenClashes: bot tokens poll their platform, so a local container on the production token
// steals updates from (and breaks) the deployed agent. Local runs must override every
// *_BOT_TOKEN / *_APP_TOKEN.
func ProdTokenClashes(f *File, a *manifest.Agent) []string {
	out := []string{}
	names := append([]string{}, a.Secrets...)
	for _, o := range a.OptionalSecrets {
		names = append(names, o.Name)
	}
	for _, n := range names {
		if !botToken.MatchString(n) {
			continue
		}
		base, _ := f.Layers.lookup(a, n)
		local, ok := f.Local.lookup(a, n)
		if base != "" && (!ok || local == base) {
			out = append(out, n)
		}
	}
	return out
}

// RenderEnvFile is docker --env-file format; values verbatim (no quotes), one per line.
func RenderEnvFile(values *env.Env) (string, error) {
	var b strings.Builder
	for _, kv := range values.Pairs() {
		if strings.ContainsAny(kv.Value, "\r\n") {
			return "", fmt.Errorf("%s: multi-line values are not supported in env files", kv.Name)
		}
		b.WriteString(kv.Name + "=" + kv.Value + "\n")
	}
	if values.Len() == 0 {
		return "\n", nil
	}
	return b.String(), nil
}

// WritePrivate writes a 0600 file in a 0700 directory.
func WritePrivate(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// WriteLocalEnv renders .swarm/env/<agent>.env from the local overlay.
func WriteLocalEnv(root string, a *manifest.Agent) (string, Resolved, error) {
	f, err := Load(Path(root))
	if err != nil {
		return "", Resolved{}, err
	}
	r := Resolve(f, a, manifest.Local)
	body, err := RenderEnvFile(r.Values)
	if err != nil {
		return "", r, err
	}
	p := filepath.Join(root, ".swarm", "env", a.ID+".env")
	return p, r, WritePrivate(p, []byte(body))
}

// PerAgent names belong to one agent by design (its engine API key, its own bot/app tokens).
func PerAgent(name string) bool {
	if slices.Contains(engines.APIKeyNames(), name) {
		return true
	}
	for _, names := range manifest.ChannelSecrets {
		if slices.Contains(names, name) {
			return true
		}
	}
	return false
}

func header(resource string) string {
	return "# " + resource + " secrets. NEVER COMMIT (gitignored). Layers: shared > units.<unit> > agents.<id>;\n" +
		"# a more specific layer wins, so a per-agent entry (even an empty one) hides the shared value.\n" +
		"# `local:` repeats that shape with overrides for local containers (e.g. dev bot tokens).\n" +
		"# Push to AWS Secrets Manager with `stormo secrets push [agent] --yes`.\n" +
		"# `secrets init` / `secrets share` rewrite this file; they do not keep other comments.\n"
}

// Save writes the file 0600 with the standard header.
func Save(path, resource string, f *File) error {
	body, err := f.Marshal()
	if err != nil {
		return err
	}
	return WritePrivate(path, append([]byte(header(resource)), append(body, '\n')...))
}

func randomKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Init scaffolds or extends the secrets file with every declared name; never overwrites a value.
// A name two or more agents declare gets its slot under `shared:` (set once), except per-agent names.
func Init(inst *instance.Instance) (string, []string, error) {
	path := Path(inst.Root)
	f, err := Load(path)
	if err != nil {
		return path, nil, err
	}
	added := []string{}
	if f.Agents == nil {
		f.Agents = NewNamed()
	}
	agents := []*manifest.Agent{}
	for _, id := range manifest.AgentIDs(inst.Root) {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			return path, nil, err
		}
		agents = append(agents, a)
	}
	declaredBy := map[string]int{}
	for _, a := range agents {
		for _, n := range a.Secrets {
			declaredBy[n]++
		}
	}
	for _, a := range agents {
		for _, name := range a.Secrets {
			if _, known := f.Layers.lookup(a, name); known {
				continue
			}
			if !PerAgent(name) && declaredBy[name] > 1 {
				if f.Shared == nil {
					f.Shared = env.New()
				}
				f.Shared.Set(name, "")
				added = append(added, "shared."+name)
				continue
			}
			v := ""
			if slices.Contains(engines.APIKeyNames(), name) { // the engine API key: per agent, generated
				v = randomKey()
			}
			f.Agents.Ensure(a.ID).Set(name, v)
			added = append(added, a.ID+"."+name)
		}
		// Local-only names are per-agent keys we mint (the core reads the same entry); they live
		// under `local:` so `secrets push` never sends them to AWS.
		for _, name := range manifest.LocalOnlySecrets(a.Engine) {
			if _, known := f.Local.lookup(a, name); known {
				continue
			}
			if f.Local == nil {
				f.Local = &Layers{}
			}
			if f.Local.Agents == nil {
				f.Local.Agents = NewNamed()
			}
			f.Local.Agents.Ensure(a.ID).Set(name, randomKey())
			added = append(added, "local."+a.ID+"."+name)
		}
	}
	if _, err := os.Stat(path); len(added) == 0 && err == nil {
		return path, added, nil
	}
	return path, added, Save(path, inst.Names.Resource, f)
}

// ShareResult reports what `secrets share` did.
type ShareResult struct {
	Shared  []string `json:"shared"`
	Removed []string `json:"removed"` // agent.NAME copies dropped (identical or empty)
	Kept    []string `json:"kept"`    // agent.NAME copies with a different value: they still override
	Missing []string `json:"missing"` // names the source agent has no value for
}

// Share moves keys to `shared:`: each value from `from` unless shared already has one, then every
// per-agent copy that is identical or empty is dropped. Pure; the caller saves.
func Share(f *File, names []string, from string) (ShareResult, error) {
	r := ShareResult{Shared: []string{}, Removed: []string{}, Kept: []string{}, Missing: []string{}}
	for _, name := range names {
		if PerAgent(name) {
			return r, fmt.Errorf("%s belongs to one agent by design; it cannot be shared", name)
		}
		value := ""
		if f.Shared != nil {
			value, _ = f.Shared.Get(name)
		}
		if value == "" && from != "" {
			if l := f.Agents.Get(from); l != nil {
				value, _ = l.Get(name)
			}
		}
		if value == "" {
			r.Missing = append(r.Missing, name)
			continue
		}
		if f.Shared == nil {
			f.Shared = env.New()
		}
		f.Shared.Set(name, value)
		r.Shared = append(r.Shared, name)
		for _, id := range f.Agents.Keys() {
			l := f.Agents.Get(id)
			v, ok := l.Get(name)
			if !ok {
				continue
			}
			if v == value || v == "" {
				l.Delete(name)
				r.Removed = append(r.Removed, id+"."+name)
			} else {
				r.Kept = append(r.Kept, id+"."+name)
			}
		}
	}
	return r, nil
}

// FileProblems reports how the secrets file is stored (tracked by git, readable by others).
func FileProblems(root string) []string {
	path := Path(root)
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	out := []string{}
	run := func(args ...string) int {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode()
			}
			return -1
		}
		return 0
	}
	if run("ls-files", "--error-unmatch", path) == 0 {
		out = append(out, path+" is tracked by git: remove it from the index and rotate every value in it")
	}
	if run("check-ignore", "-q", path) == 1 {
		out = append(out, path+" is not gitignored")
	}
	if info.Mode().Perm()&0o077 != 0 {
		out = append(out, path+" is readable by group/others: chmod 600 it")
	}
	return out
}

// PushPlan is the key-name diff against Secrets Manager.
type PushPlan struct {
	Agent     string   `json:"agent"`
	SecretID  string   `json:"secretId"`
	Exists    bool     `json:"exists"`
	Added     []string `json:"added"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
	Unchanged []string `json:"unchanged"`
	Missing   []string `json:"missing"`
}

func PlanPush(a *manifest.Agent, r Resolved, region string, cli aws.CLI) (PushPlan, error) {
	id := a.Deploy.SecretID
	got := cli([]string{"secretsmanager", "get-secret-value", "--region", region, "--secret-id", id, "--query", "SecretString", "--output", "text"}, nil)
	current := map[string]string{}
	exists := false
	if got.Code == 0 {
		exists = true
		body := strings.TrimSpace(got.Stdout)
		if body == "" {
			body = "{}"
		}
		if err := json.Unmarshal([]byte(body), &current); err != nil {
			return PushPlan{}, fmt.Errorf("%s exists but is not a JSON object; fix it by hand", id)
		}
	} else if !strings.Contains(got.Stderr, "ResourceNotFoundException") {
		return PushPlan{}, fmt.Errorf("aws get-secret-value %s failed: %s", id, strings.TrimSpace(got.Stderr))
	}
	want := r.Values.Map()
	keys := func(m map[string]string) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	p := PushPlan{Agent: a.ID, SecretID: id, Exists: exists, Added: []string{}, Changed: []string{}, Removed: []string{}, Unchanged: []string{}, Missing: r.Missing}
	for _, k := range keys(want) {
		cur, ok := current[k]
		switch {
		case !ok:
			p.Added = append(p.Added, k)
		case cur != want[k]:
			p.Changed = append(p.Changed, k)
		default:
			p.Unchanged = append(p.Unchanged, k)
		}
	}
	for _, k := range keys(current) {
		if _, ok := want[k]; !ok {
			p.Removed = append(p.Removed, k)
		}
	}
	return p, nil
}

func ApplyPush(p PushPlan, r Resolved, region, resource string, cli aws.CLI) error {
	body, err := json.Marshal(r.Values)
	if err != nil {
		return err
	}
	args := []string{"secretsmanager", "put-secret-value", "--region", region, "--secret-id", p.SecretID, "--secret-string", "file:///dev/stdin"}
	if !p.Exists {
		args = []string{"secretsmanager", "create-secret", "--region", region, "--name", p.SecretID,
			"--description", resource + " secrets for agent " + p.Agent,
			"--tags", "Key=app,Value=" + resource, "Key=agent,Value=" + p.Agent,
			"--secret-string", "file:///dev/stdin"}
	}
	s := string(body)
	if res := cli(args, &s); res.Code != 0 {
		return fmt.Errorf("aws %s %s failed: %s", args[1], p.SecretID, strings.TrimSpace(res.Stderr))
	}
	return nil
}
