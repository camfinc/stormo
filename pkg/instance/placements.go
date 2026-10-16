package instance

import (
	"fmt"
	"regexp"
	"sort"
)

// Placement is somewhere an instance's agents run (docs/remote-core.md §6a), and the deployment
// adapter that runs them there:
//   - local: docker compose on the core's own host (implicit);
//   - remote: ECS through the AWS API (implicit; today's `-r`);
//   - runner: any other host (a VPS…), where `stormo runner --placement <name>` runs them and
//     dials the core for its orders.
type Placement struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Placement kinds.
const (
	PlacementLocal  = "local"
	PlacementECS    = "ecs"
	PlacementRunner = "runner"
)

var placementName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func parsePlacements(raw map[string]struct {
	Kind string `yaml:"kind"`
}) ([]Placement, error) {
	out := []Placement{{Name: "local", Kind: PlacementLocal}, {Name: "remote", Kind: PlacementECS}}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := raw[n]
		switch {
		case n == "local" || n == "remote":
			return nil, fmt.Errorf("placements.%s is built in", n)
		case !placementName.MatchString(n):
			return nil, fmt.Errorf("placement name %q: lowercase letters, digits, hyphens", n)
		case p.Kind != PlacementRunner:
			return nil, fmt.Errorf("placements.%s.kind is runner (local and remote are built in)", n)
		}
		out = append(out, Placement{Name: n, Kind: p.Kind})
	}
	return out, nil
}

// PlacementNamed finds a placement by name.
func (i *Instance) PlacementNamed(name string) (Placement, bool) {
	ps := i.Placements
	if ps == nil {
		ps = []Placement{{Name: "local", Kind: PlacementLocal}, {Name: "remote", Kind: PlacementECS}}
	}
	for _, p := range ps {
		if p.Name == name {
			return p, true
		}
	}
	return Placement{}, false
}
