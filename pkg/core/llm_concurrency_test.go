package core

import (
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

// The gateway's concurrency: SWARM_CORE_LLM_CONCURRENCY (an operator's override) first, then
// stormo.yaml's core.llm_concurrency, else 0 (the gateway's default, 3).
func TestLLMConcurrency(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	inst := &instance.Instance{}
	if n := llmConcurrency(inst, env("")); n != 0 {
		t.Errorf("unset = %d", n)
	}
	inst.CoreLLMConcurrency = 5
	if n := llmConcurrency(inst, env("")); n != 5 {
		t.Errorf("stormo.yaml = %d", n)
	}
	if n := llmConcurrency(inst, env("8")); n != 8 {
		t.Errorf("env over stormo.yaml = %d", n)
	}
	if n := llmConcurrency(inst, env("lots")); n != 5 {
		t.Errorf("bad env = %d", n)
	}
}
