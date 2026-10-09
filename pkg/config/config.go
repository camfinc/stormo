// Package config reads and replaces an instance's configuration files for programs that edit them
// (the macOS app; docs/api.md, `config`). A file is shown with the hash of its bytes and written
// back whole, only if it still has the hash the editor loaded and the new content validates as
// that file, so a broken or stale edit never replaces what is on disk.
//
// Supported so far: agents/<id>/agent.yaml (validated like `stormo check` validates the manifest
// and its bridge actions; deploy: is read-only, it names cloud resources) and agents/<id>/SOUL.md.
// An agent.yaml can also be changed field by field (Apply, a JSON merge patch); Show then carries
// the parsed document and the choices a form offers (Options).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/pkg/bridge"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/yamlfmt"
)

// File is one configuration file as an editor sees it.
type File struct {
	Path  string `json:"path"`  // relative to the instance, with forward slashes
	Kind  string `json:"kind"`  // "agent" (agent.yaml) or "soul" (SOUL.md)
	Agent string `json:"agent"` // the agent the file belongs to
	Hash  string `json:"hash"`  // sha256 of the bytes, hex: pass it back to Write
	Text  string `json:"text"`
	// agent.yaml only: the document as JSON (null when it does not parse) and the form's choices.
	Doc     any      `json:"doc,omitempty"`
	Options *Options `json:"options,omitempty"`
}

// Error is a refusal with a stable code for --json (docs/api.md): unsupported, conflict, invalid,
// readonly.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func refuse(code, format string, a ...any) error {
	return &Error{code, fmt.Sprintf(format, a...)}
}

// Hash is the hash File and Write use.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type target struct {
	rel, kind, agent string
}

func resolve(inst *instance.Instance, rel string) (target, error) {
	clean := path.Clean(filepath.ToSlash(rel))
	parts := strings.Split(clean, "/")
	if !filepath.IsAbs(rel) && len(parts) == 3 && parts[0] == "agents" && slices.Contains(manifest.AgentIDs(inst.Root), parts[1]) {
		switch parts[2] {
		case "agent.yaml":
			return target{clean, "agent", parts[1]}, nil
		case "SOUL.md":
			return target{clean, "soul", parts[1]}, nil
		}
	}
	return target{}, refuse("unsupported", "%s: not a configuration file stormo edits (agents/<id>/agent.yaml or agents/<id>/SOUL.md)", rel)
}

func (t target) abs(inst *instance.Instance) string {
	return filepath.Join(inst.Root, filepath.FromSlash(t.rel))
}

// Show reads one configuration file.
func Show(inst *instance.Instance, rel string) (*File, error) {
	t, err := resolve(inst, rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(t.abs(inst))
	if err != nil {
		return nil, err
	}
	return file(inst, t, b), nil
}

func file(inst *instance.Instance, t target, b []byte) *File {
	f := &File{Path: t.rel, Kind: t.kind, Agent: t.agent, Hash: Hash(b), Text: string(b)}
	if t.kind == "agent" {
		var doc map[string]any
		if yaml.Unmarshal(b, &doc) == nil {
			f.Doc = doc
		}
		f.Options = options(inst, t.agent)
	}
	return f
}

// Write replaces one configuration file with body if the file still hashes to ifHash and body
// validates; the replacement is atomic (a temporary file renamed over it).
func Write(inst *instance.Instance, rel string, body []byte, ifHash string) (*File, error) {
	t, err := resolve(inst, rel)
	if err != nil {
		return nil, err
	}
	if ifHash == "" {
		return nil, refuse("usage", "%s: --if-hash is required (the hash `config show` gave)", t.rel)
	}
	p := t.abs(inst)
	old, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if Hash(old) != ifHash {
		return nil, refuse("conflict", "%s changed on disk since it was loaded; reload it and edit again", t.rel)
	}
	if err := validate(inst, t, old, body); err != nil {
		return nil, err
	}
	if !bytes.Equal(old, body) {
		if err := replace(p, body); err != nil {
			return nil, err
		}
	}
	return file(inst, t, body), nil
}

// Apply changes an agent.yaml by a JSON merge patch (RFC 7386): the file's node tree is edited, so
// comments, key order, blank lines and comment alignment stay where nothing changed. The result is
// validated and written as Write does.
func Apply(inst *instance.Instance, rel string, patch []byte, ifHash string) (*File, error) {
	t, err := resolve(inst, rel)
	if err != nil {
		return nil, err
	}
	if t.kind != "agent" {
		return nil, refuse("unsupported", "%s: only agent.yaml takes a patch; write the whole file", t.rel)
	}
	var p yaml.Node
	if err := yaml.Unmarshal(patch, &p); err != nil || len(p.Content) != 1 || p.Content[0].Kind != yaml.MappingNode {
		return nil, refuse("usage", "%s: the patch must be a JSON object", t.rel)
	}
	for _, key := range []string{"id", "deploy", "format"} {
		if keyIndex(p.Content[0], key) >= 0 {
			return nil, refuse("readonly", "%s: %s: is not edited here", t.rel, key)
		}
	}
	if ifHash == "" {
		return nil, refuse("usage", "%s: --if-hash is required (the hash `config show` gave)", t.rel)
	}
	old, err := os.ReadFile(t.abs(inst))
	if err != nil {
		return nil, err
	}
	if Hash(old) != ifHash {
		return nil, refuse("conflict", "%s changed on disk since it was loaded; reload it and edit again", t.rel)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(old, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, refuse("invalid", "%s does not parse as a YAML mapping; fix it as text first", t.rel)
	}
	applyPatch(doc.Content[0], p.Content[0])
	body, err := yamlfmt.Encode(&doc, old)
	if err != nil {
		return nil, err
	}
	return Write(inst, rel, body, ifHash)
}

func validate(inst *instance.Instance, t target, old, body []byte) error {
	switch t.kind {
	case "soul":
		if len(bytes.TrimSpace(body)) == 0 {
			return refuse("invalid", "%s: the agent's behaviour cannot be empty", t.rel)
		}
	case "agent":
		a, err := manifest.Parse(inst.Root, t.agent, inst.Names.Secret, body)
		if err != nil {
			return refuse("invalid", "%v", err)
		}
		registry, err := bridge.Load(inst)
		if err != nil {
			return err
		}
		if _, err := bridge.ActionsFor(a, registry); err != nil {
			return refuse("invalid", "%s: %v", t.rel, err)
		}
		// A file that does not parse now is being repaired: nothing to compare against.
		if was, err := deploy(old); err == nil {
			if now, _ := deploy(body); !reflect.DeepEqual(was, now) {
				return refuse("readonly", "%s: deploy: names cloud resources and is not edited here; change it in the file itself", t.rel)
			}
		}
	}
	return nil
}

func deploy(body []byte) (any, error) {
	var m struct {
		Deploy any `yaml:"deploy"`
	}
	err := yaml.Unmarshal(body, &m)
	return m.Deploy, err
}

func replace(p string, body []byte) error {
	info, err := os.Stat(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
