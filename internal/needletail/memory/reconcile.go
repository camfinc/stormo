package memory

import (
	"sort"
	"time"
)

// Reconciled is what reconcile changed.
type Reconciled struct {
	// Created: hot entries no record had (the merge at boot's seed, an entry the dream or an
	// older runtime wrote, or every entry when RECORDS.md was missing: the migration).
	Created int
	// Rewritten: records whose text the hot file has spelled differently (the same normalized
	// text): the hot file wins.
	Rewritten int
	// Demoted: hot records their hot file no longer has, moved to the archive. Without the
	// rehydrate merge report (not built: docs/needletail.md §11) a record a reviewer rejected
	// cannot be told from a nap skew, so neither is deleted.
	Demoted int
	Changed bool
}

// reconcile makes the records agree with the hot files as rehydrate (or anything else) left them:
// the hot files win for the hot set. Each entry, in order, takes the record of its target with
// the same normalized text (a hot one first, else the latest updated archived one) and its
// position; an entry no record has becomes a record (src); a hot record no entry took goes to
// the archive. No path drops a record.
func reconcile(recs []*Record, files map[string][]string, src string, now time.Time) ([]*Record, Reconciled) {
	var rep Reconciled
	byKey := map[string][]*Record{}
	ids := map[string]bool{}
	for _, r := range recs {
		k := r.Target() + "\x00" + r.key()
		byKey[k] = append(byKey[k], r)
		ids[r.ID] = true
	}
	for _, bucket := range byKey {
		sort.SliceStable(bucket, func(i, j int) bool {
			a, b := bucket[i], bucket[j]
			if (a.Hot > 0) != (b.Hot > 0) {
				return a.Hot > 0
			}
			if a.Hot > 0 {
				return a.Hot < b.Hot
			}
			return a.Updated > b.Updated
		})
	}
	used := map[*Record]bool{}
	for _, t := range targets {
		for pos, e := range files[t] {
			var r *Record
			for _, c := range byKey[t+"\x00"+Normalize(e)] {
				if !used[c] {
					r = c
					break
				}
			}
			if r == nil {
				kind := KindFact
				if t == TargetUser {
					kind = KindUser
				}
				r = &Record{ID: newID(now, func(id string) bool { return ids[id] }), Kind: kind, Tags: []string{}, Imp: 3, Src: src,
					Created: stamp(now), Updated: stamp(now), Text: e, PII: PII(e)}
				ids[r.ID] = true
				recs = append(recs, r)
				rep.Created++
			} else if r.Text != e {
				r.Text, r.PII, r.Updated = e, PII(e), stamp(now)
				rep.Rewritten++
			}
			if r.Hot != pos+1 {
				r.Hot = pos + 1
				rep.Changed = true
			}
			used[r] = true
		}
	}
	for _, r := range recs {
		if r.Hot > 0 && !used[r] {
			r.Hot = 0
			rep.Demoted++
		}
	}
	rep.Changed = rep.Changed || rep.Created > 0 || rep.Rewritten > 0 || rep.Demoted > 0
	return recs, rep
}
