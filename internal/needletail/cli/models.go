package cli

import (
	"context"
	"fmt"

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/secrets"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// jobModels resolves a scheduled job's model and provider (docs/schedules.md) as the agent's own
// model resolves (pkg/engine/needletail renderConfig): on the core, its gateway first when it
// serves the model (its GET /models lists it), and the job's provider (else the agent's own
// connection) as the fallback; off the core, that connection alone. A provider is one of
// needletail.yaml's model.providers (the instance's API connections the agent's schedules name)
// with its key set; anything else is refused with why. An empty model is the agent's own model
// (its connection's); the client keeps the agent's reasoning, vision and logging.
func jobModels(cfg *config.Config, sec *secrets.Secrets, base *llm.Client) func(ctx context.Context, model, provider string) (*llm.Client, error) {
	onCore := cfg.Core != nil && cfg.Model.Primary.KeyEnv == cfg.Core.KeyEnv
	cloud := cfg.Model.Primary
	if onCore {
		cloud = wire.Endpoint{}
		if len(cfg.Model.Fallbacks) > 0 {
			cloud = cfg.Model.Fallbacks[0]
		}
	}
	return func(ctx context.Context, model, provider string) (*llm.Client, error) {
		ep := llm.Endpoint{BaseURL: cloud.BaseURL, Key: sec.Get(cloud.KeyEnv), Model: model}
		if model == "" {
			ep.Model = cloud.Model
		}
		if provider != "" {
			var p *wire.Provider
			for i := range cfg.Model.Providers {
				if cfg.Model.Providers[i].Name == provider {
					p = &cfg.Model.Providers[i]
				}
			}
			if p == nil {
				return nil, fmt.Errorf("provider %q is not configured for this agent (a job may name the agent's own provider or one agent.yaml's schedules name: needletail.yaml model.providers)", provider)
			}
			key := sec.Get(p.KeyEnv)
			if key == "" {
				return nil, fmt.Errorf("provider %q has no key: %s is not set", provider, p.KeyEnv)
			}
			ep.BaseURL, ep.Key = p.BaseURL, key
		}
		var eps []llm.Endpoint
		if onCore && len(base.Endpoints) > 0 && base.Serves(ctx, 0, ep.Model) {
			gw := base.Endpoints[0]
			gw.Model = ep.Model
			eps = append(eps, gw)
		}
		if ep.BaseURL != "" {
			eps = append(eps, ep)
		}
		if len(eps) == 0 {
			return nil, fmt.Errorf("model %q is not served by the core and the agent has no connection to fall back to", ep.Model)
		}
		return &llm.Client{Endpoints: eps, Reasoning: base.Reasoning, HTTP: base.HTTP, Log: base.Log, Vision: base.Vision,
			DefaultContext: base.DefaultContext}, nil
	}
}
