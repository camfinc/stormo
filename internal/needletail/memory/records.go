package memory

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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

	normText, norm string // Normalize(Text), cached for normText
}

// key is Normalize(r.Text), computed once per text.
func (r *Record) key() string {
	if r.norm == "" || r.normText != r.Text {
		r.normText, r.norm = r.Text, Normalize(r.Text)
	}
	return r.norm
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

// jsonString appends s as a JSON string, escaped as encoding/json does with HTML escaping ("<",
// ">" and "&" as \u003c…, so "-->" cannot end the header comment; U+2028 and U+2029 too).
func jsonString(b *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
				i++
				continue
			}
			b.WriteString(s[start:i])
			switch c {
			case '"', '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 || r == 0x2028 || r == 0x2029 {
			b.WriteString(s[start:i])
			if r == utf8.RuneError {
				b.WriteString(`\ufffd`)
			} else {
				b.WriteString(`\u202`)
				b.WriteByte(hex[r&0xf])
			}
			i += n
			start = i
			continue
		}
		i += n
	}
	b.WriteString(s[start:])
	b.WriteByte('"')
}

// header is a record's header line as JSON, keys in the contract's order.
func (r *Record) header(b *bytes.Buffer) {
	b.WriteString(`<!-- {"id":`)
	jsonString(b, r.ID)
	b.WriteString(`,"kind":`)
	jsonString(b, r.Kind)
	b.WriteString(`,"tags":[`)
	for i, t := range r.Tags {
		if i > 0 {
			b.WriteByte(',')
		}
		jsonString(b, t)
	}
	b.WriteString(`],"imp":`)
	b.WriteString(strconv.Itoa(r.Imp))
	b.WriteString(`,"hot":`)
	b.WriteString(strconv.Itoa(r.Hot))
	b.WriteString(`,"src":`)
	jsonString(b, r.Src)
	b.WriteString(`,"created":`)
	jsonString(b, r.Created)
	b.WriteString(`,"updated":`)
	jsonString(b, r.Updated)
	if r.Expires != "" {
		b.WriteString(`,"expires":`)
		jsonString(b, r.Expires)
	}
	if len(r.PII) > 0 {
		b.WriteString(`,"pii":[`)
		for i, p := range r.PII {
			if i > 0 {
				b.WriteByte(',')
			}
			jsonString(b, p)
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
		jsonString(b, k)
		b.WriteByte(':')
		var c bytes.Buffer
		if json.Compact(&c, r.extra[k]) == nil {
			b.Write(bytes.ReplaceAll(c.Bytes(), []byte("-->"), []byte(`--\u003e`)))
		} else {
			b.WriteString("null")
		}
	}
	b.WriteString("} -->")
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

// recordHeader is a header's known keys (the fast path: a header with no other key).
type recordHeader struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Tags    []string `json:"tags"`
	Imp     int      `json:"imp"`
	Hot     int      `json:"hot"`
	Src     string   `json:"src"`
	Created string   `json:"created"`
	Updated string   `json:"updated"`
	Expires string   `json:"expires"`
	PII     []string `json:"pii"`
}

func parseRecord(e string) (*Record, bool) {
	line, text, _ := strings.Cut(e, "\n")
	m := headerRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if m == nil {
		return nil, false
	}
	var h recordHeader
	var extra map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(m[1]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		// unknown keys (a newer runtime's): read them apart, to keep them
		var all map[string]json.RawMessage
		if json.Unmarshal([]byte(m[1]), &all) != nil {
			return nil, false
		}
		h = recordHeader{}
		for _, k := range []string{"id", "kind", "tags", "imp", "hot", "src", "created", "updated", "expires", "pii"} {
			delete(all, k)
		}
		if json.Unmarshal([]byte(m[1]), &h) != nil {
			return nil, false
		}
		extra = all
	}
	r := &Record{ID: h.ID, Kind: h.Kind, Tags: h.Tags, Imp: h.Imp, Hot: h.Hot, Src: h.Src, Created: h.Created, Updated: h.Updated,
		Expires: h.Expires, PII: h.PII, Text: trim(text)}
	if !ValidID(r.ID) || !slices.Contains(Kinds, r.Kind) || r.Text == "" {
		return nil, false
	}
	if r.Imp < 1 || r.Imp > 5 {
		r.Imp = 3
	}
	r.Hot = max(r.Hot, 0)
	if r.Tags == nil {
		r.Tags = []string{}
	}
	if len(extra) > 0 {
		r.extra = extra
	}
	return r, true
}

// FormatRecords is RECORDS.md's text: the version line, then each record (its header line and
// text), then the entries that could not be read, joined by the delimiter, with a final newline.
func FormatRecords(recs []*Record, bad []string) string {
	var b bytes.Buffer
	size := len(versionLine) + 1
	for _, r := range recs {
		size += len(r.Text) + 200
	}
	b.Grow(size)
	b.WriteString(versionLine)
	for _, r := range recs {
		b.WriteString(Delimiter)
		r.header(&b)
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
