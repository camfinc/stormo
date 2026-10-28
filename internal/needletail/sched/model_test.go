package sched

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/llm"
)

// A job's model and provider are applied for its run: the scheduler asks Options.Model for the
// run's client and gives it to the turn; one that cannot be resolved (an unknown provider) is the
// run's error, with no turn.
func TestJobModelOverride(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	past := `"2026-10-21T08:00:00Z"`
	hourly := `{"kind":"interval","minutes":60,"display":"every 60m"}`
	pinned := strings.Replace(jobJSON("pinned", hourly, past), `"prompt"`, `"model":"gpt-x","provider":"openrouter","prompt"`, 1)
	unknown := strings.Replace(jobJSON("unknown", hourly, past), `"prompt"`, `"provider":"nowhere","prompt"`, 1)
	plain := jobJSON("plain", hourly, past)
	r := bareRig(t, t0, pinned, unknown, plain)
	special := &llm.Client{}
	var asked []string
	r.s.o.Model = func(_ context.Context, model, provider string) (*llm.Client, error) {
		asked = append(asked, model+"@"+provider)
		if provider == "nowhere" {
			return nil, errors.New(`provider "nowhere" is not configured for this agent`)
		}
		return special, nil
	}
	r.turns.reply("pinned", "pinned report")
	r.turns.reply("plain", "plain report")
	r.tick()
	calls := map[string]turnCall{}
	for _, c := range r.turns.taken() {
		calls[c.job] = c
	}
	if c, ok := calls["pinned"]; !ok || c.llm != special {
		t.Errorf("the pinned job's turn did not get its model's client: %+v", c)
	}
	if c, ok := calls["plain"]; !ok || c.llm != nil {
		t.Errorf("a job without a model got a client: %+v", c)
	}
	if _, ok := calls["unknown"]; ok {
		t.Error("an unresolvable provider still took a turn")
	}
	if st := r.state("unknown"); !strings.Contains(st, "error") || !strings.Contains(st, "nowhere") {
		t.Errorf("unknown provider: %s", st)
	}
	if strings.Join(asked, ",") == "" || strings.Contains(strings.Join(asked, ","), "@\"") {
		t.Errorf("asked %v", asked)
	}
}
