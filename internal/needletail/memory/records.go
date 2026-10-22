package memory

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	PII []string
	// Scope is who may be shown the record (scope.go): dm:<person> (saved in that person's DM),
	// channel:<channel> (saved in a channel's thread), or agent (everyone). "": saved before
	// scopes existed, read from Src (Store.scopeOf).
	Scope string
	Text  string

	extra map[string]json.RawMessage // header keys this version does not know, kept

	normText, norm  string // Normalize(Text), cached for normText
	piiText         string // the text piiAny was computed for (personal)
	piiDone, piiAny bool
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
	if r.Scope != "" {
		b.WriteString(`,"scope":`)
		jsonString(b, r.Scope)
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
	// Repaired is how many damaged entries were mended (see repair); Renamed how many records got a
	// new id because theirs repeated an earlier one's. Either way the file should be written again.
	Repaired, Renamed int
	// Tombs are the tombstones entry's (TombsEntry).
	Tombs []Tomb
}

// Tomb is a tombstone: a text deleted from target (its record removed, or its text replaced),
// by a hash of its normalized form (TextHash: the text itself is not kept), and when. While it
// lasts, the hot file's entry with that text and no record is a stale copy (a nap that caught an
// older hot file with this RECORDS.md, a crash between the two writes): reconcile drops it from
// the hot file instead of making it a record again. A record saved with the text again lifts it.
type Tomb struct {
	Target string `json:"target"`
	Hash   string `json:"hash"`
	At     string `json:"at"`
}

// Tombstones' bounds: the newest MaxTombstones, none older than TombstoneAge (a nap skew or a
// crash shows at the next boot; a text deleted longer ago that comes back in a hot file is taken
// for one put there on purpose).
const (
	MaxTombstones = 256
	TombstoneAge  = 30 * 24 * time.Hour
)

// TextHash is a text's tombstone hash: 128 bits of SHA-256 over Normalize(text), in hex.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(Normalize(text)))
	return hex.EncodeToString(sum[:16])
}

// tombsRe is the tombstones entry: RECORDS.md's second entry when there are any. An older v1
// runtime keeps it as an entry it cannot read (written back after its records, verbatim), and
// this one reads it wherever it is.
var tombsRe = regexp.MustCompile(`^<!-- needletail-tombstones (\{.*\}) -->$`)

type tombsDoc struct {
	Tombstones []Tomb `json:"tombstones"`
}

func parseTombs(e string) ([]Tomb, bool) {
	m := tombsRe.FindStringSubmatch(e)
	if m == nil {
		return nil, false
	}
	var d tombsDoc
	if json.Unmarshal([]byte(m[1]), &d) != nil {
		return nil, false
	}
	out := d.Tombstones[:0:0]
	for _, t := range d.Tombstones {
		if (t.Target == TargetMemory || t.Target == TargetUser) && len(t.Hash) == 32 {
			if _, err := time.Parse(time.RFC3339, t.At); err == nil {
				out = append(out, t)
			}
		}
	}
	return out, true
}

// pruneTombs drops the tombstones older than TombstoneAge, then all but the newest MaxTombstones.
func pruneTombs(tombs map[string]string, now time.Time) {
	oldest := stamp(now.Add(-TombstoneAge))
	for k, at := range tombs {
		if at < oldest {
			delete(tombs, k)
		}
	}
	if len(tombs) <= MaxTombstones {
		return
	}
	keys := make([]string, 0, len(tombs))
	for k := range tombs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if tombs[keys[i]] != tombs[keys[j]] {
			return tombs[keys[i]] > tombs[keys[j]]
		}
		return keys[i] > keys[j]
	})
	for _, k := range keys[MaxTombstones:] {
		delete(tombs, k)
	}
}

var headerRe = regexp.MustCompile(`^<!-- (\{.*\}) -->$`)

