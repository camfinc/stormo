package loop

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
)

// A handoff between places (local .swarm/store ↔ S3) moves an agent's naps to the store the next
// instance rehydrates from. Two things must arrive: the latest nap, complete (raw history
// included), so the agent resumes where it was; and every nap the dream has not folded yet,
// learning files only, because the dream keeps one watermark per agent across both stores. Raw
// blobs (PII) of older naps stay where they are.

type SyncReport struct {
	Latest *string  `json:"latest"`
	Naps   []string `json:"naps"`
	Blobs  int      `json:"blobs"`
}

// NewerDestinationError: the destination ran after the source stopped.
type NewerDestinationError struct{ Msg string }

func (e *NewerDestinationError) Error() string { return e.Msg }

func SyncNaps(from, to Store, agent string, after string, force bool) (*SyncReport, error) {
	r := &SyncReport{Naps: []string{}}
	src, err := Latest(from, agent)
	if err != nil || src == nil {
		return r, err
	}
	dst, err := Latest(to, agent)
	if err != nil {
		return nil, err
	}
	if dst != nil && dst.ID != src.ID && dst.TakenAt > src.TakenAt && !force {
		return nil, &NewerDestinationError{fmt.Sprintf("%s: the destination's latest nap (%s) is newer than the source's (%s); "+
			"it ran there after the source stopped. Pass --force to resume from the source anyway.", agent, dst.TakenAt, src.TakenAt)}
	}
	prefix := agent + "/naps/"
	keys, err := from.List(prefix)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, k := range keys {
		id := strings.TrimSuffix(strings.TrimPrefix(k, prefix), ".json")
		if id == src.ID || after == "" || id > after {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		nap := src
		if id != src.ID {
			nap = &Nap{}
			if ok, err := ReadJSON(from, NapKey(agent, id), nap); err != nil {
				return nil, err
			} else if !ok {
				continue
			}
		}
		classes := []engine.FileClass{engine.Learning}
		if id == src.ID {
			classes = []engine.FileClass{engine.Learning, engine.State, engine.Raw}
		}
		for _, f := range nap.Files {
			if !slices.Contains(classes, f.Class) {
				continue
			}
			key := BlobKey(agent, f.Class, f.Sha256)
			if ok, err := to.Exists(key); err != nil {
				return nil, err
			} else if ok {
				continue
			}
			body, err := from.Get(key)
			if err != nil {
				return nil, err
			}
			if body == nil {
				if id == src.ID {
					return nil, fmt.Errorf("%s: latest nap %s is missing blob %s in the source store", agent, id, key)
				}
				continue // an older copy that never held it; the dream skips unreadable files
			}
			if err := to.Put(key, body); err != nil {
				return nil, err
			}
			r.Blobs++
		}
		if ok, err := to.Exists(NapKey(agent, id)); err != nil {
			return nil, err
		} else if !ok {
			body, _ := learning.MarshalIndent(nap)
			if err := to.Put(NapKey(agent, id), body); err != nil {
				return nil, err
			}
			r.Naps = append(r.Naps, id)
		}
	}
	ptr, _ := learning.MarshalCompact(map[string]string{"nap": src.ID})
	r.Latest = &src.ID
	return r, to.Put(agent+"/latest.json", ptr)
}

// FinalNapWarning: did the stopped instance leave a shutdown nap after since? Otherwise say how
// much may be lost.
func FinalNapWarning(s Store, agent string, since time.Time) (string, error) {
	nap, err := Latest(s, agent)
	if err != nil {
		return "", err
	}
	if nap == nil {
		return agent + ": no nap at all in the source store; the destination starts from the baseline", nil
	}
	taken, _ := time.Parse(time.RFC3339Nano, nap.TakenAt)
	if nap.Reason == "shutdown" && !taken.Before(since) {
		return "", nil
	}
	mins := int(math.Round(since.Sub(taken).Minutes()))
	// Naps skip unchanged snapshots, so this can also mean nothing changed after that nap.
	return fmt.Sprintf("%s: no shutdown nap landed; resuming from the %s nap of %s (~%d min before the stop). Anything learned or said after it, if anything, is lost.",
		agent, nap.Reason, nap.TakenAt, max(mins, 0)), nil
}
