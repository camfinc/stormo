// Package capability holds the capability templates the engine ships (templates/<name>/): a worker
// image's build context plus what agents read about it (template.yaml, docs.md). An instance uses
// one with `template: <name>` under stormo.yaml capabilities:, and anything it sets there wins.
// Templates are generic: nothing in them names an organisation.
package capability

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed all:templates
var files embed.FS

// Template is a bundled worker: a service image the core boots on demand and puts to sleep when idle.
type Template struct {
	Name        string `yaml:"-" json:"name"`
	Description string `yaml:"description" json:"description"`
	// Docs is docs.md: how to call it, what it answers, its limits. Agents read it with cap_describe.
	Docs        string           `yaml:"-" json:"docs"`
	InputSchema map[string]any   `yaml:"input_schema" json:"inputSchema,omitempty"`
	Examples    []map[string]any `yaml:"examples" json:"examples,omitempty"`
	// Files: none | in | out | both (whether jobs take files from the shared space and give some back).
	Files          string            `yaml:"files" json:"files"`
	Port           int               `yaml:"port" json:"port"`
	Health         string            `yaml:"health" json:"health"`
	Concurrency    int               `yaml:"concurrency" json:"concurrency"`
	TimeoutMinutes int               `yaml:"timeout_minutes" json:"timeoutMinutes"`
	IdleMinutes    int               `yaml:"idle_minutes" json:"idleMinutes"`
	BootMinutes    int               `yaml:"boot_minutes" json:"bootMinutes"`
	MemoryMB       int               `yaml:"memory_mb" json:"memoryMB"`
	ShmMB          int               `yaml:"shm_mb" json:"shmMB"`
	Env            map[string]string `yaml:"env" json:"env,omitempty"`
	// Notes for the owner (licences, hardware), shown by `stormo capability templates`.
	Notes string `yaml:"notes" json:"notes,omitempty"`
}

// Templates are the bundled templates, by name.
func Templates() []Template {
	entries, err := fs.ReadDir(files, "templates")
	if err != nil {
		panic(err)
	}
	out := []Template{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := load(e.Name())
		if err != nil {
			panic(err) // a bundled template that does not parse is a build bug (the tests load them all)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds a bundled template.
func Lookup(name string) (Template, bool) {
	for _, t := range Templates() {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// Names are the bundled templates' names.
func Names() []string {
	out := []string{}
	for _, t := range Templates() {
		out = append(out, t.Name)
	}
	return out
}

func load(name string) (Template, error) {
	var t Template
	raw, err := files.ReadFile(path.Join("templates", name, "template.yaml"))
	if err != nil {
		return t, err
	}
	if err := yaml.Unmarshal(raw, &t); err != nil {
		return t, fmt.Errorf("template %s: %w", name, err)
	}
	t.Name = name
	if docs, err := files.ReadFile(path.Join("templates", name, "docs.md")); err == nil {
		t.Docs = strings.TrimSpace(string(docs))
	}
	return t, nil
}

// Context is a template's build context (its Dockerfile at the top).
func Context(name string) (fs.FS, error) {
	if _, ok := Lookup(name); !ok {
		return nil, fmt.Errorf("no capability template %q (%s)", name, strings.Join(Names(), ", "))
	}
	return fs.Sub(files, path.Join("templates", name))
}

// Image is the local tag of a template's image: its name and the hash of its build context, so a
// new engine with a changed template builds a new image and an unchanged one reuses it.
func Image(name string) string {
	ctx, err := Context(name)
	if err != nil {
		return ""
	}
	h := sha256.New()
	_ = fs.WalkDir(ctx, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(ctx, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	return "stormo-cap-" + name + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}

// ImageExists reports whether docker has the tag.
func ImageExists(tag string) bool {
	return exec.Command("docker", "image", "inspect", tag).Run() == nil
}

// Build builds a template's image (docker's cache keeps a rebuild quick) and gives its tag. The
// context is written to a temporary directory: a release binary carries its templates.
func Build(name string, out func(string)) (string, error) {
	ctx, err := Context(name)
	if err != nil {
		return "", err
	}
	tag := Image(name)
	dir, err := os.MkdirTemp("", "stormo-cap-"+name+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.CopyFS(dir, ctx); err != nil {
		return "", err
	}
	if out != nil {
		out("building " + tag)
	}
	cmd := exec.Command("docker", "build", "-t", tag, "--label", "stormo.template="+name, dir)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build %s: %w", tag, err)
	}
	return tag, nil
}

// EnsureImage builds a template's image when docker does not have it.
func EnsureImage(name string, out func(string)) (string, error) {
	if tag := Image(name); tag != "" && ImageExists(tag) {
		return tag, nil
	}
	return Build(name, out)
}