// ParseRecords reads RECORDS.md. It never fails: an entry it cannot read is kept in Bad. A record
// whose id repeats an earlier one's is kept with a new id (the earlier keeps it; Renamed counts
// them). Damage an earlier runtime could leave is mended (Repaired counts it): a bare § line at an
// entry's edge is dropped, and a header with no text followed by a text with no header (a record
// whose text began with a bare § line, read back as two entries) is one record again; a record so
// mended that repeats another's text is a copy a boot made, and dropped.
func ParseRecords(raw string, now time.Time) Records {
	out := Records{Version: RecordsVersion}
	ids := map[string]bool{}
	entries := strings.Split(raw, Delimiter)
	var mended map[*Record]bool
	for i := 0; i < len(entries); i++ {
		e := trim(entries[i])
		if e == "" {
			continue
		}
		if i == 0 {
			if m := versionRe.FindStringSubmatch(e); m != nil {
				out.Version, _ = strconv.Atoi(m[1])
				continue
			}
		}
		if tombs, ok := parseTombs(e); ok {
			out.Tombs = append(out.Tombs, tombs...)
			continue
		}
		if s, changed := stripBare(e); changed {
			out.Repaired++
			if e = s; e == "" {
				continue
			}
		}
		r, ok := parseRecord(e)
		if !ok && headerOnly(e) {
			// the text went to the next entry: join them back
			for j := i + 1; j < len(entries); j++ {
				next, _ := stripBare(trim(entries[j]))
				if next == "" {
					continue
				}
				if !strings.HasPrefix(next, "<!-- ") {
					if r, ok = parseRecord(e + "\n" + next); ok {
						out.Repaired++
						i = j
						if mended == nil {
							mended = map[*Record]bool{}
						}
						mended[r] = true
					}
				}
				break
			}
		}
		if !ok {
			out.Bad = append(out.Bad, e)
			continue
		}
		if ids[r.ID] {
			r.ID = newID(now, func(id string) bool { return ids[id] })
			out.Renamed++
		}
		ids[r.ID] = true
		out.Recs = append(out.Recs, r)
	}
	if len(mended) > 0 {
		seen := map[string]bool{}
		for _, r := range out.Recs {
			if !mended[r] {
				seen[r.Target()+"\x00"+r.key()] = true
			}
		}
		out.Recs = slices.DeleteFunc(out.Recs, func(r *Record) bool {
			if !mended[r] {
				return false
			}
			k := r.Target() + "\x00" + r.key()
			if seen[k] {
				out.Repaired++
				return true
			}
			seen[k] = true
			return false
		})
	}
	return out
}

// headerOnly: e is a record's header line and nothing else.
func headerOnly(e string) bool {
	return !strings.Contains(e, "\n") && headerRe.MatchString(strings.TrimRight(e, "\r"))
}

// bare is the delimiter's § as a line of its own.
const bare = "§"

// stripBare is text without the bare § lines at its edges (trimmed after each), and whether it
// had any. Parse never leaves one inside an entry (it splits there), but an empty entry between
// two delimiters, or a § first or last in a file, leaves one at an edge, and such a line next to
// the delimiter's newlines would read as a delimiter of its own once written into RECORDS.md.
func stripBare(text string) (string, bool) {
	changed := false
	for {
		switch {
		case text == bare:
			return "", true
		case strings.HasPrefix(text, bare+"\n"):
			text = trim(text[len(bare)+1:])
		case strings.HasSuffix(text, "\n"+bare):
			text = trim(text[:len(text)-len(bare)-1])
		default:
			return text, changed
		}
		changed = true
	}
}

// roundTrips: text, written as a record's text (after a newline, before the delimiter's), reads
// back as itself.
func roundTrips(text string) bool { return !strings.Contains("\n"+text+"\n", Delimiter) }

// safeText is text as FormatRecords writes it: a bare § line (no entry point lets one in) becomes
// "(§)", so it can never read back as a delimiter.
func safeText(text string) string {
	if roundTrips(text) {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l == bare {
			lines[i] = "(" + bare + ")"
		}
	}
	return strings.Join(lines, "\n")
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
	Scope   string   `json:"scope"`
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
		for _, k := range []string{"id", "kind", "tags", "imp", "hot", "src", "created", "updated", "expires", "pii", "scope"} {
			delete(all, k)
		}
		if json.Unmarshal([]byte(m[1]), &h) != nil {
			return nil, false
		}
		extra = all
	}
	r := &Record{ID: h.ID, Kind: h.Kind, Tags: h.Tags, Imp: h.Imp, Hot: h.Hot, Src: h.Src, Created: h.Created, Updated: h.Updated,
		Expires: h.Expires, PII: h.PII, Scope: h.Scope, Text: trim(text)}
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

// FormatRecords is RECORDS.md's text: the version line, the tombstones (when there are any), then
// each record (its header line and text), then the entries that could not be read, joined by the
// delimiter, with a final newline.
func FormatRecords(recs []*Record, bad []string, tombs ...Tomb) string {
	var b bytes.Buffer
	size := len(versionLine) + 1
	for _, r := range recs {
		size += len(r.Text) + 200
	}
	b.Grow(size)
	b.WriteString(versionLine)
	if len(tombs) > 0 {
		body, _ := json.Marshal(tombsDoc{Tombstones: tombs}) // HTML-escaped: no "-->" in it
		b.WriteString(Delimiter)
		b.WriteString("<!-- needletail-tombstones ")
		b.Write(body)
		b.WriteString(" -->")
	}
	for _, r := range recs {
		b.WriteString(Delimiter)
		r.header(&b)
		b.WriteByte('\n')
		b.WriteString(safeText(r.Text))
	}
	for _, e := range bad {
		b.WriteString(Delimiter)
		b.WriteString(safeText(e))
	}
	b.WriteByte('\n')
	return b.String()
}
