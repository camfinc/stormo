package loop

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Options.LLM runs the turn on another client (a job's model): every request of the turn goes to
// it, none to the agent's own.
func TestTurnOnAnotherModel(t *testing.T) {
	own := &stub{pick: func(map[string]any) string { return say("own model") }}
	ownSrv := httptest.NewServer(own)
	defer ownSrv.Close()
	other := &stub{pick: func(map[string]any) string { return say("pinned model") }}
	otherSrv := httptest.NewServer(other)
	defer otherSrv.Close()
	a := newAgent(t, ownSrv)
	h, err := a.Store.Create(wire.Session{ID: "cron-x-1", Source: "cron"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), h.ID, "Report.", Options{Job: true,
		LLM: &llm.Client{Endpoints: []llm.Endpoint{{BaseURL: otherSrv.URL, Key: "k2", Model: "gpt-pinned"}}}})
	if err != nil || res.Text != "pinned model" {
		t.Fatalf("%v %+v", err, res)
	}
	if len(own.requests) != 0 || len(other.requests) != 1 || other.requests[0]["model"] != "gpt-pinned" {
		t.Errorf("own %d, other %v", len(own.requests), other.requests)
	}
}
