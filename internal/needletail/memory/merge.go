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
// contract, v2; parity-tested): the entries a reviewer rejected that the boot's nap holds, and the
// hot entries the merge deferred for room, by hash. The store reads it before it reconciles, under
// the lock, then deletes it. Rehydrate writes it whole on every boot, so applying it again (a nap
// that caught the home before the runtime read it) must change nothing: it does.
const MergeFile = ".swarm/memory-merge.json"

// MergeVersion is the report's format this runtime writes against; it reads v1 too (an older
// rehydrate: purged is a list of hashes, deleted whatever their age).
const MergeVersion = 2

// mergeReport is the report: per target, the entries a reviewer rejected (with when) and the ones
// deferred (no room), each the SHA-256, in hex, of the entry's Normalize.
type mergeReport struct {
	V      int        `json:"v"`
	Memory mergeLists `json:"memory"`
	User   mergeLists `json:"user"`
}

type mergeLists struct {
	Purged   []mergePurge `json:"purged"`
	Deferred []string     `json:"deferred"`
}

// mergePurge is a rejected entry: v2 {"hash","at"} (at: when a reviewer rejected it, empty when
// unknown), v1 a bare hash.
type mergePurge struct {
	Hash string `json:"hash"`
	At   string `json:"at,omitempty"`
}

func (p *mergePurge) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		*p = mergePurge{}
		return json.Unmarshal(b, &p.Hash)
	}
	type plain mergePurge
	return json.Unmarshal(b, (*plain)(p))
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
	if m.V != 1 && m.V != MergeVersion {
		s.logf("memory: %s is format v%d, not this runtime's v%d: left as it is", MergeFile, m.V, MergeVersion)
		return nil
	}
	return &m
}

// rejections are a target's rejected hashes → when the rejection was made (zero when unknown, or
// unreadable: then every copy goes). A hash named twice keeps the later time.
func rejections(l []mergePurge) map[string]time.Time {
	out := make(map[string]time.Time, len(l))
	for _, p := range l {
		at, err := time.Parse(time.RFC3339Nano, p.At)
		if err != nil {
			at = time.Time{}
		}
		if prev, ok := out[p.Hash]; !ok || (!prev.IsZero() && (at.IsZero() || at.After(prev))) {
			out[p.Hash] = at
		}
	}
	return out
}

// savedBefore is whether r was saved by the time of a rejection made at (always, when at is zero
// or r's time is unreadable). Created has whole seconds, so a record saved in the second of the
// rejection counts as before it: a rejected text is the safer side.
func savedBefore(r *Record, at time.Time) bool {
	if at.IsZero() {
		return true
	}
	created, err := time.Parse(time.RFC3339, r.Created)
	return err != nil || !created.After(at.Truncate(time.Second))
}

// applyMerge applies the report to the records before reconcile: every record of a target whose
// text a reviewer rejected, saved before the rejection, is deleted and tombstoned, so the agent
// cannot find it and pin it again and a stale hot copy is not made a record again; one saved again
// after the rejection (on purpose: a save lifts the tombstone) stays, archived, since rehydrate
// keeps its text out of the hot file on every boot. A hot record whose text was deferred (no room)
// goes to the archive. Each list is a hash set: a long one costs a lookup per record. Applying the
// same report again changes nothing. It returns how many records each touched.
func (st *state) applyMerge(m *mergeReport, now time.Time) (purged, kept, deferred int) {
	for _, t := range targets {
		l := m.lists(t)
		purge, deferSet := rejections(l.Purged), distinct(l.Deferred)
		out := st.recs[:0:0]
		gone := map[string]bool{}
		for _, r := range st.recs {
			if r.Target() != t {
				out = append(out, r)
				continue
			}
			h := mergeHash(r.key())
			at, rejected := purge[h]
			switch {
			case rejected && savedBefore(r, at):
				gone[r.key()] = true
				st.use.drop(r.ID)
				purged++
				continue
			case rejected && r.Hot > 0:
				r.Hot = 0
				kept++
			case deferSet[h] && r.Hot > 0:
				r.Hot = 0
				deferred++
			}
			out = append(out, r)
		}
		st.recs = out
		for key := range gone {
			st.bury(t, key, now)
		}
	}
	return purged, kept, deferred
}
