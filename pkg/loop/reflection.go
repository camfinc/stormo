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

// ingestReflectionProposals takes the nap's reflection export (engine.LearningProposals) into the
// ledger as proposed memories. Only a store error fails the dream: an export or a proposal it
// cannot take is skipped and reported in the dream's notices, and a proposal the scrubber flags
// is kept scrubbed and flagged (held for a person, never promoted), like any flagged memory.
func ingestReflectionProposals(s Store, agent string, nap *Nap, eng engine.Engine, scrub *learning.Scrubber, ledger *[]*learning.Learning, report *DreamReport) error {
	provider, ok := eng.(engine.LearningProposals)
	if !ok {
		return nil
	}
	body, err := learningBlob(s, agent, nap, provider.LearningProposalsPath())
	if err != nil || body == nil {
		return err
	}
	proposals, notices := reflectionTexts(*body, scrub)
	for _, n := range notices {
		report.notice(nap.ID + ": " + n)
	}
	for _, p := range proposals {
		_, created := learning.Upsert(ledger, learning.Sighting{Kind: learning.KindMemory, Text: p.Text, PII: p.PII, Agent: agent, Unit: nap.Unit, Instance: nap.Instance, Nap: nap.ID, At: nap.TakenAt})
		if created {
			report.NewLearnings++
		} else {
			report.Resighted++
		}
	}
	return nil
}

// reflected is a proposal the dream takes: its scrubbed text and what the scrubber flagged.
type reflected struct {
	Text string
	PII  []string
}

// Bounds of a reflection export.
const (
	maxReflectionExport    = 512 << 10
	maxReflectionProposals = 100
	maxReflectionText      = 2000
)

// reflectionTexts reads a reflection export: the proposals the dream takes, and notices (no
// proposal text in them) for what it skipped or flagged. An export it cannot read is skipped whole.
func reflectionTexts(body string, scrub *learning.Scrubber) ([]reflected, []string) {
	if len(body) > maxReflectionExport {
		return nil, []string{fmt.Sprintf("reflection proposals: over %d KB, skipped", maxReflectionExport>>10)}
	}
	var export reflectionExport
	if err := json.Unmarshal([]byte(body), &export); err != nil {
		return nil, []string{"reflection proposals: not valid JSON, skipped"}
	}
	if export.Version != 1 {
		return nil, []string{fmt.Sprintf("reflection proposals: version %d is not one this build reads, skipped", export.Version)}
	}
	if len(export.Proposals) > maxReflectionProposals {
		return nil, []string{fmt.Sprintf("reflection proposals: %d, more than %d, skipped", len(export.Proposals), maxReflectionProposals)}
	}
	out, notices := []reflected{}, []string{}
	seen := map[string]bool{}
	for i, p := range export.Proposals {
		n := i + 1
		if p.Audience != "agent" || p.Kind != "procedure" || len(p.ID) == 0 || len(p.ID) > 128 || len(p.EventRefs) > 20 {
			notices = append(notices, fmt.Sprintf("reflection proposal %d: not an agent-wide procedure, skipped", n))
			continue
		}
		text := strings.TrimSpace(p.Text)
		if text == "" || len(text) > maxReflectionText {
			notices = append(notices, fmt.Sprintf("reflection proposal %d: empty or over %d characters, skipped", n, maxReflectionText))
			continue
		}
		clean, flags := scrub.Scrub(text)
		if len(flags) > 0 {
			notices = append(notices, fmt.Sprintf("reflection proposal %d: the scrubber flagged %s, kept scrubbed and flagged for a person", n, strings.Join(flags, ", ")))
		}
		key := learning.Normalize(clean)
		if !seen[key] {
			out = append(out, reflected{Text: clean, PII: flags})
			seen[key] = true
		}
	}
	return out, notices
}
