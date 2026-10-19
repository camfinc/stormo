package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/config"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

// Proposals beside lessons and skills: the schedules the agent changed at runtime
// (learnings/proposals/schedules.yaml, with schedules.meta.json) and the settings it changed in its
// engine config (learnings/proposals/settings.json). The dream rewrites them; decisions live in the
// watermark, so a decided proposal is not made again.

// SettingProposal is one runtime setting change waiting for review.
type SettingProposal struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Current is the repo's value ("" when the repo sets none).
	Current   string   `json:"current,omitempty"`
	Sensitive bool     `json:"sensitive,omitempty"`
	Opaque    bool     `json:"opaque,omitempty"`
	PII       []string `json:"pii"`
	Nap       string   `json:"nap"`
}

// SchedulesProposal is schedules.meta.json: the proposal's provenance, what the scrubber redacted,
// and the jobs in it that the repo does not have (a reject drops them from the running agent).
type SchedulesProposal struct {
	Nap         string   `json:"nap"`
	Hash        string   `json:"hash"`
	PII         []string `json:"pii"`
	RuntimeJobs []string `json:"runtimeJobs"`
}

func proposalsRoot(root, agent string) string {
	return filepath.Join(learningsDir(root, agent), "proposals")
}

func settingsPath(root, agent string) string {
	return filepath.Join(proposalsRoot(root, agent), "settings.json")
}

// settingSighting is the latest change of one setting across the dream's naps.
type settingSighting struct {
	s   engine.Setting
	nap string
}

// noteSettings records the settings nap's instance changed since it booted. A nap from before the
// booted settings were recorded is compared with the repo, sensitive settings left out (what the
// engine adds to its config itself would read as the agent's change).
func noteSettings(rs engine.RuntimeSettings, nap *Nap, body []byte, repo map[string]string, seen map[string]settingSighting) error {
	live, err := rs.Settings(body)
	if err != nil {
		return err
	}
	booted, fallback := map[string]string(nil), true
	if nap.Baseline != nil && nap.Baseline.Settings != nil {
		booted, fallback = nap.Baseline.Settings, false
	}
	for _, s := range live {
		if fallback {
			if s.Sensitive || repo[s.Key] == s.Value {
				continue
			}
		} else if booted[s.Key] == s.Value {
			continue
		}
		seen[s.Key] = settingSighting{s, nap.ID}
	}
	return nil
}

// writeSettings writes settings.json from the changes seen, leaving out what the repo already
// says, values decided before and settings the engine cannot write back.
func writeSettings(rs engine.RuntimeSettings, agent *manifest.Agent, dir string, seen map[string]settingSighting,
	repo map[string]string, wm *Watermark, scrubber *learning.Scrubber) (int, error) {
	out := []SettingProposal{}
	for _, key := range sortedKeys(seen) {
		c := seen[key]
		if repo[key] == c.s.Value {
			continue
		}
		if d, ok := wm.SettingDecisions[key]; ok && d.Hash == build.Sha256([]byte(c.s.Value)) {
			continue
		}
		if _, err := rs.SettingEdit(agent, c.s); err != nil {
			continue
		}
		text, pii := c.s.Value, []string{}
		if !c.s.Opaque {
			text, pii = scrubber.Scrub(c.s.Value)
		}
		if pii == nil {
			pii = []string{}
		}
		out = append(out, SettingProposal{Key: key, Value: text, Current: repo[key], Sensitive: c.s.Sensitive, Opaque: c.s.Opaque, PII: pii, Nap: c.nap})
	}
	if len(out) == 0 {
		return 0, nil
	}
	body, err := learning.MarshalIndent(out)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	return len(out), os.WriteFile(filepath.Join(dir, "settings.json"), append(body, '\n'), 0o644)
}

func repoSettings(rs engine.RuntimeSettings, agent *manifest.Agent, agentDir string) (map[string]string, error) {
	list, err := rs.RepoSettings(agent, agentDir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range list {
		out[s.Key] = s.Value
	}
	return out, nil
}

// ListSettingProposals reads settings.json.
func ListSettingProposals(root, agent string) ([]SettingProposal, error) {
	out := []SettingProposal{}
	body, err := os.ReadFile(settingsPath(root, agent))
	if os.IsNotExist(err) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s settings.json: %w", agent, err)
	}
	return out, nil
}

// Decider names who made a review decision: empty for a person, else the core's review.
type Decider struct {
	By     string
	Reason string
}

