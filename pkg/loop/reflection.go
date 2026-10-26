package loop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
)

// Reflection exports contain only procedures explicitly selected by an operator.
// Private candidates and transcripts never cross this interface. No confirmation
// is inferred from selection: these entries still need evidence or human review.
type reflectionExport struct {
	Version   int `json:"version"`
	Proposals []struct {
		ID        string   `json:"id"`
		Text      string   `json:"text"`
		EventRefs []string `json:"eventRefs"`
		Audience  string   `json:"audience"`
		Kind      string   `json:"kind"`
	} `json:"proposals"`
}

func ingestReflectionProposals(s Store, agent string, nap *Nap, eng engine.Engine, scrub *learning.Scrubber, ledger *[]*learning.Learning, report *DreamReport) error {
	provider, ok := eng.(engine.LearningProposals)
	if !ok {
		return nil
	}
	body, err := learningBlob(s, agent, nap, provider.LearningProposalsPath())
	if err != nil || body == nil {
		return err
	}
	texts, err := reflectionTexts(*body, scrub)
	if err != nil {
		return err
	}
	for _, text := range texts {
		_, created := learning.Upsert(ledger, learning.Sighting{Kind: learning.KindMemory, Text: text, Agent: agent, Unit: nap.Unit, Instance: nap.Instance, Nap: nap.ID, At: nap.TakenAt})
		if created {
			report.NewLearnings++
		} else {
			report.Resighted++
		}
	}
	return nil
}

func reflectionTexts(body string, scrub *learning.Scrubber) ([]string, error) {
	if len(body) > 512<<10 {
		return nil, fmt.Errorf("reflection export exceeds limit")
	}
	var export reflectionExport
	if err := json.Unmarshal([]byte(body), &export); err != nil {
		return nil, fmt.Errorf("invalid reflection export")
	}
	if export.Version != 1 || len(export.Proposals) > 100 {
		return nil, fmt.Errorf("unsupported reflection export version or size")
	}
	texts := []string{}
	seen := map[string]bool{}
	for _, p := range export.Proposals {
		if p.Audience != "agent" || p.Kind != "procedure" || len(p.ID) == 0 || len(p.ID) > 128 || len(p.EventRefs) > 20 {
			return nil, fmt.Errorf("invalid reflection proposal metadata")
		}
		text := strings.TrimSpace(p.Text)
		if text == "" || len(text) > 2000 {
			return nil, fmt.Errorf("invalid reflection proposal text length")
		}
		clean, flags := scrub.Scrub(text)
		if len(flags) > 0 {
			return nil, fmt.Errorf("reflection proposal requires private review")
		}
		key := learning.Normalize(clean)
		if !seen[key] {
			texts = append(texts, clean)
			seen[key] = true
		}
	}
	return texts, nil
}
