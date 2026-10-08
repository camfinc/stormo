package loop

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
}

type SkillDecision struct {
	Hash     string `json:"hash"`
	Decision string `json:"decision"` // rejected | accepted
	At       string `json:"at"`
}

// Watermark is learnings/watermark.json.
type Watermark struct {
	LastNap        *string                  `json:"lastNap"`
	SkillDecisions map[string]SkillDecision `json:"skillDecisions"`
}

func learningsDir(root, agent string) string {
	return filepath.Join(manifest.AgentDir(root, agent), "learnings")
}

func LoadWatermark(root, agent string) (*Watermark, error) {
	w := &Watermark{SkillDecisions: map[string]SkillDecision{}}
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

// Dream folds an agent's unseen naps into its ledger and proposals.
func Dream(inst *instance.Instance, agentID string, s Store) (*DreamReport, error) {
	root := inst.Root
	agent, err := manifest.Load(root, agentID, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	eng, err := engines.Get(agent.Engine.Kind, inst)
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
	napIDs := []string{}
	for _, k := range keys {
		id := strings.TrimSuffix(strings.TrimPrefix(k, agentID+"/naps/"), ".json")
		if wm.LastNap == nil || id > *wm.LastNap {
			napIDs = append(napIDs, id)
		}
	}
	sort.Strings(napIDs)
	report := &DreamReport{Agent: agentID, Naps: napIDs, SkillProposals: []SkillProposalReport{}}
	if len(napIDs) == 0 {
		return report, nil
	}

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
		for _, m := range []struct {
			kind learning.Kind
			path string
		}{{learning.KindMemory, L.Memory.Memory}, {learning.KindUser, L.Memory.User}} {
			blob, err := learningBlob(s, agentID, nap, m.path)
			if err != nil {
				return nil, err
			}
			for _, raw := range learning.ParseEntries(deref(blob)) {
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
		bundledBlob, err := learningBlob(s, agentID, last, L.SkillsDir+"/.bundled_manifest")
		if err != nil {
			return nil, err
		}
		bundled := map[string]bool{}
		for _, l := range strings.Split(deref(bundledBlob), "\n") {
			if n := strings.TrimSpace(strings.SplitN(l, ":", 2)[0]); n != "" {
				bundled[n] = true
			}
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
			meta, _ := learning.MarshalIndent(SkillProposal{Skill: dir, NapHash: napHash, Nap: last.ID, PII: piiList})
			if err := os.WriteFile(filepath.Join(proposals, "skills", dir, ".proposal.json"), meta, 0o644); err != nil {
				return nil, err
			}
			status := "new"
			if inRepo {
				status = "changed"
			}
			report.SkillProposals = append(report.SkillProposals, SkillProposalReport{dir, status, piiList})
		}

		cron, err := learningBlob(s, agentID, last, "cron/jobs.json")
		if err != nil {
			return nil, err
		}
		repoCron, _ := os.ReadFile(filepath.Join(manifest.AgentDir(root, agentID), "hermes", "cron.jobs.json"))
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
		if usage, err := learningBlob(s, agentID, last, L.SkillsDir+"/.usage.json"); err != nil {
			return nil, err
		} else if usage != nil {
			if err := os.WriteFile(filepath.Join(learningsDir(root, agentID), "skill-usage.json"), []byte(*usage), 0o644); err != nil {
				return nil, err
			}
		}
	}

	if err := learning.SaveLedger(root, agentID, ledger); err != nil {
		return nil, err
	}
	lastID := napIDs[len(napIDs)-1]
	wm.LastNap = &lastID
	if err := SaveWatermark(root, agentID, wm); err != nil {
		return nil, err
	}
	return report, os.WriteFile(filepath.Join(learningsDir(root, agentID), "DREAM.md"), []byte(renderReport(report)), 0o644)
}

// repoSame: the nap's copy of a skill has exactly the repo's files with the same contents.
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
		cron = "yes (learnings/proposals/cron/jobs.json)"
	}
	lines = append(lines, "- cron proposal: "+cron, "",
		"Review with `stormo learn list "+r.Agent+"` and `stormo learn skills "+r.Agent+"`.", "")
	return strings.Join(lines, "\n")
}
