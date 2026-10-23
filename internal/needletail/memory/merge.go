package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// MergeFile is rehydrate's memory merge report, relative to the home (pkg/loop.MemoryMergePath, a
// contract, v1; parity-tested): the hot entries the merge at boot took out of the hot files, by
// hash. The store reads it before it reconciles, under the lock, then deletes it.
const MergeFile = ".swarm/memory-merge.json"

// MergeVersion is the report's format this runtime reads.
const MergeVersion = 1

// mergeReport is the report: per target, the entries the merge purged (a reviewer rejected them)
// and deferred (no room), each the SHA-256, in hex, of the entry's Normalize.
type mergeReport struct {
	V      int        `json:"v"`
	Memory mergeLists `json:"memory"`
	User   mergeLists `json:"user"`
}

type mergeLists struct {
	Purged   []string `json:"purged"`
	Deferred []string `json:"deferred"`
}

func (m *mergeReport) lists(target string) mergeLists {
	if target == TargetUser {
		return m.User
	}
	return m.Memory
}

// mergeHash is a normalized text's hash in the report (the whole SHA-256; a tombstone keeps 128 bits).
func mergeHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// readMerge is the home's report: nil when there is none or this runtime cannot read it (a newer
// format, a damaged file: logged, and left for a newer runtime or a person).
func (s *Store) readMerge() *mergeReport {
	body, err := os.ReadFile(filepath.Join(s.Home, filepath.FromSlash(MergeFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.logf("memory: could not read %s: %v", MergeFile, err)
		return nil
	}
	var m mergeReport
	if err := json.Unmarshal(body, &m); err != nil {
		s.logf("memory: %s is not a merge report this runtime reads (%v): left as it is", MergeFile, err)
		return nil
	}
	if m.V != MergeVersion {
		s.logf("memory: %s is format v%d, not this runtime's v%d: left as it is", MergeFile, m.V, MergeVersion)
		return nil
	}
	return &m
}

// applyMerge applies the report to the records before reconcile: every record of a target whose
// text the merge purged (a reviewer rejected it) is deleted and tombstoned, so the agent cannot
// find it and pin it again and a stale hot copy is not made a record again; a hot record whose
// text it deferred (no room) goes to the archive. It returns how many records each touched.
func (st *state) applyMerge(m *mergeReport, now time.Time) (purged, deferred int) {
	for _, t := range targets {
		l := m.lists(t)
		purge, deferSet := distinct(l.Purged), distinct(l.Deferred)
		kept := st.recs[:0:0]
		gone := map[string]bool{}
		for _, r := range st.recs {
			if r.Target() != t {
				kept = append(kept, r)
				continue
			}
			h := mergeHash(r.key())
			switch {
			case purge[h]:
				gone[r.key()] = true
				st.use.drop(r.ID)
				purged++
				continue
			case deferSet[h] && r.Hot > 0:
				r.Hot = 0
				deferred++
			}
			kept = append(kept, r)
		}
		st.recs = kept
		for key := range gone {
			st.bury(t, key, now)
		}
	}
	return purged, deferred
}
