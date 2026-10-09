package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, yaml string) (*Instance, error) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, File), []byte("slug: acme\n"+yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(root)
}

func TestConnections(t *testing.T) {
	// Without connections: the two every instance had.
	inst, err := load(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if or, ok := inst.Connection("openrouter"); !ok || !or.API() || or.Key != "OPENROUTER_API_KEY" || !or.Implicit {
		t.Errorf("openrouter = %+v", or)
	}
	if c, ok := inst.Connection(DefaultChatGPT); !ok || c.API() {
		t.Errorf("chatgpt = %+v", c)
	}
	// Adding one keeps them (an agent saying provider: openrouter keeps working); a kind's defaults fill in.
	inst, err = load(t, "connections:\n  - {name: work-gpt, kind: chatgpt}\n  - {name: openai, kind: openai}\n  - {name: local-llm, kind: custom, base_url: http://localhost:11434/v1, key: OLLAMA_API_KEY}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.Connections) != 5 {
		t.Errorf("connections = %+v", inst.Connections)
	}
	if c, _ := inst.Connection("openai"); c.BaseURL != "https://api.openai.com/v1" || c.Key != "OPENAI_API_KEY" || c.Implicit {
		t.Errorf("openai = %+v", c)
	}
	if _, ok := inst.Connection("openrouter"); !ok {
		t.Error("the implicit openrouter is gone")
	}
	// Redefining an implicit one by name replaces it.
	inst, _ = load(t, "connections:\n  - {name: openrouter, kind: openrouter, key: OR_TEAM_KEY}\n")
	if c, _ := inst.Connection("openrouter"); c.Key != "OR_TEAM_KEY" || c.Implicit || len(inst.Connections) != 2 {
		t.Errorf("redefined = %+v", inst.Connections)
	}
	for body, want := range map[string]string{
		"connections:\n  - {name: x, kind: gemini}\n":                              "kind must be one of",
		"connections:\n  - {name: X, kind: openai}\n":                              "lowercase id",
		"connections:\n  - {name: x, kind: custom, key: K}\n":                      "base_url is required",
		"connections:\n  - {name: x, kind: openai, key: lower}\n":                  "env var NAME",
		"connections:\n  - {name: x, kind: chatgpt, key: K}\n":                     "no base_url or key",
		"connections:\n  - {name: x, kind: openai}\n  - {name: x, kind: openai}\n": "defined twice",
	} {
		if _, err := load(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", body, want, err)
		}
	}
}
