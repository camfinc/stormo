// Package learning is the nap/dream loop: hot memory, the PII scrubber, the ledger, compiled
// knowledge, snapshots, the nap store, rehydrate, dream and review.
package learning

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf16"
)

// Hermes hot-memory files (MEMORY.md / USER.md): entries joined by "\n§\n", budgeted in chars of
// the joined string (hermes-agent tools/memory_tool_store.py, ENTRY_DELIMITER).
const EntryDelimiter = "\n§\n"

// unicodeSpace is every Unicode space and line terminator; normalisation and trimming use it
// (Go's \s is ASCII only, and a no-break space must not make two entries differ).
const unicodeSpace = "\u0009\u000a\u000b\u000c\u000d\u0020\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

var spaceRun = regexp.MustCompile(`[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)

// Trim removes leading and trailing unicodeSpace.
func Trim(s string) string { return strings.Trim(s, unicodeSpace) }

// UTF16Len counts UTF-16 code units, the unit the memory budgets are expressed in.
func UTF16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// ParseEntries splits a hot-memory file into its non-empty, trimmed entries.
func ParseEntries(raw string) []string {
	out := []string{}
	for _, e := range strings.Split(raw, EntryDelimiter) {
		if e = Trim(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func SerializeEntries(entries []string) string { return strings.Join(entries, EntryDelimiter) }

// Normalize is the whitespace/case-folded form used to recognise the same entry across instances
// and edits.
func Normalize(entry string) string {
	return strings.Trim(spaceRun.ReplaceAllString(strings.ToLower(entry), " "), " ")
}

// Pack greedily keeps entries in priority order while the joined size stays within limit.
func Pack(entries []string, limit int) (kept, dropped []string) {
	kept, dropped = []string{}, []string{}
	for _, e := range entries {
		if UTF16Len(SerializeEntries(append(append([]string{}, kept...), e))) <= limit {
			kept = append(kept, e)
		} else {
			dropped = append(dropped, e)
		}
	}
	return kept, dropped
}

// MergeHot is the boot-time merge of live memory (from the latest nap) with the compiled seed:
// live entries win; entries a human rejected are purged from live memory; seed entries are added
// only while they fit seedFill of the budget. Whatever does not fit stays in the cold tier skill.
func MergeHot(live, seed []string, rejected map[string]bool, limit int, seedFill float64) (entries, purged, deferred []string) {
	purged, kept := []string{}, []string{}
	for _, e := range live {
		if rejected[Normalize(e)] {
			purged = append(purged, e)
		} else {
			kept = append(kept, e)
		}
	}
	liveKeys := map[string]bool{}
	for _, e := range kept {
		liveKeys[Normalize(e)] = true
	}
	fresh := []string{}
	for _, e := range seed {
		if !liveKeys[Normalize(e)] && !rejected[Normalize(e)] {
			fresh = append(fresh, e)
		}
	}
	liveKept, liveDropped := Pack(kept, limit)
	ceiling := min(limit, int(math.Floor(float64(limit)*seedFill)))
	entries = append([]string{}, liveKept...)
	deferred = append([]string{}, liveDropped...)
	for _, e := range fresh {
		if UTF16Len(SerializeEntries(append(append([]string{}, entries...), e))) <= ceiling {
			entries = append(entries, e)
		} else {
			deferred = append(deferred, e)
		}
	}
	return entries, purged, deferred
}
