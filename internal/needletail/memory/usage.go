package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// UsageFile is the records' counters, relative to the home: ids and numbers only, no text (State,
// under state/needletail/**). They change on reads, so they stay out of the readable RECORDS.md.
const UsageFile = "state/needletail/memory-usage.json"

// usageEvery is how often changed counters are written (and at shutdown, Flush): a crash loses at
// most this much counting.
const usageEvery = time.Minute

// Use is one record's counters. A use is a fetch by id, an update or a pin; a recall (being put
// in a turn's recalled block) is counted apart, so recalled records do not become more recallable.
type Use struct {
	Uses         int    `json:"uses,omitempty"`
	LastUsed     string `json:"last_used,omitempty"`
	Recalls      int    `json:"recalls,omitempty"`
	LastRecalled string `json:"last_recalled,omitempty"`
}

type usageDoc struct {
	V       int             `json:"v"`
	Records map[string]*Use `json:"records"`
}

type usage struct {
	recs    map[string]*Use
	dirty   bool
	written time.Time
}

func loadUsage(home string) *usage {
	u := &usage{recs: map[string]*Use{}}
	body, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(UsageFile)))
	if err != nil {
		return u
	}
	var d usageDoc
	if json.Unmarshal(body, &d) == nil && d.Records != nil {
		for id, x := range d.Records {
			if x != nil && ValidID(id) {
				u.recs[id] = x
			}
		}
	}
	return u
}

func (u *usage) get(id string) *Use {
	x := u.recs[id]
	if x == nil {
		x = &Use{}
		u.recs[id] = x
	}
	return x
}

func (u *usage) used(id string, now time.Time) {
	x := u.get(id)
	x.Uses++
	x.LastUsed = stamp(now)
	u.dirty = true
}

func (u *usage) recalled(id string, now time.Time) {
	x := u.get(id)
	x.Recalls++
	x.LastRecalled = stamp(now)
	u.dirty = true
}

func (u *usage) drop(id string) {
	if _, ok := u.recs[id]; ok {
		delete(u.recs, id)
		u.dirty = true
	}
}

// write saves the counters when they changed and force or usageEvery has passed.
func (u *usage) write(home string, now time.Time, force bool) error {
	if !u.dirty || !force && now.Sub(u.written) < usageEvery {
		return nil
	}
	body, err := json.Marshal(usageDoc{V: 1, Records: u.recs})
	if err != nil {
		return err
	}
	p := filepath.Join(home, filepath.FromSlash(UsageFile))
	if err := fsx.WriteAtomicIn(p, filepath.Join(home, fsx.TmpDir), body, 0o600); err != nil {
		return err
	}
	u.dirty, u.written = false, now
	return nil
}
