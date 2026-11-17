package main

import (
	"slices"
	"strings"
	"testing"
)

// connections lists the tools APIs every instance has: keyed once in the shared secrets, used by
// the agents that declare their key; asking one for models is refused before anything is called.
func TestToolsConnections(t *testing.T) {
	inst := voiceInstance(t, "http://127.0.0.1:1", "")
	rows, err := connectionRows(inst)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]connectionRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	apify, comfy := byName["apify"], byName["comfy"]
	if apify.Serves != "tools" || !apify.API || !apify.Implicit || apify.Key != "APIFY_API_TOKEN" || apify.KeySet || !slices.Equal(apify.UsedBy, []string{"atlas"}) {
		t.Errorf("apify = %+v", apify)
	}
	if comfy.Serves != "tools" || comfy.Key != "COMFY_API_KEY" || len(comfy.UsedBy) != 0 || !strings.HasPrefix(comfy.Label, "Comfy Cloud") {
		t.Errorf("comfy = %+v", comfy)
	}
	// web_search's providers: implicit too, keyed by the names the runtime reads.
	if tv, br := byName["tavily"], byName["brave"]; tv.Serves != "tools" || !tv.Implicit || tv.Key != "TAVILY_API_KEY" || br.Serves != "tools" || br.Key != "BRAVE_SEARCH_API_KEY" {
		t.Errorf("tavily = %+v, brave = %+v", tv, br)
	}
	if _, err := connectionModels(inst, "apify", ""); err == nil || !strings.Contains(err.Error(), "serves tools, not models") {
		t.Errorf("models of a tools API: %v", err)
	}
	if _, err := voiceClient(inst, "comfy", ""); err == nil || !strings.Contains(err.Error(), "serves tools, not voice") {
		t.Errorf("voices of a tools API: %v", err)
	}
}
