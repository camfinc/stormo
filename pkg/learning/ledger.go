package learning

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// One line per distinct learning, keyed by content hash so the same lesson captured by several
// task instances (or re-learned after a restart) merges into one entry with a rising seen count.

type Kind string

const (
	KindMemory Kind = "memory"
	KindUser   Kind = "user"
)

type Status string

const (
	Proposed Status = "proposed"
	Accepted Status = "accepted"
	Rejected Status = "rejected"
)

// Learning is one ledger line. Field order is the ledger's line format; keep it.
type Learning struct {
	ID        string         `json:"id"`
	Kind      Kind           `json:"kind"`
	Scope     manifest.Scope `json:"scope"`
	Agent     string         `json:"agent"`
	Unit      string         `json:"unit"`
	Text      string         `json:"text"` // already scrubbed; raw text never reaches git
	PII       []string       `json:"pii"`  // scrubber finding kinds, empty when clean
	Status    Status         `json:"status"`
	Pinned    *bool          `json:"pinned,omitempty"`
	FirstSeen string         `json:"firstSeen"`
	LastSeen  string         `json:"lastSeen"`
	SeenCount int            `json:"seenCount"`
	Instances []string       `json:"instances"`
	Naps      []string       `json:"naps"` // nap keys that carried it (provenance)
	DecidedAt *string        `json:"decidedAt,omitempty"`
	Note      *string        `json:"note,omitempty"`
}

func (l *Learning) IsPinned() bool { return l.Pinned != nil && *l.Pinned }

// LearningID is the first 12 hex chars of sha256("<kind>\n<normalized text>").
func LearningID(kind Kind, text string) string {
	h := sha256.Sum256([]byte(string(kind) + "\n" + Normalize(text)))
	return hex.EncodeToString(h[:])[:12]
}

func LedgerPath(root, agent string) string {
	return filepath.Join(manifest.AgentDir(root, agent), "learnings", "ledger.jsonl")
}

func LoadLedger(root, agent string) ([]*Learning, error) {
	body, err := os.ReadFile(LedgerPath(root, agent))
	if os.IsNotExist(err) {
		return []*Learning{}, nil
	} else if err != nil {
		return nil, err
	}
	out := []*Learning{}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var l Learning
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return nil, fmt.Errorf("%s: %w", LedgerPath(root, agent), err)
		}
		out = append(out, &l)
	}
	return out, nil
}

// MarshalCompact is compact JSON without HTML escaping or a trailing newline.
func MarshalCompact(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// MarshalIndent is two-space indented JSON without HTML escaping or a trailing newline.
func MarshalIndent(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func SaveLedger(root, agent string, entries []*Learning) error {
	p := LedgerPath(root, agent)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	sorted := append([]*Learning{}, entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].FirstSeen != sorted[j].FirstSeen {
			return sorted[i].FirstSeen < sorted[j].FirstSeen
		}
		return sorted[i].ID < sorted[j].ID
	})
	var b strings.Builder
	for _, e := range sorted {
		line, err := MarshalCompact(e)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteString("\n")
	}
	return os.WriteFile(p, []byte(b.String()), 0o644)
}

// Sighting is a learning seen in a nap.
type Sighting struct {
	Kind     Kind
	Text     string // scrubbed
	PII      []string
	Agent    string
	Unit     string
	Instance string
	Nap      string
	At       string
}

// Upsert records a sighting. Decided entries keep their status; only provenance moves.
func Upsert(ledger *[]*Learning, s Sighting) (*Learning, bool) {
	id := LearningID(s.Kind, s.Text)
	for _, e := range *ledger {
		if e.ID != id {
			continue
		}
		if !contains(e.Naps, s.Nap) {
			e.Naps = append(e.Naps, s.Nap)
			e.SeenCount++
		}
		if !contains(e.Instances, s.Instance) {
			e.Instances = append(e.Instances, s.Instance)
		}
		if s.At > e.LastSeen {
			e.LastSeen = s.At
		}
		return e, false
	}
	pii := s.PII
	if pii == nil {
		pii = []string{}
	}
	e := &Learning{ID: id, Kind: s.Kind, Scope: manifest.ScopeAgent, Agent: s.Agent, Unit: s.Unit, Text: s.Text, PII: pii,
		Status: Proposed, FirstSeen: s.At, LastSeen: s.At, SeenCount: 1, Instances: []string{s.Instance}, Naps: []string{s.Nap}}
	*ledger = append(*ledger, e)
	return e, true
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// PromotionError: user-profile memories and PII-flagged text never leave the agent.
type PromotionError struct{ Msg string }

func (e *PromotionError) Error() string { return e.Msg }

// Promote widens a learning's audience.
func Promote(e *Learning, scope manifest.Scope) error {
	if scope == manifest.ScopeAgent {
		e.Scope = scope
		return nil
	}
	if e.Kind == KindUser {
		return &PromotionError{e.ID + ": user-profile memories stay agent-scoped"}
	}
	if len(e.PII) > 0 {
		return &PromotionError{fmt.Sprintf("%s: flagged %s; rewrite it without PII first", e.ID, strings.Join(e.PII, ","))}
	}
	if e.Status != Accepted {
		return &PromotionError{e.ID + ": accept it before promoting"}
	}
	e.Scope = scope
	return nil
}

// Rank orders the hot tier: pinned, then corroboration across naps, then recency.
func Rank(a, b *Learning) bool {
	if a.IsPinned() != b.IsPinned() {
		return a.IsPinned()
	}
	if a.SeenCount != b.SeenCount {
		return a.SeenCount > b.SeenCount
	}
	if a.LastSeen != b.LastSeen {
		return a.LastSeen > b.LastSeen
	}
	return a.ID < b.ID
}

// VisibleLearnings are the accepted learnings an agent may see: its own, unit-scoped ones from
// agents in the same unit, and group-scoped ones from everyone. Never other units' unit-scoped ones.
func VisibleLearnings(inst *instance.Instance, agent string) ([]*Learning, error) {
	self, err := manifest.Load(inst.Root, agent, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	out := []*Learning{}
	for _, other := range manifest.AgentIDs(inst.Root) {
		m := self
		if other != agent {
			if m, err = manifest.Load(inst.Root, other, inst.Names.Secret); err != nil {
				return nil, err
			}
		}
		entries, err := LoadLedger(inst.Root, other)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Status != Accepted {
				continue
			}
			visible := other == agent || e.Scope == manifest.ScopeGroup || (e.Scope == manifest.ScopeUnit && m.Unit == self.Unit)
			if visible && (other == agent || e.Kind != KindUser) {
				out = append(out, e)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return Rank(out[i], out[j]) })
	return out, nil
}
