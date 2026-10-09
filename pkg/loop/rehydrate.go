package loop

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

// Rehydrate runs once per task start, before the engine container. It builds the engine home from
// the image baseline (what git says) plus the latest nap (what the previous instance learned or
// stored and the dream has not yet folded into git).

// RehydrateReport is written to <home>/.swarm/rehydrate.json.
type RehydrateReport struct {
	Nap            *string  `json:"nap"`
	Baseline       string   `json:"baseline"`
	RestoredSkills []string `json:"restoredSkills"`
	KeptRepoSkills []string `json:"keptRepoSkills"` // runtime patch dropped: the repo changed that skill since the nap
	PurgedMemories int      `json:"purgedMemories"`
	DeferredSeed   int      `json:"deferredSeed"`
	RestoredFiles  int      `json:"restoredFiles"`
}

type liveFile struct {
	meta NapFile
	body []byte
}

func readBaseline(dir string) (map[string][]byte, *build.Info, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == build.InfoPath {
			return nil
		}
		b, err := os.ReadFile(p)
		files[rel] = b
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	body, err := os.ReadFile(filepath.Join(dir, build.InfoPath))
	if err != nil {
		return nil, nil, err
	}
	var info build.Info
	return files, &info, json.Unmarshal(body, &info)
}

// ChownTree hands dir to uid:gid recursively (symlinks themselves, never their targets).
func ChownTree(dir string, uid, gid int, chown func(string, int, int) error) (int, error) {
	if chown == nil {
		chown = os.Lchown
	}
	n := 0
	var walk func(string) error
	walk = func(p string) error {
		if err := chown(p, uid, gid); err != nil {
			return err
		}
		n++
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if st.IsDir() {
			entries, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := walk(filepath.Join(p, e.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return n, walk(dir)
}

// RehydrateOptions: the agent, its engine, the home to build, the baseline and the store.
type RehydrateOptions struct {
	Agent       *manifest.Agent
	Engine      engine.Engine
	Home        string
	BaselineDir string
	Store       Store // nil: baseline only
	Scrubber    *learning.Scrubber
}

func Rehydrate(o RehydrateOptions) (*RehydrateReport, error) {
	L := o.Engine.Layout()
	base, info, err := readBaseline(o.BaselineDir)
	if err != nil {
		return nil, err
	}
	var nap *Nap
	live := map[string]liveFile{}
	if o.Store != nil {
		if nap, err = Latest(o.Store, o.Agent.ID); err != nil {
			return nil, err
		}
		if nap != nil {
			for _, f := range nap.Files {
				body, err := o.Store.Get(BlobKey(o.Agent.ID, f.Class, f.Sha256))
				if err != nil {
					return nil, err
				}
				if body == nil {
					log.Printf("[rehydrate] missing blob for %s", f.Path)
					continue
				}
				live[f.Path] = liveFile{f, body}
			}
		}
	}
	out := map[string][]byte{}
	report := &RehydrateReport{Baseline: info.GitSha, RestoredSkills: []string{}, KeptRepoSkills: []string{}}
	if nap != nil {
		report.Nap = &nap.ID
	}

	// 1. Baseline, except files that need merging.
	for p, body := range base {
		if p != L.Memory.Memory && p != L.Memory.User {
			out[p] = body
		}
	}

	// 2. Skills: restore runtime versions unless the repo moved underneath them.
	livePaths := make([]string, 0, len(live))
	for p := range live {
		livePaths = append(livePaths, p)
	}
	engineOwned := o.Engine.EngineOwnedSkills(func(p string) []byte { return live[p].body })
	withheld := map[string]bool{}
	for _, w := range info.Withheld {
		withheld[w] = true
	}
	baseSkills := info.Skills.Map()
	var napSkills map[string]string
	if nap != nil && nap.Baseline != nil {
		napSkills = nap.Baseline.Skills.Map()
	}
	dirs := build.SkillDirs(livePaths, L.SkillsDir)
	for _, dir := range sortedKeys(dirs) {
		paths := dirs[dir]
		if withheld[dir] {
			continue // left out of this build: an optional secret it needs is absent
		}
		baseHash, inBaseline := baseSkills[dir]
		leaf := dir[strings.LastIndex(dir, "/")+1:]
		if !inBaseline && engineOwned[leaf] {
			continue // the image ships its own copy
		}
		if inBaseline {
			if napHash, ok := napSkills[dir]; !ok || napHash != baseHash {
				report.KeptRepoSkills = append(report.KeptRepoSkills, dir)
				continue
			}
			hashes := map[string]string{}
			for _, p := range paths {
				hashes[p] = live[p].meta.Sha256
			}
			if build.SkillHash(hashes, paths) == baseHash {
				continue // unchanged at runtime
			}
			for p := range out {
				if strings.HasPrefix(p, L.SkillsDir+"/"+dir+"/") {
					delete(out, p)
				}
			}
		}
		for _, p := range paths {
			out[p] = live[p].body
		}
		report.RestoredSkills = append(report.RestoredSkills, dir)
	}
	// The engine's skill telemetry and archive come back, except what it rebuilds (NotRestored).
	for p, v := range live {
		if strings.HasPrefix(p, L.SkillsDir+"/.") && !slices.Contains(L.NotRestored, p) && v.meta.Class == engine.Learning {
			out[p] = v.body
		}
	}

	// 3. Hot memory: live wins, rejected purged, seed fills headroom.
	rejected := map[string]bool{}
	for _, id := range info.Rejected {
		rejected[id] = true
	}
	for _, m := range []struct {
		kind  learning.Kind
		path  string
		limit int
	}{{learning.KindMemory, L.Memory.Memory, o.Agent.Learning.MemoryCharLimit}, {learning.KindUser, L.Memory.User, o.Agent.Learning.UserCharLimit}} {
		liveEntries := learning.ParseEntries(o.Engine.Memory(), string(live[m.path].body))
		seed := learning.ParseEntries(o.Engine.Memory(), string(base[m.path]))
		rejectedText := map[string]bool{}
		for _, e := range liveEntries {
			text, _ := o.Scrubber.Scrub(e)
			if rejected[learning.LearningID(m.kind, text)] {
				rejectedText[learning.Normalize(e)] = true
			}
		}
		entries, purged, deferred := learning.MergeHot(o.Engine.Memory(), liveEntries, seed, rejectedText, m.limit, o.Agent.Learning.SeedFill)
		report.PurgedMemories += len(purged)
		report.DeferredSeed += len(deferred)
		out[m.path] = []byte(o.Engine.Memory().File(entries))
	}

	// 4. Schedules: merge baseline and runtime-created jobs.
	if c, ok := live[L.Schedules]; ok && L.Schedules != "" {
		merged, err := o.Engine.MergeSchedules(base[L.Schedules], c.body)
		if err != nil {
			return nil, err
		}
		out[L.Schedules] = merged
	}

	// 5. State and raw history come back verbatim.
	for p, v := range live {
		if v.meta.Class != engine.Learning {
			out[p] = v.body
		}
	}

	// Write. Runtime dirs the engine owns but we did not capture (logs, caches) are left alone.
	for _, d := range L.Managed {
		if err := os.RemoveAll(filepath.Join(o.Home, d)); err != nil {
			return nil, err
		}
	}
	for p, body := range out {
		dst := filepath.Join(o.Home, p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return nil, err
		}
	}
	for _, s := range o.Agent.State {
		if err := os.MkdirAll(filepath.Join(o.Home, L.StateRoot, s.Name), 0o755); err != nil {
			return nil, err
		}
	}
	for _, p := range L.Placeholders {
		if _, err := os.Stat(filepath.Join(o.Home, p)); os.IsNotExist(err) {
			if err := os.WriteFile(filepath.Join(o.Home, p), nil, 0o600); err != nil {
				return nil, err
			}
		}
	}
	report.RestoredFiles = len(out)
	rep, _ := learning.MarshalIndent(report)
	if err := os.MkdirAll(filepath.Join(o.Home, ".swarm"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(o.Home, ".swarm", "rehydrate.json"), rep, 0o644); err != nil {
		return nil, err
	}
	if L.RuntimeUID != 0 && os.Geteuid() == 0 {
		if _, err := ChownTree(o.Home, L.RuntimeUID, L.RuntimeGID, nil); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(filepath.Join(o.Home, L.Memory.Memory)); err != nil {
		return nil, errors.New("rehydrate produced no memory file")
	}
	return report, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}
