package loop

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The dream folds naps back into git as *proposals*. It runs off-task (a laptop or a scheduled job
// with read access to the blobs/ and naps/ prefixes only) and is structurally unable to read
// conversation history: it only ever fetches learning-class blobs, and raw blobs live under raw/.
// It writes to the working tree and never commits, branches or pushes.

type SkillProposalReport struct {
	Skill  string   `json:"skill"`
	Status string   `json:"status"` // new | changed
	PII    []string `json:"pii"`
}

type DreamReport struct {
	Agent          string                `json:"agent"`
	Naps           []string              `json:"naps"`
	NewLearnings   int                   `json:"newLearnings"`
	Resighted      int                   `json:"resighted"`
	SkillProposals []SkillProposalReport `json:"skillProposals"`
	CronProposal   bool                  `json:"cronProposal"`
	// Runtime settings changes proposed (learnings/proposals/settings.json).
	SettingProposals int `json:"settingProposals"`
	// Notices are what the dream skipped or could not take and went on without (a learning export
	// it cannot read, an evidence store that refused the merge). They never carry lesson text.
	Notices []string `json:"notices,omitempty"`
}

// notice records n in the report and the log.
func (r *DreamReport) notice(n string) {
	r.Notices = append(r.Notices, n)
	log.Printf("[dream] %s: %s", r.Agent, n)
}

type SkillDecision struct {
	Hash     string `json:"hash"`
	Decision string `json:"decision"` // rejected | accepted
	At       string `json:"at"`
	// By is who decided when it was not a person at the terminal or review page (the core's
	// review, with its model), and Reason what it said.
	By     string `json:"by,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Watermark is learnings/watermark.json.
type Watermark struct {
	LastNap *string `json:"lastNap"`
	// EvidenceNap is the newest nap whose outcome evidence (engine.OutcomeEvidence) is merged into
	// the evidence store: it moves only after a merge succeeds, so a refused merge is tried again
	// by the next dream, with or without a new nap, and Prune keeps the naps after it. Absent (nil):
	// the same as LastNap (a watermark from before it); "" is before every nap.
	EvidenceNap    *string                  `json:"evidenceNap,omitempty"`
	SkillDecisions map[string]SkillDecision `json:"skillDecisions"`
	// The last decision on the schedules proposal (Hash: its text), and the jobs the agent created
	// at runtime that a reviewer rejected: rehydrate drops them (build.Info.RejectedJobs).
	ScheduleDecision *SkillDecision `json:"scheduleDecision,omitempty"`
	RejectedJobs     []string       `json:"rejectedJobs,omitempty"`
	// Scripts that came with a rejected schedules proposal (file name → the rejected version's
	// sha256): rehydrate leaves them out (build.Info.RejectedScripts) and the dream does not
	// propose that version again.
	RejectedScripts map[string]string `json:"rejectedScripts,omitempty"`
	// Decisions on runtime settings by key (Hash: the value), so a decided value is not proposed again.
	SettingDecisions map[string]SkillDecision `json:"settingDecisions,omitempty"`
}

func learningsDir(root, agent string) string {
	return filepath.Join(manifest.AgentDir(root, agent), "learnings")
}

func LoadWatermark(root, agent string) (*Watermark, error) {
	w := &Watermark{SkillDecisions: map[string]SkillDecision{}, SettingDecisions: map[string]SkillDecision{}}
	body, err := os.ReadFile(filepath.Join(learningsDir(root, agent), "watermark.json"))
	if os.IsNotExist(err) {
		return w, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, w); err != nil {
		return nil, err
	}
	if w.SkillDecisions == nil {
		w.SkillDecisions = map[string]SkillDecision{}
	}
	if w.SettingDecisions == nil {
		w.SettingDecisions = map[string]SkillDecision{}
	}
	return w, nil
}

func SaveWatermark(root, agent string, w *Watermark) error {
	p := filepath.Join(learningsDir(root, agent), "watermark.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	body, err := learning.MarshalIndent(w)
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(body, '\n'), 0o644)
}

// runtimeScript is a script in the nap that the agent wrote or edited: its instance did not boot with
// that version, and the repo does not hold it either.
type runtimeScript struct {
	ScriptChange
	Body string
}

// runtimeScripts are the nap's scripts the agent wrote or edited, less versions a reviewer
// rejected. A nap from before booted scripts were recorded has none (nothing tells them apart).
func runtimeScripts(s Store, root, agentID string, L *engine.Layout, nap *Nap, wm *Watermark) ([]runtimeScript, error) {
	if L.ScriptsDir == "" || nap.Baseline == nil || nap.Baseline.Scripts == nil {
		return nil, nil
	}
	out := []runtimeScript{}
	for _, f := range nap.Files {
		name, ok := strings.CutPrefix(f.Path, L.ScriptsDir+"/")
		if !ok || f.Class != engine.Learning || nap.Baseline.Scripts[name] == f.Sha256 || wm.RejectedScripts[name] == f.Sha256 {
			continue
		}
		body, err := learningBlob(s, agentID, nap, f.Path)
		if err != nil {
			return nil, err
		}
		if body == nil {
			continue
		}
		c := ScriptChange{Name: name, Status: "new", Sha256: f.Sha256}
		if repo, err := os.ReadFile(filepath.Join(manifest.AgentDir(root, agentID), "scripts", filepath.FromSlash(name))); err == nil {
			if string(repo) == *body {
				continue // already in the repo
			}
			c.Status = "changed"
			c.Stale = build.Sha256(repo) != nap.Baseline.Scripts[name]
		}
		out = append(out, runtimeScript{c, *body})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func learningBlob(s Store, agent string, nap *Nap, path string) (*string, error) {
	for _, f := range nap.Files {
		if f.Path != path {
			continue
		}
		if f.Class != engine.Learning {
			return nil, fmt.Errorf("dream refused to read %s: class %s", path, f.Class)
		}
		body, err := s.Get(BlobKey(agent, engine.Learning, f.Sha256))
		if err != nil || body == nil {
			return nil, err
		}
		t := string(body)
		return &t, nil
	}
	return nil, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// engineFor is an agent's engine (engines.Get; tests give it a fake engine).
var engineFor = engines.Get

// Dream folds an agent's unseen naps into its ledger and proposals.
func Dream(inst *instance.Instance, agentID string, s Store) (*DreamReport, error) {
	root := inst.Root
	agent, err := manifest.Load(root, agentID, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	eng, err := engineFor(agent.Engine.Kind, inst)
	if err != nil {
		return nil, err
	}
	L := eng.Layout()
	scrubber := learning.NewScrubber(inst)
	wm, err := LoadWatermark(root, agentID)
	if err != nil {
		return nil, err
	}
	ledger, err := learning.LoadLedger(root, agentID)
	if err != nil {
		return nil, err
	}
	keys, err := s.List(agentID + "/naps/")
	if err != nil {
		return nil, err
	}
	napIDs, evidenceIDs := []string{}, []string{}
	evidenceMark := wm.evidenceMark()
	for _, k := range keys {
		id := strings.TrimSuffix(strings.TrimPrefix(k, agentID+"/naps/"), ".json")
		if wm.LastNap == nil || id > *wm.LastNap {
			napIDs = append(napIDs, id)
		}
		if evidenceMark == nil || id > *evidenceMark {
			evidenceIDs = append(evidenceIDs, id)
		}
	}
	sort.Strings(napIDs)
	sort.Strings(evidenceIDs)
	report := &DreamReport{Agent: agentID, Naps: napIDs, SkillProposals: []SkillProposalReport{}}
	if len(napIDs) == 0 {
		if len(evidenceIDs) == 0 {
			return report, nil
		}
		// No new nap, but evidence a refused merge left behind: merge it alone.
		events, err := pendingEvidence(s, agentID, evidenceIDs, nil, eng, report)
		if err != nil {
			return nil, err
		}
		if mergeOutcomes(root, agentID, events, report) {
			wm.EvidenceNap = &evidenceIDs[len(evidenceIDs)-1]
			if err := SaveWatermark(root, agentID, wm); err != nil {
				return nil, err
			}
		}
		return report, nil
	}

	// Settings the agent changed at runtime: every unseen nap, since a restart resets the config
	// and only an earlier nap may hold the change.
	rs, hasSettings := eng.(engine.RuntimeSettings)
	hasSettings = hasSettings && L.Config != ""
	agentDir := manifest.AgentDir(root, agentID)
	var repoSet map[string]string
	if hasSettings {
		if repoSet, err = repoSettings(rs, agent, agentDir); err != nil {
			return nil, err
		}
	}
	settings := map[string]settingSighting{}

	outcomeEvents := []learning.OutcomeEvent{}
	// 1. Memories from every unseen nap, so provenance counts every instance that held a lesson.
	var last *Nap
	for _, id := range napIDs {
		nap := &Nap{}
		if ok, err := ReadJSON(s, NapKey(agentID, id), nap); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		last = nap
		if err := ingestReflectionProposals(s, agentID, nap, eng, scrubber, &ledger, report); err != nil {
			return nil, err
		}
		events, err := outcomeEvidence(s, agentID, nap, eng, report)
		if err != nil {
			return nil, err
		}
		outcomeEvents = append(outcomeEvents, events...)
		if hasSettings {
			blob, err := learningBlob(s, agentID, nap, L.Config)
			if err != nil {
				return nil, err
			}
			if blob != nil {
				// A plain file can be napped mid-write; one unreadable config must not stop the
				// dream (the watermark would never pass this nap). Its settings are skipped.
				if err := noteSettings(rs, nap, []byte(*blob), repoSet, settings); err != nil {
					log.Printf("[dream] %s: nap %s: %s unreadable, settings skipped: %v", agentID, nap.ID, L.Config, err)
				}
			}
		}
		for _, m := range []struct {
			kind learning.Kind
			path string
		}{{learning.KindMemory, L.Memory.Memory}, {learning.KindUser, L.Memory.User}} {
			blob, err := learningBlob(s, agentID, nap, m.path)
			if err != nil {
				return nil, err
			}
			for _, raw := range learning.ParseEntries(eng.Memory(), deref(blob)) {
				text, findings := scrubber.Scrub(raw)
				_, created := learning.Upsert(&ledger, learning.Sighting{Kind: m.kind, Text: text, PII: findings, Agent: agentID,
					Unit: agent.Unit, Instance: nap.Instance, Nap: nap.ID, At: nap.TakenAt})
				if created {
					report.NewLearnings++
				} else {
					report.Resighted++
				}
			}
		}
	}

	// 2. Skills and cron from the newest nap only: it already contains every earlier patch.
	proposals := filepath.Join(learningsDir(root, agentID), "proposals")
	if err := os.RemoveAll(proposals); err != nil {
		return nil, err
	}
	if last != nil {
		var readErr error
		bundled := eng.EngineOwnedSkills(func(p string) []byte {
			b, err := learningBlob(s, agentID, last, p)
			if err != nil {
				readErr = err
			}
			return []byte(deref(b))
		})
		if readErr != nil {
			return nil, readErr
		}
		learningPaths := []string{}
		napHashes := map[string]string{}
		for _, f := range last.Files {
			napHashes[f.Path] = f.Sha256
			if f.Class == engine.Learning {
				learningPaths = append(learningPaths, f.Path)
			}
		}
		repoSkills := filepath.Join(manifest.AgentDir(root, agentID), "skills")
		// What the nap's instance booted with: a skill still at that hash is not the agent's change,
		// however far the repo has moved since (an edit not yet deployed would read as a revert).
		booted := map[string]string{}
		baseSha := ""
		if last.Baseline != nil {
			booted = last.Baseline.Skills.Map()
			baseSha = last.Baseline.GitSha
		}
		dirs := build.SkillDirs(learningPaths, L.SkillsDir)
		for _, dir := range sortedKeys(dirs) {
			paths := dirs[dir]
			leaf := dir[strings.LastIndex(dir, "/")+1:]
			if leaf == inst.Names.KnowledgeSkill || leaf == inst.Names.SharedSkill {
				continue // generated; never round-trips
			}
			_, statErr := os.Stat(filepath.Join(repoSkills, dir, "SKILL.md"))
			inRepo := statErr == nil
			if !inRepo && bundled[leaf] {
				continue // engine-shipped skill
			}
			if strings.HasPrefix(strings.Split(dir, "/")[0], ".") {
				continue // .archive etc.
			}
			napHash := build.SkillHash(napHashes, paths)
			if d, ok := wm.SkillDecisions[dir]; ok && d.Hash == napHash {
				continue // already decided
			}
			bootHash, wasBooted := booted[dir]
			if wasBooted && bootHash == napHash {
				continue // unchanged since boot
			}
			// The agent changed a version the repo has since moved past: accepting it would undo
			// the repo's change, so the proposal says so and accept refuses it.
			stale := inRepo && wasBooted && repoSkillHash(filepath.Join(repoSkills, dir), L.SkillsDir+"/"+dir) != bootHash
			bodies := map[string]string{}
			for _, p := range paths {
				b, err := learningBlob(s, agentID, last, p)
				if err != nil {
					return nil, err
				}
				bodies[p] = deref(b)
			}
			if inRepo && repoSame(filepath.Join(repoSkills, dir), L.SkillsDir+"/"+dir, bodies) {
				continue
			}
			pii := map[string]bool{}
			for _, p := range sortedKeys(bodies) {
				text, findings := scrubber.Scrub(bodies[p])
				for _, f := range findings {
					pii[f] = true
				}
				dest := filepath.Join(proposals, "skills", strings.TrimPrefix(p, L.SkillsDir+"/"))
				if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(dest, []byte(text), 0o644); err != nil {
					return nil, err
				}
			}
			piiList := sortedKeys(pii)
			meta, _ := learning.MarshalIndent(SkillProposal{Skill: dir, NapHash: napHash, Nap: last.ID, PII: piiList, Stale: stale, Base: baseSha})
			if err := os.WriteFile(filepath.Join(proposals, "skills", dir, ".proposal.json"), meta, 0o644); err != nil {
				return nil, err
			}
			status := "new"
			if stale {
				status = "changed (on an older version; merge by hand)"
			} else if inRepo {
				status = "changed"
			}
			report.SkillProposals = append(report.SkillProposals, SkillProposalReport{dir, status, piiList})
		}

		var cron *string
		var repoCron []byte
		if L.Schedules != "" {
			if cron, err = learningBlob(s, agentID, last, L.Schedules); err != nil {
				return nil, err
			}
			repoCron, _ = os.ReadFile(filepath.Join(manifest.AgentDir(root, agentID), filepath.FromSlash(L.ScheduleSource)))
		}
		// Format 1: the agent's schedules live in agent.yaml; a change the agent made at runtime is
		// proposed in that form. A job agent.yaml cannot express falls back to the engine's file.
		if cron != nil && agent.Schedules != nil {
			if live, err := eng.ReadSchedules([]byte(*cron)); err == nil {
				scripts, err := runtimeScripts(s, root, agentID, L, last, wm)
				if err != nil {
					return nil, err
				}
				if report.CronProposal, err = writeSchedules(proposals, live, agent.Schedules, scripts, last.ID, wm, scrubber); err != nil {
					return nil, err
				}
				cron = nil
			}
		}
		if cron != nil && learning.Trim(*cron) != learning.Trim(string(repoCron)) {
			if err := os.MkdirAll(filepath.Join(proposals, "cron"), 0o755); err != nil {
				return nil, err
			}
			text, _ := scrubber.Scrub(*cron)
			if err := os.WriteFile(filepath.Join(proposals, "cron", "jobs.json"), []byte(text), 0o644); err != nil {
				return nil, err
			}
			report.CronProposal = true
		}
		if L.SkillUsage == "" {
			// the engine keeps no usage file
		} else if usage, err := learningBlob(s, agentID, last, L.SkillUsage); err != nil {
			return nil, err
		} else if usage != nil {
			if err := os.WriteFile(filepath.Join(learningsDir(root, agentID), "skill-usage.json"), []byte(*usage), 0o644); err != nil {
				return nil, err
			}
		}
	}

	if hasSettings {
		if report.SettingProposals, err = writeSettings(rs, agent, proposals, settings, repoSet, wm, scrubber); err != nil {
			return nil, err
		}
	}

	// Evidence of earlier naps whose merge was refused comes first (the merge dedupes by event).
	earlier, err := pendingEvidence(s, agentID, evidenceIDs, napIDs, eng, report)
	if err != nil {
		return nil, err
	}
	merged := mergeOutcomes(root, agentID, append(earlier, outcomeEvents...), report)
	if err := learning.SaveLedger(root, agentID, ledger); err != nil {
		return nil, err
	}
	lastID := napIDs[len(napIDs)-1]
	if merged {
		wm.EvidenceNap = nil // caught up: the same as LastNap
	} else if wm.EvidenceNap == nil {
		prev := ""
		if wm.LastNap != nil {
			prev = *wm.LastNap
		}
		wm.EvidenceNap = &prev
	}
	wm.LastNap = &lastID
	if err := SaveWatermark(root, agentID, wm); err != nil {
		return nil, err
	}
	return report, os.WriteFile(filepath.Join(learningsDir(root, agentID), "DREAM.md"), []byte(renderReport(report)), 0o644)
}

// evidenceMark is the newest nap whose evidence is merged (nil: none).
func (w *Watermark) evidenceMark() *string {
	if w.EvidenceNap == nil {
		return w.LastNap
	}
	if *w.EvidenceNap == "" {
		return nil
	}
	return w.EvidenceNap
}

// pendingEvidence are the outcome events of the naps in ids that skip does not list: naps the
// nap watermark passed whose merge was refused.
func pendingEvidence(s Store, agentID string, ids, skip []string, eng engine.Engine, report *DreamReport) ([]learning.OutcomeEvent, error) {
	out := []learning.OutcomeEvent{}
	for _, id := range ids {
		if slices.Contains(skip, id) {
			continue
		}
		nap := &Nap{}
		if ok, err := ReadJSON(s, NapKey(agentID, id), nap); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		events, err := outcomeEvidence(s, agentID, nap, eng, report)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

// mergeOutcomes merges events into the evidence store; false (with a notice) when the store
// refused them, so the evidence watermark stays behind and the next dream tries again.
func mergeOutcomes(root, agentID string, events []learning.OutcomeEvent, report *DreamReport) bool {
	if len(events) == 0 {
		return true
	}
	if err := learning.MergeEvidence(root, agentID, events); err != nil {
		report.notice("outcome evidence not merged (" + err.Error() + "); the next dream tries again")
		return false
	}
	return true
}

// outcomeEvidence are the agent-wide, exportable events of the nap's outcome export
// (engine.OutcomeEvidence). Only a store error fails the dream: an export it cannot read, or an
// event it cannot take, is skipped and reported in the dream's notices.
func outcomeEvidence(s Store, agentID string, nap *Nap, eng engine.Engine, report *DreamReport) ([]learning.OutcomeEvent, error) {
	source, ok := eng.(engine.OutcomeEvidence)
	if !ok {
		return nil, nil
	}
	blob, err := learningBlob(s, agentID, nap, source.OutcomePath())
	if err != nil || blob == nil {
		return nil, err
	}
	skip := func(why string) ([]learning.OutcomeEvent, error) {
		report.notice(nap.ID + ": outcome evidence export " + why + ", skipped")
		return nil, nil
	}
	if len(*blob) > learning.MaxEvidenceBytes {
		return skip("over the evidence byte limit")
	}
	var outcomes learning.EvidenceStore
	if err := json.Unmarshal([]byte(*blob), &outcomes); err != nil {
		return skip("is not valid JSON")
	}
	if outcomes.Version != learning.EvidenceVersion || len(outcomes.Events) > learning.MaxOutcomeEvents {
		return skip("has an unsupported version or too many events")
	}
	out, invalid := []learning.OutcomeEvent{}, 0
	for _, event := range outcomes.Events {
		if event.Validate() != nil {
			invalid++
			continue
		}
		// References never cross into tracked evidence; only deliberately agent-scoped events
		// with opaque identities do (learning.ExportableEvidence).
		event.EvidenceRef = ""
		if event.Audience != "agent" || !learning.ExportableEvidence(event) {
			continue
		}
		out = append(out, event)
	}
	if invalid > 0 {
		report.notice(fmt.Sprintf("%s: outcome evidence export: %d invalid events skipped", nap.ID, invalid))
	}
	return out, nil
}

// repoSame: the nap's copy of a skill has exactly the repo's files with the same contents.
// repoSkillHash hashes a skill dir in the repo as the build does (its files are copied verbatim).
func repoSkillHash(repoDir, prefix string) string {
	hashes := map[string]string{}
	paths := []string{}
	_ = filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repoDir, p)
		key := prefix + "/" + filepath.ToSlash(rel)
		paths = append(paths, key)
		hashes[key] = build.Sha256(b)
		return nil
	})
	return build.SkillHash(hashes, paths)
}

func repoSame(repoDir, prefix string, bodies map[string]string) bool {
	n := 0
	same := true
	_ = filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		rel, _ := filepath.Rel(repoDir, p)
		b, err := os.ReadFile(p)
		if body, ok := bodies[prefix+"/"+filepath.ToSlash(rel)]; err != nil || !ok || body != string(b) {
			same = false
		}
		return nil
	})
	return same && n == len(bodies)
}

func renderReport(r *DreamReport) string {
	first, last := "-", "-"
	if len(r.Naps) > 0 {
		first, last = r.Naps[0], r.Naps[len(r.Naps)-1]
	}
	lines := []string{
		"# Last dream: " + r.Agent, "",
		fmt.Sprintf("- naps folded: %d (%s → %s)", len(r.Naps), first, last),
		fmt.Sprintf("- new learnings proposed: %d; re-sighted: %d", r.NewLearnings, r.Resighted),
		fmt.Sprintf("- skill proposals: %d", len(r.SkillProposals)),
	}
	for _, p := range r.SkillProposals {
		scrubbed := ""
		if len(p.PII) > 0 {
			scrubbed = " (scrubbed: " + strings.Join(p.PII, ", ") + ")"
		}
		lines = append(lines, fmt.Sprintf("  - %s `%s`%s", p.Status, p.Skill, scrubbed))
	}
	cron := "no"
	if r.CronProposal {
		cron = "yes (learnings/proposals/schedules.yaml, or cron/jobs.json for an engine-only job)"
	}
	lines = append(lines, "- cron proposal: "+cron, fmt.Sprintf("- settings changed at runtime: %d", r.SettingProposals))
	if len(r.Notices) > 0 {
		lines = append(lines, "- notices (skipped or not taken; the rest of the dream went through):")
		for _, n := range r.Notices {
			lines = append(lines, "  - "+n)
		}
	}
	lines = append(lines, "",
		"Review with `stormo learn list "+r.Agent+"`, `stormo learn skills "+r.Agent+"`, `stormo learn schedules "+r.Agent+
			"` and `stormo learn settings "+r.Agent+"`.", "")
	return strings.Join(lines, "\n")
}

// sameSchedules compares schedules as data (nil and empty lists alike).
func sameSchedules(a, b []manifest.Schedule) bool {
	norm := func(l []manifest.Schedule) []manifest.Schedule {
		out := make([]manifest.Schedule, len(l))
		for i, s := range l {
			if len(s.Skills) == 0 {
				s.Skills = nil
			}
			if len(s.Tools) == 0 {
				s.Tools = nil
			}
			out[i] = s
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}
