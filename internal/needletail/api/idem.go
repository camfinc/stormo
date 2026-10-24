package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// idemTTL is how long a run's Idempotency-Key is remembered.
const idemTTL = 24 * time.Hour

// idemStore is state/needletail/idem.json: the runs started per Idempotency-Key, kept 24 h so a
// retried wake starts one run.
type idemStore struct {
	mu   sync.Mutex // held by the caller across get and put
	path string
	tmp  string // where its temporary file goes (the home's unclaimed temp dir; "": beside it)
	now  func() time.Time
	keys map[string]idemEntry
}

type idemEntry struct {
	wire.RunAccepted
	At time.Time `json:"at"`
}

type idemFile struct {
	V    int                  `json:"v"`
	Keys map[string]idemEntry `json:"keys"`
}

func loadIdem(dir, tmp string, now func() time.Time) (*idemStore, error) {
	if now == nil {
		now = time.Now
	}
	s := &idemStore{now: now, tmp: tmp, keys: map[string]idemEntry{}}
	if dir == "" {
		return s, nil
	}
	s.path = filepath.Join(dir, "idem.json")
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f idemFile
	if json.Unmarshal(raw, &f) == nil && f.Keys != nil {
		s.keys = f.Keys // an unreadable file starts empty: at worst a retried wake runs twice
	}
	s.prune()
	return s, nil
}

func (s *idemStore) prune() {
	cut := s.now().Add(-idemTTL)
	for k, e := range s.keys {
		if e.At.Before(cut) {
			delete(s.keys, k)
		}
	}
}

func (s *idemStore) get(key string) (wire.RunAccepted, bool) {
	e, ok := s.keys[key]
	if !ok || e.At.Before(s.now().Add(-idemTTL)) {
		return wire.RunAccepted{}, false
	}
	return e.RunAccepted, true
}

func (s *idemStore) put(key string, a wire.RunAccepted) error {
	s.keys[key] = idemEntry{RunAccepted: a, At: s.now().UTC()}
	s.prune()
	if s.path == "" {
		return nil
	}
	body, err := json.Marshal(idemFile{V: 1, Keys: s.keys})
	if err != nil {
		return err
	}
	tmp := s.tmp
	if tmp == "" {
		tmp = filepath.Dir(s.path)
	}
	return fsx.WriteAtomicIn(s.path, tmp, append(body, '\n'), 0o600)
}
