package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	// Scripts the agent wrote or edited, restored from the nap; KeptRepoScripts are edits dropped
	// because the repo changed that script since the nap's instance booted.
	RestoredScripts []string `json:"restoredScripts"`
	KeptRepoScripts []string `json:"keptRepoScripts"`
}

// MemoryMergePath is rehydrate's memory merge report, relative to the home (a contract, v2;
// ARCHITECTURE.md): the entries a reviewer rejected that the boot's nap holds, by hash, so an
// engine that keeps its own copy of the memories (an archive) can drop them there too, and the
// hot entries the merge deferred for room. Every rehydrate writes it afresh and whole (every
// rejection the nap still holds, not only what this boot purged: a nap taken before the engine
// read the last report must not lose one); an engine may delete it once read. No engine's snapshot
// rules claim .swarm/, so it is never napped.
const MemoryMergePath = ".swarm/memory-merge.json"

// MemoryMergeVersion is the report's format version (MemoryMerge.V). v1's purged was a list of
// hashes, the entries this boot's merge purged.
const MemoryMergeVersion = 2

// MemoryMerge is the report, per hot file (memory, user). Purged are the entries a reviewer
// rejected that the nap holds: the live hot entries the merge purged, in the merge's order, then
// the engine's archived ones (engine.MemoryArchive), each once. Deferred are the entries that did
// not fit the budget (a live entry past it, or a seed entry past the seed fill), in the merge's
// order. Hashes only (MemoryMergeHash): never the text.
type MemoryMerge struct {
	V      int              `json:"v"`
	Memory MemoryMergeLists `json:"memory"`
	User   MemoryMergeLists `json:"user"`
}

// MemoryMergeLists are one hot file's rejected and deferred entries (never nil).
type MemoryMergeLists struct {
	Purged   []MemoryMergeRejection `json:"purged"`
	Deferred []string               `json:"deferred"`
}

// MemoryMergeRejection is a rejected entry: its hash, and when the reviewer rejected it (the ledger
// entry's decidedAt, build.Info.RejectedAt; empty when unknown). An engine deletes the copies saved
// before that time, and all of them when it is empty; one saved again after the rejection is the
// owner's or the agent's on purpose, and stays out of the hot files only.
type MemoryMergeRejection struct {
	Hash string `json:"hash"`
	At   string `json:"at,omitempty"`
}

// MemoryMergeHash is an entry's hash in the report: SHA-256, in hex, of learning.Normalize of the
// entry as the hot file (or the archive) had it (unscrubbed).
func MemoryMergeHash(entry string) string {
	sum := sha256.Sum256([]byte(learning.Normalize(entry)))
	return hex.EncodeToString(sum[:])
}

