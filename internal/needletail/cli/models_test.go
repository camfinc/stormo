package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/secrets"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// A job's model resolves as the agent's does: on the core, the gateway first when it serves the
// model, the named provider (else the agent's own connection) as the fallback; off the core, the
// connection alone. An unknown or unkeyed provider is refused.
func TestJobModels(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"gpt-6"},{"id":"gpt-6-mini"}]}`)
	}))
	defer gw.Close()
	cfg := &config.Config{}
	cfg.Core = &wire.CoreConfig{KeyEnv: "SWARM_CORE_KEY"}
	cfg.Model = wire.ModelConfig{
		Primary:   wire.Endpoint{BaseURL: gw.URL, KeyEnv: "SWARM_CORE_KEY", Model: "gpt-6"},
		Fallbacks: []wire.Endpoint{{BaseURL: "https://openrouter.example/api/v1", KeyEnv: "OPENROUTER_API_KEY", Model: "openai/gpt-6-luna"}},
		Providers: []wire.Provider{{Name: "openrouter", BaseURL: "https://openrouter.example/api/v1", KeyEnv: "OPENROUTER_API_KEY"},
			{Name: "other", BaseURL: "https://other.example/v1", KeyEnv: "OTHER_API_KEY"},
			{Name: "unkeyed", BaseURL: "https://unkeyed.example/v1", KeyEnv: "UNKEYED_API_KEY"}},
	}
	environ := []string{"SWARM_CORE_KEY=core-key-0123456789", "OPENROUTER_API_KEY=or-key-0123456789", "OTHER_API_KEY=other-key-0123456789"}
	sec := secrets.Load(&cfg.Config, environ)
	base := &llm.Client{Endpoints: []llm.Endpoint{{BaseURL: gw.URL, Key: "core-key-0123456789", Model: "gpt-6"}}, Reasoning: "low"}
	resolve := jobModels(cfg, sec, base)
	eps := func(c *llm.Client) string {
		var out []string
		for _, e := range c.Endpoints {
			out = append(out, e.BaseURL+"|"+e.Key+"|"+e.Model)
		}
		return strings.Join(out, " ; ")
	}
	ctx := context.Background()
	c, err := resolve(ctx, "gpt-6-mini", "")
	if err != nil || eps(c) != gw.URL+"|core-key-0123456789|gpt-6-mini ; https://openrouter.example/api/v1|or-key-0123456789|gpt-6-mini" || c.Reasoning != "low" {
		t.Errorf("served on the core: %v %s", err, eps(c))
	}
	if c, err = resolve(ctx, "anthropic/claude-x", "other"); err != nil || eps(c) != "https://other.example/v1|other-key-0123456789|anthropic/claude-x" {
		t.Errorf("not served, a named provider: %v %s", err, eps(c))
	}
	if c, err = resolve(ctx, "", "other"); err != nil || eps(c) != "https://other.example/v1|other-key-0123456789|openai/gpt-6-luna" {
		t.Errorf("provider alone (the agent's model): %v %s", err, eps(c))
	}
	if _, err = resolve(ctx, "m", "nowhere"); err == nil || !strings.Contains(err.Error(), `"nowhere"`) {
		t.Errorf("unknown provider: %v", err)
	}
	if _, err = resolve(ctx, "m", "unkeyed"); err == nil || !strings.Contains(err.Error(), "UNKEYED_API_KEY") || strings.Contains(err.Error(), "key-0123") {
		t.Errorf("unkeyed provider: %v", err)
	}
	// off the core: the connection alone
	aws := &config.Config{}
	aws.Model = wire.ModelConfig{Primary: wire.Endpoint{BaseURL: "https://openrouter.example/api/v1", KeyEnv: "OPENROUTER_API_KEY", Model: "openai/gpt-6-luna"}}
	sec = secrets.Load(&aws.Config, environ)
	if c, err = jobModels(aws, sec, &llm.Client{})(ctx, "gpt-6-mini", ""); err != nil || eps(c) != "https://openrouter.example/api/v1|or-key-0123456789|gpt-6-mini" {
		t.Errorf("off the core: %v %s", err, eps(c))
	}
}
