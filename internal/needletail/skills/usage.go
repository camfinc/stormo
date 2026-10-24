package skills

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// UsageFile is the skills' usage counts, relative to the home (the engine's Layout.SkillUsage): a
// Learning file the nap keeps, rehydrate restores and the dream copies to the agent's
// learnings/skill-usage.json.
//
// It is an object keyed by skill name whose entries use Hermes' keys, so the file reads the same
// for agents on either engine (and one an agent brings from Hermes keeps counting):
//
//	use_count, view_count, patch_count   how often the skill was used (a view, a file read, a
//	                                     terminal command run from it), its SKILL.md viewed,
//	                                     and changed with skill_manage
//	last_used_at, last_viewed_at, last_patched_at
//	created_at                           first counted, or created by the agent
//	created_by                           "agent" for a skill skill_manage created, else null
//	state                                "active"
//
// Entries are keyed by name, as in Hermes, not by dir: Create refuses a name another skill has,
// but two repo skills with the same front-matter name share one entry.
//
// Times are RFC 3339 with microseconds and +00:00, as Hermes writes them. Keys this runtime does
// not maintain (Hermes' curator fields) are kept as they are.
const UsageFile = "skills/.usage.json"

const usageTime = "2006-01-02T15:04:05.000000+00:00"

type usageEntry = map[string]any

func (l *Library) usagePath() string { return filepath.Join(l.Home, filepath.FromSlash(UsageFile)) }

// Usage is the usage file as it is now (empty when there is none).
func (l *Library) Usage() (map[string]usageEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readUsage()
}

func (l *Library) readUsage() (map[string]usageEntry, error) {
	out := map[string]usageEntry{}
	body, err := os.ReadFile(l.usagePath())
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(body, &out) != nil || out == nil {
		// A file no one can read counts nothing; it is started again rather than left to block
		// every count.
		return map[string]usageEntry{}, nil
	}
	return out, nil
}

// note changes name's entry (made with zero counts when missing) and writes the file atomically.
// The caller holds l.mu.
func (l *Library) note(name string, change func(e usageEntry, now string)) error {
	if name == "" {
		return nil
	}
	all, err := l.readUsage()
	if err != nil {
		return err
	}
	now := l.now().UTC().Format(usageTime)
	e := all[name]
	if e == nil {
		e = usageEntry{"created_at": now, "created_by": nil, "state": "active",
			"use_count": 0, "view_count": 0, "patch_count": 0,
			"last_used_at": nil, "last_viewed_at": nil, "last_patched_at": nil}
		all[name] = e
	}
	if change != nil {
		change(e, now)
	} else {
		delete(all, name)
	}
	body, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteAtomicBelow(l.Home, UsageFile, l.tmp(), append(body, '\n'), fsx.FilePerm)
}

func bump(e usageEntry, key string) {
	n := 0
	switch v := e[key].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	}
	e[key] = n + 1
}

// Counting never fails a tool call: a usage file that cannot be written only loses a count.

// Viewed counts a view of s's SKILL.md (a view is also a use) and makes s the session's current
// skill.
func (l *Library) Viewed(session string, s Skill) {
	l.setViewed(session, s.Dir)
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.note(s.Name, func(e usageEntry, now string) {
		bump(e, "view_count")
		bump(e, "use_count")
		e["last_viewed_at"], e["last_used_at"] = now, now
	})
}

// Used counts a use of s: one of its files read, or a terminal command run from it.
func (l *Library) Used(s Skill) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.note(s.Name, func(e usageEntry, now string) {
		bump(e, "use_count")
		e["last_used_at"] = now
	})
}

// patched counts a change to s (the caller holds l.mu).
func (l *Library) patched(s Skill) {
	_ = l.note(s.Name, func(e usageEntry, now string) {
		bump(e, "patch_count")
		e["last_patched_at"] = now
	})
}

// created records s as the agent's own (the caller holds l.mu).
func (l *Library) created(s Skill) {
	_ = l.note(s.Name, func(e usageEntry, now string) {
		e["created_at"], e["created_by"], e["state"] = now, "agent", "active"
	})
}

// deleted drops s's entry (the caller holds l.mu).
func (l *Library) deleted(s Skill) { _ = l.note(s.Name, nil) }
