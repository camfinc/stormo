// Package config reads and replaces an instance's configuration files for programs that edit them
// (the macOS app; docs/api.md, `config`). A file is shown with the hash of its bytes and written
// back whole, only if it still has the hash the editor loaded and the new content validates as
// that file, so a broken or stale edit never replaces what is on disk.
//
// Supported so far: agents/<id>/agent.yaml (validated like `stormo check` validates the manifest
// and its bridge actions; deploy: is read-only, it names cloud resources) and agents/<id>/SOUL.md.
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
)

// File is one configuration file as an editor sees it.
type File struct {
	Path  string `json:"path"`  // relative to the instance, with forward slashes
	Kind  string `json:"kind"`  // "agent" (agent.yaml) or "soul" (SOUL.md)
	Agent string `json:"agent"` // the agent the file belongs to
	Hash  string `json:"hash"`  // sha256 of the bytes, hex: pass it back to Write
	Text  string `json:"text"`
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
	return &File{Path: t.rel, Kind: t.kind, Agent: t.agent, Hash: Hash(b), Text: string(b)}, nil
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
	return &File{Path: t.rel, Kind: t.kind, Agent: t.agent, Hash: Hash(body), Text: string(body)}, nil
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
