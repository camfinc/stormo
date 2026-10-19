package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/link"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/version"
)

// startLink runs the agent's link to its core next to the nap loop, when the sidecar is given a
// core (SWARM_CORE_URL and SWARM_CORE_KEY): the relay the engine's hooks and MCP calls go to, and
// the live wire for status, wakes and naps (pkg/link, docs/remote-core.md §4).
func startLink(a *manifest.Agent, eng engine.Engine, sc *sidecar, o loop.NapOptions) {
	coreURL, key := os.Getenv("SWARM_CORE_URL"), os.Getenv("SWARM_CORE_KEY")
	if coreURL == "" || key == "" {
		return
	}
	api := os.Getenv("SWARM_AGENT_API")
	if api == "" {
		api = "http://localhost:8642"
	}
	ag := link.NewAgent(link.AgentOptions{
		Agent: a.ID, Placement: os.Getenv("SWARM_PLACEMENT"), Engine: a.Engine.Kind, Version: version.String(),
		CoreURL: coreURL, CoreKey: key, AgentAPI: api, APIKey: os.Getenv("SWARM_AGENT_API_KEY"), Runtime: eng.Runtime(),
		// Never under the home: naps snapshot it, and held hook bodies can carry client data.
		Spool: &link.Spool{Dir: filepath.Join(linkDir(), "spool")},
		Nap: func(reason string) error {
			n := o
			n.Reason = reason
			_, err := loop.NapOnce(n)
			return err
		},
	})
	go func() {
		if err := ag.Run(context.Background()); err != nil {
			log.Printf("[link] %v", err)
		}
	}()
}

// linkDir is the link's own state (SWARM_LINK_DIR, a volume of its own locally), else a temp dir.
func linkDir() string {
	if d := os.Getenv("SWARM_LINK_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "stormo-link")
}
