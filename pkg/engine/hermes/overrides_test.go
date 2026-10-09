package hermes

import (
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/env"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

var openrouter = instance.Connection{Name: "openrouter", Kind: instance.KindOpenRouter, BaseURL: "https://openrouter.ai/api/v1", Key: "OPENROUTER_API_KEY"}

func overridden(t *testing.T, base string, local bool, target manifest.Target) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(base), &doc); err != nil {
		t.Fatal(err)
	}
	a := &manifest.Agent{ID: "atlas", Unit: "sales"}
	a.Engine.Model, a.Engine.Provider = "openai/gpt-6-luna", "openrouter"
	if local {
		a.Engine.Local = &manifest.EngineLocal{Via: "core", Model: "gpt-6-luna"}
	}
	if err := ApplyOverrides(doc.Content[0], a, openrouter, target); err != nil {
		t.Fatal(err)
	}
	return doc.Content[0]
}

func TestCoreHooksOnlyLocallyOnTheCore(t *testing.T) {
	base := "hooks_auto_accept: false\nhooks:\n  outbound:\n    - name: audit\n      url: https://audit.example/hook\n      events: [post_tool_call]\n    - name: swarm-core\n      url: http://old/ingest\n      events: [pre_tool_call]\n"
	cfg := overridden(t, base, true, manifest.Local)
	list := getPath(cfg, "hooks.outbound")
	if list == nil || len(list.Content) != 2 {
		t.Fatalf("want the instance's own target kept and one core target, got %v", list)
	}
	if n := getPath(list.Content[0], "name"); n.Value != "audit" {
		t.Errorf("first target %q", n.Value)
	}
	core := list.Content[1]
	for k, want := range map[string]string{"name": CoreHookName, "url": manifest.Core.IngestURL + "/hermes", "secret_env": manifest.Core.KeyEnv, "timeout": "5"} {
		if n := getPath(core, k); n == nil || n.Value != want {
			t.Errorf("core target %s = %v, want %s", k, n, want)
		}
	}
	events := []string{}
	for _, e := range getPath(core, "events").Content {
		events = append(events, e.Value)
	}
	if strings.Join(events, ",") != strings.Join(CoreHookEvents, ",") {
		t.Errorf("events %v", events)
	}
	if strings.Contains(strings.Join(events, ","), "llm") {
		t.Error("LLM hooks carry whole conversations; the core must not subscribe to them")
	}

	// AWS builds and agents that do not go through the core get no core target.
	for name, cfg := range map[string]*yaml.Node{"aws": overridden(t, base, true, manifest.AWS), "no core": overridden(t, base, false, manifest.Local)} {
		for _, tgt := range getPath(cfg, "hooks.outbound").Content {
			if n := getPath(tgt, "name"); n.Value == CoreHookName && getPath(tgt, "url").Value == manifest.Core.IngestURL+"/hermes" {
				t.Errorf("%s: core hook target present", name)
			}
		}
	}
}

func TestCoreMCPServerOnlyLocallyOnTheCore(t *testing.T) {
	cfg := overridden(t, "mcp_servers:\n  docs:\n    url: https://docs.example/mcp\n", true, manifest.Local)
	if getPath(cfg, "mcp_servers.docs.url") == nil {
		t.Error("the instance's own MCP servers are kept")
	}
	if n := getPath(cfg, "mcp_servers.core.url"); n == nil || n.Value != manifest.Core.MCPURL {
		t.Fatalf("core url %v", n)
	}
	// The key itself never lands in config.yaml: Hermes expands the placeholder from the env.
	if n := getPath(cfg, "mcp_servers.core.headers.Authorization"); n == nil || n.Value != "Bearer ${SWARM_CORE_KEY}" {
		t.Errorf("auth header %v", n)
	}
	for name, cfg := range map[string]*yaml.Node{"aws": overridden(t, "{}", true, manifest.AWS), "no core": overridden(t, "{}", false, manifest.Local)} {
		if getPath(cfg, "mcp_servers.core") != nil {
			t.Errorf("%s: core MCP server present", name)
		}
	}
}

