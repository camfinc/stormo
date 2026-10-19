package link

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Spool holds requests the core could not take (hook deliveries, queued msg_send calls) on the
// sidecar's disk until it can, oldest first. It is bounded: past MaxItems the oldest are dropped.
type Spool struct {
	Dir      string
	MaxItems int
	mu       sync.Mutex
	seq      int
}

// Item is one held request, sent again verbatim (its signature header still matches the body).
type Item struct {
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"body"`
	At      int64             `json:"at"`
}

func (s *Spool) max() int {
	if s.MaxItems <= 0 {
		return 5000
	}
	return s.MaxItems
}

// Put stores an item.
func (s *Spool) Put(it Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	s.seq++
	it.At = time.Now().UnixMilli()
	b, err := json.Marshal(it)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%019d-%06d.json", time.Now().UnixNano(), s.seq%1000000)
	tmp := filepath.Join(s.Dir, "."+name)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.Dir, name)); err != nil {
		return err
	}
	names := s.names()
	for len(names) > s.max() {
		_ = os.Remove(filepath.Join(s.Dir, names[0]))
		names = names[1:]
	}
	return nil
}

func (s *Spool) names() []string {
	entries, _ := os.ReadDir(s.Dir)
	out := []string{}
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && filepath.Ext(n) == ".json" && n[0] != '.' {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Len is how many items wait.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.names())
}

// Drain sends items oldest first until send fails; sent items are removed. It returns how many went.
func (s *Spool) Drain(send func(Item) error) int {
	s.mu.Lock()
	names := s.names()
	s.mu.Unlock()
	sent := 0
	for _, n := range names {
		p := filepath.Join(s.Dir, n)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var it Item
		if json.Unmarshal(b, &it) != nil {
			_ = os.Remove(p)
			continue
		}
		if err := send(it); err != nil {
			return sent
		}
		_ = os.Remove(p)
		sent++
	}
	return sent
}
