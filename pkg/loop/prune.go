package loop

import (
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
)

// Pruning: a nap is working material for the dream. Once the dream has folded it (its id is at or
// below the watermark) it has served its purpose, so it can go, except the newest few: the latest
// one is what rehydrate restores, and a short tail is kept as a safety margin. Stored bodies no
// remaining nap refers to go with them. Nothing the dream has not read yet is ever deleted.

// KeepNaps is how many of the newest naps pruning always keeps.
const KeepNaps = 3

// blobGrace protects bodies written recently: a nap uploads its bodies before its manifest, so a
// body no manifest names yet may belong to a nap being written right now.
const blobGrace = time.Hour

// PruneReport says what a prune removed.
type PruneReport struct {
	Agent string `json:"agent"`
	Naps  int    `json:"naps"`  // manifests deleted
	Blobs int    `json:"blobs"` // bodies deleted (learning, state and raw)
	Bytes int64  `json:"bytes"` // their size, as the deleted manifests recorded it
	Kept  int    `json:"kept"`  // naps left
	// Repaired: naps written during the prune that lost a body to it; they were dropped and
	// latest.json moved back, so the next nap uploads again.
	Repaired int `json:"repaired,omitempty"`
}

// Prune deletes agent's naps the dream has folded, keeping the newest keep (KeepNaps when 0) and
// the one latest.json points at, then the bodies no remaining nap refers to.
func Prune(inst *instance.Instance, agent string, s Store, keep int) (*PruneReport, error) {
	if keep <= 0 {
		keep = KeepNaps
	}
	rep := &PruneReport{Agent: agent}
	w, err := LoadWatermark(inst.Root, agent)
	if err != nil {
		return nil, err
	}
	ids, err := napIDs(s, agent)
	if err != nil {
		return nil, err
	}
	latest := ""
	if n, err := Latest(s, agent); err == nil && n != nil {
		latest = n.ID
	}
	start := time.Now()
	keepSet := map[string]bool{latest: true}
	for i := len(ids) - 1; i >= 0 && i >= len(ids)-keep; i-- {
		keepSet[ids[i]] = true
	}
	for _, id := range ids {
		if keepSet[id] || w.LastNap == nil || id > *w.LastNap {
			continue // kept, or the dream has not read it
		}
		if err := s.Delete(NapKey(agent, id)); err != nil {
			return rep, err
		}
		rep.Naps++
	}

	// Bodies the remaining naps refer to.
	ids, err = napIDs(s, agent)
	if err != nil {
		return rep, err
	}
	rep.Kept = len(ids)
	used := map[string]bool{}
	for _, id := range ids {
		var n Nap
		if ok, err := ReadJSON(s, NapKey(agent, id), &n); err != nil || !ok {
			return rep, err
		}
		for _, f := range n.Files {
			used[BlobKey(agent, f.Class, f.Sha256)] = true
		}
	}
	for _, prefix := range []string{agent + "/blobs/", agent + "/raw/"} {
		keys, err := s.List(prefix)
		if err != nil {
			return rep, err
		}
		for _, k := range keys {
			if used[k] {
				continue
			}
			mod, err := s.Modified(k)
			if err != nil {
				return rep, err
			}
			if mod.IsZero() || time.Since(mod) < blobGrace {
				continue
			}
			if body, _ := s.Get(k); body != nil {
				rep.Bytes += int64(len(body))
			}
			if err := s.Delete(k); err != nil {
				return rep, err
			}
			rep.Blobs++
		}
	}
	if err := repair(s, agent, start, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// napIDs are agent's nap ids, oldest first (ids sort by time).
func napIDs(s Store, agent string) ([]string, error) {
	keys, err := s.List(agent + "/naps/")
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, k := range keys {
		if id, ok := strings.CutSuffix(strings.TrimPrefix(k, agent+"/naps/"), ".json"); ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// repair drops naps written since start that name a missing body (a nap that reused a body the
// prune was deleting), and moves latest.json back to the newest intact nap.
func repair(s Store, agent string, start time.Time, rep *PruneReport) error {
	ids, err := napIDs(s, agent)
	if err != nil {
		return err
	}
	intact := ""
	for _, id := range ids {
		var n Nap
		if ok, err := ReadJSON(s, NapKey(agent, id), &n); err != nil || !ok {
			return err
		}
		complete := true
		for _, f := range n.Files {
			if ok, err := s.Exists(BlobKey(agent, f.Class, f.Sha256)); err != nil {
				return err
			} else if !ok {
				complete = false
				break
			}
		}
		taken, _ := time.Parse(time.RFC3339Nano, n.TakenAt)
		if !complete && !taken.Before(start.Add(-time.Minute)) {
			if err := s.Delete(NapKey(agent, id)); err != nil {
				return err
			}
			rep.Repaired++
			continue
		}
		intact = id
	}
	if rep.Repaired == 0 || intact == "" {
		return nil
	}
	ptr, _ := learning.MarshalCompact(map[string]string{"nap": intact})
	return s.Put(agent+"/latest.json", ptr)
}
