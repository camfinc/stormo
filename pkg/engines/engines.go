// Package engines is the engine registry. A second engine is a new package implementing
// engine.Engine plus one line here; manifests pick it with `engine.kind`.
package engines

import (
	"fmt"
	"sort"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
)

var registry = map[string]func(*instance.Instance) engine.Engine{
	"hermes": func(i *instance.Instance) engine.Engine { return hermes.New(i) },
}

// runtimes are the engines' runtimes without an instance (the core's hook ingest, secrets).
var runtimes = map[string]func() engine.Runtime{
	"hermes": hermes.NewRuntime,
}

// Runtime is how Stormo talks to a running agent of engine kind.
func Runtime(kind string) (engine.Runtime, error) {
	f, ok := runtimes[kind]
	if !ok {
		return nil, fmt.Errorf("unknown engine %q (known: %v)", kind, Kinds())
	}
	return f(), nil
}

// APIKeyNames are the engines' API key secrets (per agent by design; secrets init mints them).
func APIKeyNames() []string {
	out := []string{}
	for _, k := range Kinds() {
		out = append(out, runtimes[k]().APIKeyName())
	}
	return out
}

// Get returns the engine for kind, bound to the instance.
func Get(kind string, inst *instance.Instance) (engine.Engine, error) {
	f, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown engine %q (known: %v)", kind, Kinds())
	}
	return f(inst), nil
}

func Kinds() []string {
	out := []string{}
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
