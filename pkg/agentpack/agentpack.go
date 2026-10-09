// Package agentpack moves an agent between instances as one zip (docs/agent-standard.md): Export
// packs its folder (format 1) with what it relies on outside it (its persona, its unit's identity,
// the bridge actions it uses) and, on request, its data (naps and every secret value it resolves);
// Import unpacks one into an instance, checks it and rolls back if it does not check.
package agentpack

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
	"github.com/camfinc/stormo/pkg/version"
	"github.com/camfinc/stormo/pkg/yamlfmt"
)

// Format names the zip's layout; ManifestFile describes the rest of the zip.
const (
	Format       = "stormo-agent/1"
	ManifestFile = "stormo-agent.json"
	// Disclaimer is at the root of every zip that carries secret values.
	Disclaimer = "DATA-EXPORT-CONTAINS-SECRETS.txt"
)

const disclaimerText = `This file was exported with its data: it contains the agent's SECRET VALUES in plain
text (data/secrets.yaml: API keys, bot tokens, passwords, every value the agent resolves,
shared ones included) and its memory and conversation history (data/store: naps, which can hold
client data).

Treat it like the credentials themselves: keep it out of email, chat, git and shared drives,
store it encrypted, and delete it once it is imported. If it leaked, rotate every value in
data/secrets.yaml.
`