func mergeHashes(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, MemoryMergeHash(e))
	}
	return out
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
	report := &RehydrateReport{Baseline: info.GitSha, RestoredSkills: []string{}, KeptRepoSkills: []string{}, RestoredScripts: []string{}, KeptRepoScripts: []string{}}
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
	rejectedSkills := info.RejectedSkills.Map()
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
		liveHashes := map[string]string{}
		for _, p := range paths {
			liveHashes[p] = live[p].meta.Sha256
		}
		if h, ok := rejectedSkills[dir]; ok && h == build.SkillHash(liveHashes, paths) {
			if inBaseline {
				report.KeptRepoSkills = append(report.KeptRepoSkills, dir)
			}
			continue // a reviewer rejected this runtime version
		}
		leaf := dir[strings.LastIndex(dir, "/")+1:]
		if !inBaseline && engineOwned[leaf] {
			continue // the image ships its own copy
		}
		if inBaseline {
			if napHash, ok := napSkills[dir]; !ok || napHash != baseHash {
				report.KeptRepoSkills = append(report.KeptRepoSkills, dir)
				continue
			}
			if build.SkillHash(liveHashes, paths) == baseHash {
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

	// 2b. Scripts the agent wrote or edited (any version its instance did not boot with) come back
	// until a reviewer decides, as skills do, so a job it made never outlives the script it runs. A
	// version a reviewer rejected stays out, and so does an edit to a script the repo has changed
	// since that instance booted (the repo's newer version wins; the dream marks the edit stale).
	if L.ScriptsDir != "" && nap != nil && nap.Baseline != nil && nap.Baseline.Scripts != nil {
		rejectedScripts := info.RejectedScripts.Map()
		for _, p := range sortedKeys(live) {
			v := live[p]
			name, ok := strings.CutPrefix(p, L.ScriptsDir+"/")
			booted := nap.Baseline.Scripts[name]
			if !ok || v.meta.Class != engine.Learning || booted == v.meta.Sha256 || rejectedScripts[name] == v.meta.Sha256 {
				continue
			}
			if body, inRepo := base[p]; inRepo && build.Sha256(body) != booted {
				report.KeptRepoScripts = append(report.KeptRepoScripts, name)
				continue
			}
			out[p] = v.body
			report.RestoredScripts = append(report.RestoredScripts, name)
		}
	}

	// 3. Hot memory: live wins, rejected purged, seed fills headroom. The report names every
	// rejected entry the nap holds, the engine's archive included.
	rejected := map[string]bool{}
	for _, id := range info.Rejected {
		rejected[id] = true
	}
	var archived []engine.ArchivedMemory
	if a, ok := o.Engine.(engine.MemoryArchive); ok && len(rejected) > 0 {
		archived = a.ArchivedMemories(func(p string) []byte { return live[p].body })
	}
	merge := MemoryMerge{V: MemoryMergeVersion}
	for _, m := range []struct {
		kind  learning.Kind
		path  string
		limit int
		lists *MemoryMergeLists
	}{{learning.KindMemory, L.Memory.Memory, o.Agent.Learning.MemoryCharLimit, &merge.Memory}, {learning.KindUser, L.Memory.User, o.Agent.Learning.UserCharLimit, &merge.User}} {
		liveEntries := learning.ParseEntries(o.Engine.Memory(), string(live[m.path].body))
		seed := learning.ParseEntries(o.Engine.Memory(), string(base[m.path]))
		rejectedText := map[string]bool{}
		rejectedID := func(e string) (string, bool) {
			text, _ := o.Scrubber.Scrub(e)
			id := learning.LearningID(m.kind, text)
			return id, rejected[id]
		}
		ids := map[string]string{} // normalized rejected text → its ledger id
		for _, e := range liveEntries {
			if id, ok := rejectedID(e); ok {
				rejectedText[learning.Normalize(e)] = true
				ids[learning.Normalize(e)] = id
			}
		}
		entries, purged, deferred := learning.MergeHot(o.Engine.Memory(), liveEntries, seed, rejectedText, m.limit, o.Agent.Learning.SeedFill)
		report.PurgedMemories += len(purged)
		report.DeferredSeed += len(deferred)
		m.lists.Purged, m.lists.Deferred = []MemoryMergeRejection{}, mergeHashes(deferred)
		named := map[string]bool{}
		name := func(e, id string) {
			if h := MemoryMergeHash(e); !named[h] {
				named[h] = true
				m.lists.Purged = append(m.lists.Purged, MemoryMergeRejection{Hash: h, At: info.RejectedAt[id]})
			}
		}
		for _, e := range purged {
			name(e, ids[learning.Normalize(e)])
		}
		for _, a := range archived {
			if a.Target != string(m.kind) || named[MemoryMergeHash(a.Text)] {
				continue
			}
			if id, ok := rejectedID(a.Text); ok {
				name(a.Text, id)
			}
		}
		out[m.path] = []byte(o.Engine.Memory().File(entries))
	}

	// 4. Schedules: merge baseline and runtime-created jobs, less the ones a reviewer rejected.
	if c, ok := live[L.Schedules]; ok && L.Schedules != "" {
		body := c.body
		if d, ok := o.Engine.(engine.ScheduleDropper); ok && len(info.RejectedJobs) > 0 {
			if body, err = d.DropSchedules(body, info.RejectedJobs); err != nil {
				return nil, err
			}
		}
		merged, err := o.Engine.MergeSchedules(base[L.Schedules], body)
		if err != nil {
			return nil, err
		}
		out[L.Schedules] = merged
	}

	// 5. State and raw history come back verbatim.
	// Explicitly selected proposal exports survive reboot too, without broadening the
	// default rule that other Learning files must be reviewed/merged. The runtime's
	// private evidence lives in Raw; its metadata-only outcome export is rebuilt there.
	privateMetadata := map[string]bool{}
	if provider, ok := o.Engine.(engine.PrivateLearningState); ok {
		for _, p := range provider.PrivateLearningPaths() {
			privateMetadata[p] = true
		}
	}
	if provider, ok := o.Engine.(engine.LearningProposals); ok {
		p := provider.LearningProposalsPath()
		if v, found := live[p]; found && v.meta.Class == engine.Learning {
			out[p] = v.body
			privateMetadata[p] = true
		}
	}
	for p, v := range live {
		if v.meta.Class != engine.Learning {
			out[p] = v.body
		}
	}
	privateDirs := map[string]bool{}
	if provider, ok := o.Engine.(engine.PrivateLearningTrees); ok {
		for _, tree := range provider.PrivateLearningDirectories() {
			if tree == "" || filepath.IsAbs(tree) || filepath.ToSlash(filepath.Clean(tree)) != tree || strings.HasPrefix(tree, "../") {
				return nil, fmt.Errorf("invalid private learning directory")
			}
			for p := range out {
				if strings.HasPrefix(p, tree+"/") {
					privateMetadata[p] = true
					for dir := filepath.ToSlash(filepath.Dir(p)); dir == tree || strings.HasPrefix(dir, tree+"/"); dir = filepath.ToSlash(filepath.Dir(dir)) {
						privateDirs[dir] = true
					}
				}
			}
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
		mode := os.FileMode(0o644)
		if privateMetadata[p] {
			mode = 0o600
		}
		if err := os.WriteFile(dst, body, mode); err != nil {
			return nil, err
		}
		if privateMetadata[p] {
			if err := os.Chmod(dst, mode); err != nil {
				return nil, err
			}
		}
	}
	for dir := range privateDirs {
		if err := os.Chmod(filepath.Join(o.Home, dir), 0700); err != nil {
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
	mergeBody, _ := learning.MarshalIndent(merge)
	if err := writeAtomic(filepath.Join(o.Home, MemoryMergePath), mergeBody, 0o600); err != nil {
		return nil, err
	}
	// What this instance booted from, for its naps: the baseline dir can be rebuilt under a running
	// instance (locally it is a bind mount), and a nap must not claim the new one.
	booted, _ := learning.MarshalIndent(NapBaseline{GitSha: info.GitSha, Skills: info.Skills, Settings: bootedSettings(o.Engine, base),
		Scripts: bootedScripts(L, base)})
	if err := os.WriteFile(filepath.Join(o.Home, BootedPath), booted, 0o644); err != nil {
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

// bootedScripts are the repo's scripts in the baseline, by file name (empty, never nil, when it has
// none: nil marks a nap from before they were recorded).
func bootedScripts(L *engine.Layout, base map[string][]byte) map[string]string {
	out := map[string]string{}
	if L.ScriptsDir == "" {
		return out
	}
	for p, body := range base {
		if name, ok := strings.CutPrefix(p, L.ScriptsDir+"/"); ok {
			out[name] = build.Sha256(body)
		}
	}
	return out
}

// bootedSettings are the runtime-changeable settings in the baseline's engine config (empty, never
// nil, when the engine has none: nil marks a nap from before they were recorded).
func bootedSettings(eng engine.Engine, base map[string][]byte) map[string]string {
	out := map[string]string{}
	rs, ok := eng.(engine.RuntimeSettings)
	L := eng.Layout()
	if !ok || L.Config == "" || base[L.Config] == nil {
		return out
	}
	list, err := rs.Settings(base[L.Config])
	if err != nil {
		log.Printf("[rehydrate] reading settings from %s: %v", L.Config, err)
		return out
	}
	for _, s := range list {
		out[s.Key] = s.Value
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}
