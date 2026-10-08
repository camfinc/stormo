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