// Manifest is stormo-agent.json.
type Manifest struct {
	Format      string `json:"format"`
	Agent       string `json:"agent"`
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	AgentFormat int    `json:"agentFormat"`
	Engine      struct {
		Kind    string `json:"kind"`
		Version string `json:"version"`
	} `json:"engine"`
	// Mode is config (the definition) or data (also its naps and secret values).
	Mode            string `json:"mode"`
	ContainsSecrets bool   `json:"containsSecrets"`
	Source          struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"source"`
	ExportedAt string `json:"exportedAt"`
	Stormo     string `json:"stormo"`
	// Persona is the slug of the persona folder in the zip, if any.
	Persona string `json:"persona,omitempty"`
	// Files is the sha256 of every other member, by path.
	Files map[string]string `json:"files"`
}

// Error is a refusal with a stable code for --json: legacy, exists, unknown_unit, missing_actions,
// tracked, invalid.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func refuse(code, format string, a ...any) error { return &Error{code, fmt.Sprintf(format, a...)} }

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// tracked reports whether git tracks anything under rel (relative to root).
func tracked(root, rel string) bool {
	out, err := exec.Command("git", "-C", root, "ls-files", "--", rel).Output()
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

// ExportResult is what Export wrote.
type ExportResult struct {
	Path            string `json:"path"`
	Agent           string `json:"agent"`
	Mode            string `json:"mode"`
	Files           int    `json:"files"`
	ContainsSecrets bool   `json:"containsSecrets"`
	Naps            int    `json:"naps"`
}

// Export writes agent id as a zip at out: config only, or with data (its naps and secret values).
func Export(inst *instance.Instance, id string, withData bool, out string) (*ExportResult, error) {
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	if a.Format < 1 {
		return nil, refuse("legacy", "%s uses agent.yaml format 0; `stormo migrate agent %s` first", id, id)
	}
	if tracked(inst.Root, filepath.Join("agents", id, "data")) {
		return nil, refuse("tracked", "agents/%s/data is tracked by git: its secret values may already be in history; remove it from the index and rotate them first", id)
	}
	m := Manifest{Format: Format, Agent: id, Name: a.Name, Unit: a.Unit, AgentFormat: a.Format, Mode: "config",
		ExportedAt: time.Now().UTC().Format(time.RFC3339), Stormo: version.String(), Files: map[string]string{}}
	m.Engine.Kind, m.Engine.Version = a.Engine.Kind, a.Engine.Version
	m.Source.Name, m.Source.Slug = inst.Name, inst.Slug
	members := map[string][]byte{}

	agentDir := manifest.AgentDir(inst.Root, id)
	if err := addTree(members, agentDir, "agent", func(rel string) bool { return rel == "data" || strings.HasPrefix(rel, "data/") }); err != nil {
		return nil, err
	}
	if dir, slug := personaDir(inst, a); dir != "" {
		if err := addTree(members, dir, "persona/"+slug, nil); err != nil {
			return nil, err
		}
		m.Persona = slug
	}
	u, err := manifest.LoadUnit(inst.Root, a.Unit)
	if err != nil {
		u = &manifest.Unit{ID: a.Unit, Name: a.Unit}
	}
	members["unit.json"], _ = json.MarshalIndent(u, "", "  ")
	if len(a.Actions) > 0 {
		body, err := actionsYAML(inst, a.Actions)
		if err != nil {
			return nil, err
		}
		members["actions.yaml"] = body
	}

	res := &ExportResult{Agent: id, Mode: "config"}
	if withData {
		m.Mode, res.Mode = "data", "data"
		tmp, err := os.MkdirTemp("", "stormo-export-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		// The latest nap complete, older ones learning files only (as a handoff carries them).
		r, err := loop.SyncNaps(loop.LocalStore{Root: inst.Root}, loop.FsStore{Root: tmp}, id, "", true)
		if err != nil {
			return nil, err
		}
		res.Naps = len(r.Naps)
		if err := addTree(members, filepath.Join(tmp, id), "data/store", nil); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		f, err := secrets.Load(secrets.Path(inst.Root))
		if err != nil {
			return nil, err
		}
		aws := secrets.Resolve(f, a, manifest.AWS).Values
		local := secrets.Resolve(f, a, manifest.Local).Values
		doc := map[string]map[string]string{"secrets": {}, "local": {}}
		for _, kv := range aws.Pairs() {
			doc["secrets"][kv.Name] = kv.Value
		}
		for _, kv := range local.Pairs() {
			if v, ok := aws.Get(kv.Name); !ok || v != kv.Value {
				doc["local"][kv.Name] = kv.Value
			}
		}
		body, err := yaml.Marshal(doc)
		if err != nil {
			return nil, err
		}
		members["data/secrets.yaml"] = append([]byte("# Every secret value "+id+" resolves (shared and unit values included). SECRET.\n"), body...)
		members[Disclaimer] = []byte(disclaimerText)
		m.ContainsSecrets, res.ContainsSecrets = true, true
	}

	names := make([]string, 0, len(members))
	for p, b := range members {
		names = append(names, p)
		m.Files[p] = sum(b)
	}
	sort.Strings(names)
	head, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if out == "" {
		out = fmt.Sprintf("%s-%s-%s.stormo-agent.zip", id, m.Mode, time.Now().Format("20060102"))
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, body []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}
	if err := write(ManifestFile, head); err != nil {
		return nil, err
	}
	for _, n := range names {
		if err := write(n, members[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	perm := os.FileMode(0o644)
	if m.ContainsSecrets {
		perm = 0o600
	}
	if err := os.WriteFile(out, buf.Bytes(), perm); err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(out)
	res.Path, res.Files = abs, len(names)
	return res, nil
}

// addTree adds every file under dir as prefix/<rel>; skip (relative, slash-separated) leaves
// paths out.
func addTree(members map[string][]byte, dir, prefix string, skip func(rel string) bool) error {
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel != "." && skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Name() == ".DS_Store" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		members[prefix+"/"+rel] = b
		return nil
	})
}

// personaDir is the agent's persona folder and slug, when it can be found.
func personaDir(inst *instance.Instance, a *manifest.Agent) (string, string) {
	if a.Persona == nil || a.Persona.Path == "" {
		return "", ""
	}
	dir := filepath.Join(inst.Root, a.Persona.Path)
	if a.Persona.Repo != "" {
		repos := os.Getenv("SWARM_REPOS_DIR")
		if repos == "" {
			repos = filepath.Dir(inst.Root)
		}
		dir = filepath.Join(repos, a.Persona.Repo, a.Persona.Path)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", ""
	}
	return dir, filepath.Base(dir)
}

// actionsYAML is the instance's bridge actions the agent uses, as written in its actions file.
func actionsYAML(inst *instance.Instance, names []string) ([]byte, error) {
	if inst.BridgeActions == "" {
		return nil, nil
	}
	body, err := os.ReadFile(filepath.Join(inst.Root, inst.BridgeActions))
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s: not a list of actions", inst.BridgeActions)
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, item := range doc.Content[0].Content {
		if want[actionName(item)] {
			list.Content = append(list.Content, item)
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(list); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

func actionName(item *yaml.Node) string {
	for i := 0; i+1 < len(item.Content); i += 2 {
		if item.Content[i].Value == "name" {
			return item.Content[i+1].Value
		}
	}
	return ""
}

// Read opens a zip and checks it: its manifest's format and every member's hash; member paths
// are never absolute and never leave the zip.
func Read(zipPath string) (*Manifest, map[string][]byte, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, nil, err
	}
	defer zr.Close()
	members := map[string][]byte{}
	for _, f := range zr.File {
		name := f.Name
		if strings.HasSuffix(name, "/") {
			continue
		}
		if path.IsAbs(name) || strings.Contains(name, "\\") || path.Clean(name) != name || strings.HasPrefix(name, "../") || name == ".." {
			return nil, nil, refuse("invalid", "%s: unsafe member path %q", zipPath, name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 512<<20))
		rc.Close()
		if err != nil {
			return nil, nil, err
		}
		members[name] = b
	}
	head, ok := members[ManifestFile]
	if !ok {
		return nil, nil, refuse("invalid", "%s is not a stormo agent export (no %s)", zipPath, ManifestFile)
	}
	var m Manifest
	if err := json.Unmarshal(head, &m); err != nil {
		return nil, nil, refuse("invalid", "%s: %v", ManifestFile, err)
	}
	if m.Format != Format {
		return nil, nil, refuse("invalid", "%s is format %q; this stormo reads %q", zipPath, m.Format, Format)
	}
	delete(members, ManifestFile)
	for p, b := range members {
		if want, ok := m.Files[p]; !ok || want != sum(b) {
			return nil, nil, refuse("invalid", "%s: %s does not match its manifest (changed or added after the export)", zipPath, p)
		}
	}
	for p := range m.Files {
		if _, ok := members[p]; !ok {
			return nil, nil, refuse("invalid", "%s: %s is missing", zipPath, p)
		}
	}
	return &m, members, nil
}

// ImportOptions: As renames the agent, Unit puts it in another unit, Replace overwrites an agent
// with that id, WithActions adds bridge actions the instance lacks.
type ImportOptions struct {
	As, Unit             string
	Replace, WithActions bool
}

// ImportResult is what Import did.
type ImportResult struct {
	Agent   string   `json:"agent"`
	From    string   `json:"from"` // the source instance's slug
	Mode    string   `json:"mode"`
	Changes []string `json:"changes"`
}

// Import unpacks zipPath into the instance. check validates the result (stormo check); on any
// failure the instance is left as it was.
func Import(inst *instance.Instance, zipPath string, o ImportOptions, check func(id string) error) (*ImportResult, error) {
	m, members, err := Read(zipPath)
	if err != nil {
		return nil, err
	}
	id := m.Agent
	if o.As != "" {
		id = o.As
	}
	if !instance.ValidSlug(id) {
		return nil, refuse("invalid", "%q is not an agent id (a lowercase slug)", id)
	}
	unit := m.Unit
	if o.Unit != "" {
		unit = o.Unit
	}
	units := manifest.UnitIDs(inst.Root)
	if unit == "group" || !contains(units, unit) {
		return nil, refuse("unknown_unit", "this instance has no unit %q (units: %s); pass --unit", unit, strings.Join(units, ", "))
	}
	dir := manifest.AgentDir(inst.Root, id)
	if _, err := os.Stat(dir); err == nil && !o.Replace {
		return nil, refuse("exists", "agents/%s exists; pass --replace to overwrite it, or --as to import under another id", id)
	}
	if tracked(inst.Root, filepath.Join("agents", id, "data")) {
		return nil, refuse("tracked", "agents/%s/data is tracked by git; remove it from the index first", id)
	}
	r := &ImportResult{Agent: id, From: m.Source.Slug, Mode: m.Mode, Changes: []string{}}

	// Bridge actions the agent uses that the instance does not have.
	var actionsBody []byte
	if b, ok := members["actions.yaml"]; ok {
		missing, body, err := missingActions(inst, b)
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			if !o.WithActions {
				return nil, refuse("missing_actions", "this instance has no bridge actions %s; pass --with-actions to add them to %s", strings.Join(missing, ", "), inst.BridgeActions)
			}
			actionsBody = body
			r.Changes = append(r.Changes, "bridge actions added: "+strings.Join(missing, ", "))
		}
	}

	// Stage the new folder next to the old one, then swap; roll back on failure.
	stage := filepath.Join(inst.Root, "agents", ".import-"+id)
	_ = os.RemoveAll(stage)
	defer os.RemoveAll(stage)
	for p, b := range members {
		rel, ok := strings.CutPrefix(p, "agent/")
		if !ok || rel == "data" || strings.HasPrefix(rel, "data/") {
			continue
		}
		if err := writeFile(filepath.Join(stage, filepath.FromSlash(rel)), b, 0o644); err != nil {
			return nil, err
		}
	}
	if err := retarget(filepath.Join(stage, "agent.yaml"), id, unit, m); err != nil {
		return nil, err
	}

	// Persona: the instance's own when it has one by that slug, else the zip's.
	if m.Persona != "" {
		pdir := filepath.Join(inst.Root, "personas", m.Persona)
		if _, err := os.Stat(pdir); err == nil {
			r.Changes = append(r.Changes, "persona: kept this instance's personas/"+m.Persona)
		} else {
			for p, b := range members {
				if rel, ok := strings.CutPrefix(p, "persona/"+m.Persona+"/"); ok {
					if err := writeFile(filepath.Join(pdir, filepath.FromSlash(rel)), b, 0o644); err != nil {
						return nil, err
					}
				}
			}
			r.Changes = append(r.Changes, "persona: added personas/"+m.Persona)
		}
	}

	// Swap.
	old := ""
	if _, err := os.Stat(dir); err == nil {
		old = filepath.Join(inst.Root, "agents", ".replaced-"+id)
		_ = os.RemoveAll(old)
		if err := os.Rename(dir, old); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(stage, dir); err != nil {
		return nil, err
	}
	rollback := func(cause error) (*ImportResult, error) {
		_ = os.RemoveAll(dir)
		if old != "" {
			_ = os.Rename(old, dir)
		}
		return nil, cause
	}
	// An agent replaced without data keeps its own data.
	hasData := m.Mode == "data"
	if old != "" && !hasData {
		if _, err := os.Stat(filepath.Join(old, "data")); err == nil {
			if err := os.Rename(filepath.Join(old, "data"), filepath.Join(dir, "data")); err != nil {
				return rollback(err)
			}
		}
	}
	if hasData {
		if _, err := manifest.EnsureDataDir(inst.Root, id); err != nil {
			return rollback(err)
		}
		n := 0
		for p, b := range members {
			if rel, ok := strings.CutPrefix(p, "data/store/"); ok {
				if err := writeFile(filepath.Join(manifest.DataDir(inst.Root, id), "store", filepath.FromSlash(rel)), b, 0o644); err != nil {
					return rollback(err)
				}
				n++
			}
		}
		if n > 0 {
			r.Changes = append(r.Changes, fmt.Sprintf("naps: %d files into agents/%s/data/store", n, id))
		}
		added, err := importSecrets(inst, id, unit, members["data/secrets.yaml"])
		if err != nil {
			return rollback(err)
		}
		r.Changes = append(r.Changes, added...)
	}
	if actionsBody != nil {
		path := filepath.Join(inst.Root, inst.BridgeActions)
		before, _ := os.ReadFile(path)
		if err := os.WriteFile(path, actionsBody, 0o644); err != nil {
			return rollback(err)
		}
		undo := rollback
		rollback = func(cause error) (*ImportResult, error) {
			_ = os.WriteFile(path, before, 0o644)
			return undo(cause)
		}
	}
	if check != nil {
		if err := check(id); err != nil {
			return rollback(refuse("invalid", "the imported agent does not check, nothing was kept: %v", err))
		}
	}
	if old != "" {
		_ = os.RemoveAll(old)
		r.Changes = append([]string{"replaced agents/" + id}, r.Changes...)
	} else {
		r.Changes = append([]string{"added agents/" + id}, r.Changes...)
	}
	return r, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func writeFile(p string, b []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, perm)
}

// retarget sets the staged agent.yaml's id and unit, and points its persona at this instance.
func retarget(p, id, unit string, m *Manifest) error {
	old, err := os.ReadFile(p)
	if err != nil {
		return refuse("invalid", "the export has no agent/agent.yaml")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(old, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return refuse("invalid", "agent/agent.yaml does not parse")
	}
	root := doc.Content[0]
	set := func(key, v string) {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				root.Content[i+1].Value = v
				return
			}
		}
	}
	set("id", id)
	set("unit", unit)
	if m.Persona != "" {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "persona" && root.Content[i+1].Kind == yaml.MappingNode {
				pm := root.Content[i+1]
				kept := []*yaml.Node{}
				for j := 0; j+1 < len(pm.Content); j += 2 {
					switch pm.Content[j].Value {
					case "repo":
						continue // the persona comes with the agent, into this instance
					case "path":
						pm.Content[j+1].Value = "personas/" + m.Persona
					}
					kept = append(kept, pm.Content[j], pm.Content[j+1])
				}
				pm.Content = kept
			}
		}
	}
	body, err := yamlfmt.Encode(&doc, old)
	if err != nil {
		return err
	}
	return os.WriteFile(p, body, 0o644)
}

// missingActions are the export's actions the instance lacks, and the instance's actions file
// with them appended (nil when none are missing).
func missingActions(inst *instance.Instance, exported []byte) ([]string, []byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(exported, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.SequenceNode {
		return nil, nil, refuse("invalid", "actions.yaml: not a list of actions")
	}
	have := map[string]bool{}
	var list *yaml.Node
	var old []byte
	var file yaml.Node
	if inst.BridgeActions != "" {
		var err error
		if old, err = os.ReadFile(filepath.Join(inst.Root, inst.BridgeActions)); err != nil && !os.IsNotExist(err) {
			return nil, nil, err
		}
		if len(old) > 0 {
			if err := yaml.Unmarshal(old, &file); err != nil {
				return nil, nil, err
			}
		}
		if len(file.Content) == 1 && file.Content[0].Kind == yaml.SequenceNode {
			list = file.Content[0]
			for _, item := range list.Content {
				have[actionName(item)] = true
			}
		}
	}
	missing := []string{}
	for _, item := range doc.Content[0].Content {
		if n := actionName(item); !have[n] {
			missing = append(missing, n)
			if list != nil {
				list.Content = append(list.Content, item)
			}
		}
	}
	if len(missing) == 0 {
		return nil, nil, nil
	}
	if inst.BridgeActions == "" {
		return missing, nil, refuse("missing_actions", "this instance has no bridge actions file (stormo.yaml bridge.actions) for %s", strings.Join(missing, ", "))
	}
	if list == nil {
		file = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{doc.Content[0]}}
	}
	body, err := yamlfmt.Encode(&file, old)
	return missing, body, err
}

// importSecrets writes the export's values into the agent's own layer (its folder), except a name
// the instance already resolves for it from a shared or unit value, which is left alone.
func importSecrets(inst *instance.Instance, id, unit string, body []byte) ([]string, error) {
	if body == nil {
		return nil, nil
	}
	var doc struct {
		Secrets map[string]string `yaml:"secrets"`
		Local   map[string]string `yaml:"local"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, refuse("invalid", "data/secrets.yaml: %v", err)
	}
	f, err := secrets.Load(secrets.Path(inst.Root))
	if err != nil {
		return nil, err
	}
	shared := func(name string) bool {
		if f.Shared != nil {
			if _, ok := f.Shared.Get(name); ok {
				return true
			}
		}
		if l := f.Units.Get(unit); l != nil {
			if _, ok := l.Get(name); ok {
				return true
			}
		}
		return false
	}
	f.MoveToFolder(id)
	if f.Agents == nil {
		f.Agents = secrets.NewNamed()
	}
	own := f.Agents.Ensure(id)
	set, kept := 0, []string{}
	for _, name := range sortedKeys(doc.Secrets) {
		if shared(name) {
			kept = append(kept, name)
			continue
		}
		own.Set(name, doc.Secrets[name])
		set++
	}
	if len(doc.Local) > 0 {
		if f.Local == nil {
			f.Local = &secrets.Layers{}
		}
		if f.Local.Agents == nil {
			f.Local.Agents = secrets.NewNamed()
		}
		l := f.Local.Agents.Ensure(id)
		for _, name := range sortedKeys(doc.Local) {
			l.Set(name, doc.Local[name])
			set++
		}
	}
	if err := secrets.Save(secrets.Path(inst.Root), inst.Names.Resource, f); err != nil {
		return nil, err
	}
	out := []string{fmt.Sprintf("secret values: %d into agents/%s/data/secrets.yaml", set, id)}
	if len(kept) > 0 {
		out = append(out, "secret values left to this instance's shared or unit layer: "+strings.Join(kept, ", "))
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