// DecideSetting accepts (writes it to the repo through the engine's SettingEdit) or rejects a
// setting proposal, and records the decision.
func DecideSetting(inst *instance.Instance, agentID, key, decision string, who Decider) (*SettingProposal, error) {
	root := inst.Root
	props, err := ListSettingProposals(root, agentID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(props, func(p SettingProposal) bool { return p.Key == key })
	if i < 0 {
		return nil, fmt.Errorf("%s: no setting proposal %s", agentID, key)
	}
	prop := props[i]
	if decision == "accepted" {
		if len(prop.PII) > 0 {
			return nil, fmt.Errorf("%s: %s was redacted (%v); set it by hand in the agent's files, then reject the proposal", agentID, key, prop.PII)
		}
		agent, err := manifest.Load(root, agentID, inst.Names.Secret)
		if err != nil {
			return nil, err
		}
		eng, err := engines.Get(agent.Engine.Kind, inst)
		if err != nil {
			return nil, err
		}
		rs, ok := eng.(engine.RuntimeSettings)
		if !ok {
			return nil, fmt.Errorf("%s: the %s engine has no runtime settings", agentID, eng.Kind())
		}
		edit, err := rs.SettingEdit(agent, engine.Setting{Key: prop.Key, Value: prop.Value, Sensitive: prop.Sensitive, Opaque: prop.Opaque})
		if err != nil {
			return nil, err
		}
		if err := config.SetPath(inst, agentID, edit.File, edit.Path, edit.Value); err != nil {
			return nil, err
		}
	}
	props = slices.Delete(props, i, i+1)
	if len(props) == 0 {
		if err := os.Remove(settingsPath(root, agentID)); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		pruneEmpty(proposalsRoot(root, agentID))
	} else {
		body, err := learning.MarshalIndent(props)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(settingsPath(root, agentID), append(body, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	wm, err := LoadWatermark(root, agentID)
	if err != nil {
		return nil, err
	}
	wm.SettingDecisions[key] = SkillDecision{Hash: build.Sha256([]byte(prop.Value)), Decision: decision, At: IsoMillis(time.Now()), By: who.By, Reason: who.Reason}
	return &prop, SaveWatermark(root, agentID, wm)
}

// writeSchedules writes schedules.yaml and its meta for the agent's runtime schedules, unless they
// match the repo or this exact proposal was decided before. Jobs a reviewer rejected are left out.
func writeSchedules(dir string, live, repo []manifest.Schedule, nap string, wm *Watermark, scrubber *learning.Scrubber) (bool, error) {
	repoIDs := map[string]bool{}
	for _, s := range repo {
		repoIDs[s.ID] = true
	}
	kept, runtime := []manifest.Schedule{}, []string{}
	for _, s := range live {
		if !repoIDs[s.ID] {
			if slices.Contains(wm.RejectedJobs, s.ID) {
				continue
			}
			runtime = append(runtime, s.ID)
		}
		kept = append(kept, s)
	}
	if sameSchedules(kept, repo) {
		return false, nil
	}
	body, err := yaml.Marshal(map[string]any{"schedules": kept})
	if err != nil {
		return false, err
	}
	text, pii := scrubber.Scrub(string(body))
	hash := build.Sha256([]byte(text))
	if wm.ScheduleDecision != nil && wm.ScheduleDecision.Hash == hash {
		return false, nil
	}
	if pii == nil {
		pii = []string{}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(dir, "schedules.yaml"), []byte(text), 0o644); err != nil {
		return false, err
	}
	meta, _ := learning.MarshalIndent(SchedulesProposal{Nap: nap, Hash: hash, PII: pii, RuntimeJobs: runtime})
	return true, os.WriteFile(filepath.Join(dir, "schedules.meta.json"), append(meta, '\n'), 0o644)
}

// ReadSchedulesProposal is the schedules proposal and its meta; nil when there is none.
func ReadSchedulesProposal(root, agent string) ([]manifest.Schedule, *SchedulesProposal, error) {
	dir := proposalsRoot(root, agent)
	body, err := os.ReadFile(filepath.Join(dir, "schedules.yaml"))
	if os.IsNotExist(err) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	var doc struct {
		Schedules []manifest.Schedule `yaml:"schedules"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, nil, fmt.Errorf("%s schedules.yaml: %w", agent, err)
	}
	meta := &SchedulesProposal{Hash: build.Sha256(body), PII: []string{}, RuntimeJobs: []string{}}
	if b, err := os.ReadFile(filepath.Join(dir, "schedules.meta.json")); err == nil {
		if err := json.Unmarshal(b, meta); err != nil {
			return nil, nil, fmt.Errorf("%s schedules.meta.json: %w", agent, err)
		}
	}
	if doc.Schedules == nil {
		doc.Schedules = []manifest.Schedule{}
	}
	return doc.Schedules, meta, nil
}

// DecideSchedules accepts the schedules proposal (agent.yaml's schedules: becomes it) or rejects
// it (the runtime jobs in it are dropped on the agent's next start), and records the decision.
func DecideSchedules(inst *instance.Instance, agentID, decision string, who Decider) (*SchedulesProposal, error) {
	root := inst.Root
	list, meta, err := ReadSchedulesProposal(root, agentID)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, fmt.Errorf("%s: no schedules proposal", agentID)
	}
	if decision == "accepted" {
		if len(meta.PII) > 0 {
			return nil, fmt.Errorf("%s: the schedules proposal was redacted (%v); copy what you want into agent.yaml by hand, then reject it", agentID, meta.PII)
		}
		if err := config.SetPath(inst, agentID, "agent.yaml", []string{"schedules"}, list); err != nil {
			return nil, err
		}
	}
	wm, err := LoadWatermark(root, agentID)
	if err != nil {
		return nil, err
	}
	if decision == "rejected" {
		for _, id := range meta.RuntimeJobs {
			if !slices.Contains(wm.RejectedJobs, id) {
				wm.RejectedJobs = append(wm.RejectedJobs, id)
			}
		}
		sort.Strings(wm.RejectedJobs)
	}
	wm.ScheduleDecision = &SkillDecision{Hash: meta.Hash, Decision: decision, At: IsoMillis(time.Now()), By: who.By, Reason: who.Reason}
	if err := SaveWatermark(root, agentID, wm); err != nil {
		return nil, err
	}
	dir := proposalsRoot(root, agentID)
	for _, f := range []string{"schedules.yaml", "schedules.meta.json"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	pruneEmpty(dir)
	return meta, nil
}

// PendingChanges counts an agent's open runtime proposals: its schedules (one proposal) and each
// setting.
func PendingChanges(root, agent string) int {
	n := 0
	if fileExists(filepath.Join(proposalsRoot(root, agent), "schedules.yaml")) {
		n++
	}
	if props, err := ListSettingProposals(root, agent); err == nil {
		n += len(props)
	}
	return n
}
