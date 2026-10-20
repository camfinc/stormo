// Package memory is the agent's two hot-memory files (docs/needletail.md §4.6):
// memories/MEMORY.md (target "memory") and memories/USER.md (target "user"), entries joined by
// "\n§\n", the same format as Hermes so nap, rehydrate and the dream read both alike. Budgets are
// counted in UTF-16 units of the whole file.
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// Delimiter separates entries (a frozen contract, §8).
const Delimiter = "\n§\n"

// Targets and their files, relative to the home.
const (
	TargetMemory = "memory"
	TargetUser   = "user"
)

var files = map[string]string{TargetMemory: "memories/MEMORY.md", TargetUser: "memories/USER.md"}

// File is a target's path relative to the home.
func File(target string) (string, bool) { f, ok := files[target]; return f, ok }

// Store reads and writes the two files of one home.
type Store struct {
	Home   string
	Limits map[string]int // per target, UTF-16 units; 0 is unlimited

	mu sync.Mutex
}

// Units is s's length in UTF-16 code units.
func Units(s string) int { return len(utf16.Encode([]rune(s))) }

// Parse splits a file into its entries, dropping empty ones.
func Parse(raw string) []string {
	out := []string{}
	for _, e := range strings.Split(raw, Delimiter) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Format joins entries as the file stores them.
func Format(entries []string) string { return strings.Join(entries, Delimiter) }

func (s *Store) path(target string) (string, error) {
	f, ok := files[target]
	if !ok {
		return "", fmt.Errorf("target must be %q or %q", TargetMemory, TargetUser)
	}
	return filepath.Join(s.Home, filepath.FromSlash(f)), nil
}

// Entries is a target's entries.
func (s *Store) Entries(target string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries(target)
}

func (s *Store) entries(target string) ([]string, error) {
	p, err := s.path(target)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(string(raw)), nil
}

// Usage is a target's file size against its budget.
type Usage struct {
	Target  string `json:"target"`
	Entries int    `json:"entries"`
	Used    int    `json:"used"`
	Limit   int    `json:"limit,omitempty"`
}

func (u Usage) String() string {
	if u.Limit > 0 {
		return fmt.Sprintf("%d/%d", u.Used, u.Limit)
	}
	return fmt.Sprint(u.Used)
}

// OverBudget is a change refused because the file would pass its budget.
type OverBudget struct{ Usage Usage }

func (e *OverBudget) Error() string {
	return fmt.Sprintf("%s memory would use %d of %d UTF-16 units: replace or remove entries first", e.Usage.Target, e.Usage.Used, e.Usage.Limit)
}

// Errors.
var (
	ErrNoMatch   = errors.New("no entry contains old_text")
	ErrAmbiguous = errors.New("old_text matches more than one entry: give more of it")
	ErrEmpty     = errors.New("content is empty")
	ErrDelimiter = errors.New("content may not contain the entry separator (a line holding only §)")
)

func (s *Store) usage(target string, entries []string) Usage {
	return Usage{Target: target, Entries: len(entries), Used: Units(Format(entries)), Limit: s.Limits[target]}
}

// Current is a target's usage now.
func (s *Store) Current(target string) (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entries(target)
	if err != nil {
		return Usage{}, err
	}
	return s.usage(target, e), nil
}

// Add appends an entry.
func (s *Store) Add(target, content string) (Usage, error) {
	return s.change(target, func(e []string) ([]string, error) {
		c, err := clean(content)
		if err != nil {
			return nil, err
		}
		for _, x := range e {
			if x == c {
				return e, nil // already there
			}
		}
		return append(e, c), nil
	})
}

// Replace swaps the one entry containing old for content.
func (s *Store) Replace(target, old, content string) (Usage, error) {
	return s.change(target, func(e []string) ([]string, error) {
		c, err := clean(content)
		if err != nil {
			return nil, err
		}
		i, err := match(e, old)
		if err != nil {
			return nil, err
		}
		e[i] = c
		return e, nil
	})
}

// Remove deletes the one entry containing old.
func (s *Store) Remove(target, old string) (Usage, error) {
	return s.change(target, func(e []string) ([]string, error) {
		i, err := match(e, old)
		if err != nil {
			return nil, err
		}
		return append(e[:i], e[i+1:]...), nil
	})
}

func clean(content string) (string, error) {
	c := strings.TrimSpace(content)
	if c == "" {
		return "", ErrEmpty
	}
	if strings.Contains("\n"+c+"\n", Delimiter) {
		return "", ErrDelimiter
	}
	return c, nil
}

func match(entries []string, old string) (int, error) {
	old = strings.TrimSpace(old)
	if old == "" {
		return -1, errors.New("old_text is empty")
	}
	found := -1
	for i, e := range entries {
		if strings.Contains(e, old) {
			if found >= 0 {
				return -1, ErrAmbiguous
			}
			found = i
		}
	}
	if found < 0 {
		return -1, ErrNoMatch
	}
	return found, nil
}

// change applies f and writes the file atomically, unless the result passes the budget (a change
// that shrinks the file is always allowed).
func (s *Store) change(target string, f func([]string) ([]string, error)) (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.path(target)
	if err != nil {
		return Usage{}, err
	}
	before, err := s.entries(target)
	if err != nil {
		return Usage{}, err
	}
	was := s.usage(target, before)
	after, err := f(append([]string(nil), before...))
	if err != nil {
		return was, err
	}
	now := s.usage(target, after)
	if now.Limit > 0 && now.Used > now.Limit && now.Used > was.Used {
		return was, &OverBudget{Usage: now}
	}
	return now, fsx.WriteAtomic(p, []byte(Format(after)), 0o644)
}

// Block is a target's entries as the system prompt shows them ("" when empty).
func (s *Store) Block(target string) string {
	e, err := s.Entries(target)
	if err != nil || len(e) == 0 {
		return ""
	}
	return strings.Join(e, "\n§\n")
}
