package memory

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RecordsFile is the archive, relative to the home: every memory as a record (docs/needletail.md
// §4.6, §8). It is the runtime's source of truth; the hot files are a rendering of its hot
// records.
const RecordsFile = "memories/RECORDS.md"

// RecordsVersion is RECORDS.md's format (its first entry, the version line).
const RecordsVersion = 1

const versionLine = "<!-- needletail-records v1 -->"

var versionRe = regexp.MustCompile(`^<!-- needletail-records v([0-9]+) -->$`)

// Kinds of record. KindUser renders to USER.md (the user profile, never promoted by the ledger);
// every other kind to MEMORY.md.
const (
	KindFact       = "fact"
	KindPreference = "preference"
	KindProcedure  = "procedure"
	KindEpisode    = "episode"
	KindUser       = "user"
)

// Kinds are the record kinds.
var Kinds = []string{KindFact, KindPreference, KindProcedure, KindEpisode, KindUser}

// Record is one memory. The usage counters (uses, recalls) are not here: they change on reads, so
// they live in state/needletail/memory-usage.json and never rewrite the readable file.
type Record struct {
	ID   string
	Kind string
	Tags []string
	// Imp is the importance, 1 to 5.
	Imp int
	// Hot is the record's position in its hot file (1, 2, …); 0 is the archive.
	Hot int
	// Src is where it came from: chat:<session>, job:<id>, luau:<tool>, migration, boot.
	Src string
	// Created and Updated are RFC 3339 UTC, seconds.
	Created, Updated string
	// Expires is a date (YYYY-MM-DD) after which the record is stale: still searchable, never
	// recalled on its own.
	Expires string
	// PII is the kinds of personal data the generic rules find in Text.
	PII  []string
	Text string

	extra map[string]json.RawMessage // header keys this version does not know, kept
}

// Target is the hot file the record renders to.
func (r *Record) Target() string { return targetOf(r.Kind) }

func targetOf(kind string) string {
	if kind == KindUser {
		return TargetUser
	}
	return TargetMemory
}

// Expired: the record's expiry date is before today (UTC).
func (r *Record) Expired(now time.Time) bool {
	return r.Expires != "" && r.Expires < now.UTC().Format(time.DateOnly)
}

func (r *Record) clone() *Record {
	c := *r
	c.Tags = slices.Clone(r.Tags)
	c.PII = slices.Clone(r.PII)
	return &c
}

var idRe = regexp.MustCompile(`^m[0-9a-z]{11}$`)

// ValidID reports an id in the records' form.
func ValidID(id string) bool { return idRe.MatchString(id) }

// idEpoch is where the ids' time part counts from: six base-36 digits of seconds hold 69 years.
var idEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// newID is m + 6 base-36 digits of the time (so ids sort by creation) + 5 random ones.
func newID(now time.Time, taken func(string) bool) string {
	secs := max(0, now.Unix()-idEpoch.Unix())
	head := strconv.FormatInt(secs, 36)
	if len(head) < 6 {
		head = strings.Repeat("0", 6-len(head)) + head
	}
	head = head[len(head)-6:]
	for {
		n, err := rand.Int(rand.Reader, big.NewInt(60466176)) // 36^5
		if err != nil {
			panic(err)
		}
		tail := strconv.FormatInt(n.Int64(), 36)
		id := "m" + head + strings.Repeat("0", 5-len(tail)) + tail
		if !taken(id) {
			return id
		}
	}
}

func stamp(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// ---- codec ----------------------------------------------------------------------------------

// header is a record's header line as JSON, keys in the contract's order.
func (r *Record) header() string {
	var b bytes.Buffer
	str := func(s string) []byte { v, _ := json.Marshal(s); return v } // HTML-escaped: "-->" cannot end the comment
	b.WriteString(`<!-- {"id":`)
	b.Write(str(r.ID))
	b.WriteString(`,"kind":`)
	b.Write(str(r.Kind))
	b.WriteString(`,"tags":[`)
	for i, t := range r.Tags {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(str(t))
	}
	fmt.Fprintf(&b, `],"imp":%d,"hot":%d,"src":`, r.Imp, r.Hot)
	b.Write(str(r.Src))
	b.WriteString(`,"created":`)
	b.Write(str(r.Created))
	b.WriteString(`,"updated":`)
	b.Write(str(r.Updated))
	if r.Expires != "" {
		b.WriteString(`,"expires":`)
		b.Write(str(r.Expires))
	}
	if len(r.PII) > 0 {
		b.WriteString(`,"pii":[`)
		for i, p := range r.PII {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(str(p))
		}
		b.WriteByte(']')
	}
	keys := make([]string, 0, len(r.extra))
	for k := range r.extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(',')
		b.Write(str(k))
		b.WriteByte(':')
		var c bytes.Buffer
		if json.Compact(&c, r.extra[k]) == nil {
			b.Write(bytes.ReplaceAll(c.Bytes(), []byte("-->"), []byte(`-->`)))
		} else {
			b.WriteString("null")
		}
	}
	b.WriteString("} -->")
	return b.String()
}

// Records is a parsed RECORDS.md.
type Records struct {
	// Version is the file's format (RecordsVersion, or a newer one this runtime cannot write).
	Version int
	Recs    []*Record
	// Bad are entries that could not be read as records, kept verbatim so a rewrite never drops
	// them (a hand edit gone wrong, a torn line): written back after the records.
	Bad []string
}

var headerRe = regexp.MustCompile(`^<!-- (\{.*\}) -->$`)

// ParseRecords reads RECORDS.md. It never fails: an entry it cannot read is kept in Bad. A record
// whose id repeats an earlier one's is kept with a new id (the earlier keeps it).
func ParseRecords(raw string, now time.Time) Records {
	out := Records{Version: RecordsVersion}
	ids := map[string]bool{}
	for i, e := range strings.Split(raw, Delimiter) {
		e = trim(e)
		if e == "" {
			continue
		}
		if i == 0 {
			if m := versionRe.FindStringSubmatch(e); m != nil {
				out.Version, _ = strconv.Atoi(m[1])
				continue
			}
		}
		r, ok := parseRecord(e)
		if !ok {
			out.Bad = append(out.Bad, e)
			continue
		}
		if ids[r.ID] {
			r.ID = newID(now, func(id string) bool { return ids[id] })
		}
		ids[r.ID] = true
		out.Recs = append(out.Recs, r)
	}
	return out
}

func parseRecord(e string) (*Record, bool) {
	line, text, _ := strings.Cut(e, "\n")
	m := headerRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if m == nil {
		return nil, false
	}
	var h map[string]json.RawMessage
	if err := json.Unmarshal([]byte(m[1]), &h); err != nil {
		return nil, false
	}
	r := &Record{Text: trim(text)}
	take := func(k string, v any) bool {
		raw, ok := h[k]
		delete(h, k)
		return !ok || json.Unmarshal(raw, v) == nil
	}
	ok := take("id", &r.ID) && take("kind", &r.Kind) && take("tags", &r.Tags) && take("imp", &r.Imp) && take("hot", &r.Hot) &&
		take("src", &r.Src) && take("created", &r.Created) && take("updated", &r.Updated) && take("expires", &r.Expires) && take("pii", &r.PII)
	if !ok || !ValidID(r.ID) || !slices.Contains(Kinds, r.Kind) || r.Text == "" {
		return nil, false
	}
	if r.Imp < 1 || r.Imp > 5 {
		r.Imp = 3
	}
	r.Hot = max(r.Hot, 0)
	if r.Tags == nil {
		r.Tags = []string{}
	}
	if len(h) > 0 {
		r.extra = h
	}
	return r, true
}

// FormatRecords is RECORDS.md's text: the version line, then each record (its header line and
// text), then the entries that could not be read, joined by the delimiter, with a final newline.
func FormatRecords(recs []*Record, bad []string) string {
	var b strings.Builder
	b.WriteString(versionLine)
	for _, r := range recs {
		b.WriteString(Delimiter)
		b.WriteString(r.header())
		b.WriteByte('\n')
		b.WriteString(r.Text)
	}
	for _, e := range bad {
		b.WriteString(Delimiter)
		b.WriteString(e)
	}
	b.WriteByte('\n')
	return b.String()
}