func TestWorkdirHelperEnvOnlyLocallyOnTheCore(t *testing.T) {
	h := &Hermes{}
	a := &manifest.Agent{ID: "atlas", Unit: "sales", Env: env.New()}
	a.Engine.Local = &manifest.EngineLocal{Via: "core", Model: "gpt-6-luna"}
	if v, _ := h.Env(a, manifest.Local).Get("SWARM_CORE_URL"); v != manifest.Core.URL {
		t.Errorf("local SWARM_CORE_URL %q", v)
	}
	if _, ok := h.Env(a, manifest.AWS).Get("SWARM_CORE_URL"); ok {
		t.Error("AWS tasks never reach the core")
	}
	pass := func(cfg *yaml.Node) string {
		out := []string{}
		for _, n := range getPath(cfg, "terminal.env_passthrough").Content {
			out = append(out, n.Value)
		}
		return strings.Join(out, ",")
	}
	if got := pass(overridden(t, "{}", true, manifest.Local)); !strings.Contains(got, "SWARM_CORE_KEY") || !strings.Contains(got, "SWARM_CORE_URL") {
		t.Errorf("local passthrough %s", got)
	}
	if got := pass(overridden(t, "{}", true, manifest.AWS)); strings.Contains(got, "SWARM_CORE") {
		t.Errorf("AWS passthrough %s", got)
	}
}

func TestLimitsReachTheConfig(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("agent:\n  # kept\n  max_turns: 60\n  reasoning_effort: high\nterminal:\n  timeout: 180\n"), &doc); err != nil {
		t.Fatal(err)
	}
	a := &manifest.Agent{ID: "atlas", Unit: "sales", Limits: manifest.Limits{Turns: 80, ScriptTimeout: 900}}
	a.Engine.Model, a.Engine.Provider = "openai/gpt-6-luna", "openrouter"
	if err := ApplyOverrides(doc.Content[0], a, openrouter, manifest.AWS); err != nil {
		t.Fatal(err)
	}
	cfg := doc.Content[0]
	for path, want := range map[string]string{"agent.max_turns": "80", "agent.reasoning_effort": "high", "terminal.timeout": "180", "cron.script_timeout_seconds": "900"} {
		if n := getPath(cfg, path); n == nil || n.Value != want {
			t.Errorf("%s = %v, want %s", path, n, want)
		}
	}
}

func TestAnOpenAIConnectionLeavesNoOpenRouterBehind(t *testing.T) {
	base := "model:\n  default: x\n  provider: openrouter\n  base_url: https://openrouter.ai/api/v1\n  api_mode: chat_completions\n"
	conn := instance.Connection{Name: "openai", Kind: instance.KindOpenAI, BaseURL: "https://api.openai.com/v1", Key: "OPENAI_API_KEY"}
	for _, local := range []bool{false, true} {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(base), &doc); err != nil {
			t.Fatal(err)
		}
		a := &manifest.Agent{ID: "atlas", Unit: "sales"}
		a.Engine.Model, a.Engine.Provider = "gpt-6", "openai"
		target := manifest.AWS
		if local {
			a.Engine.Local, target = &manifest.EngineLocal{Via: "core", Model: "gpt-5.6-sol"}, manifest.Local
		}
		if err := ApplyOverrides(doc.Content[0], a, conn, target); err != nil {
			t.Fatal(err)
		}
		cfg := doc.Content[0]
		if !local {
			for path, want := range map[string]string{"model.provider": "custom", "model.base_url": "https://api.openai.com/v1", "model.key_env": "OPENAI_API_KEY", "model.default": "gpt-6"} {
				if n := getPath(cfg, path); n == nil || n.Value != want {
					t.Errorf("%s = %v, want %s", path, n, want)
				}
			}
			continue
		}
		// On the core the cloud model is the fallback, with its own key.
		fb := getPath(cfg, "fallback_providers")
		if fb == nil || len(fb.Content) == 0 {
			t.Fatal("no fallback")
		}
		for k, want := range map[string]string{"provider": "custom", "model": "gpt-6", "base_url": "https://api.openai.com/v1", "key_env": "OPENAI_API_KEY"} {
			if n := getPath(fb.Content[0], k); n == nil || n.Value != want {
				t.Errorf("fallback %s = %v, want %s", k, n, want)
			}
		}
	}
}
