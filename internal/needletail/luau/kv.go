package luau

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// The key-value store (docs/needletail.md §5.3): kv_get (kv:read), kv_set and kv_delete (kv:write).
// One store per agent, in the runtime's private state (state/needletail/kv.json, 0600: the agent's
// commands cannot read it), napped as Raw (a handler can keep message text there, however much the
// docs say not to: restored verbatim, never read by the dream), so it survives restarts and is
// never harvested into git. It holds what tools and handlers need to remember between runs (a
// dedupe hash, a counter, a cursor): it is unscoped (every tool and handler of the agent sees every
// key), so it is no place for client text. Bounded: kvMaxKeys keys of at most KVMaxKey bytes,
// values of at most KVMaxValue bytes as JSON, KVMaxBytes in all (keys and values as the file holds
// them), a ttl of at most KVMaxTTL; expired entries are pruned at every call and dropped from the
// file at the next write. A file that cannot be loaded leaves the store read-only, failing every
// call, until it is fixed: it is never taken for an empty store and written over.

// Bounds of the store.
const (
	KVMaxKey   = 256         // bytes of a key
	KVMaxValue = 16 << 10    // bytes of a value, as JSON
	KVMaxBytes = 2 << 20     // bytes of every key and value together
	KVMaxTTL   = 366 * 86400 // seconds
	// KVFile is the store's file in the home.
	KVFile = "state/needletail/kv.json"
	// kvVersion is the file's "v".
	kvVersion = 1
)

// kvKeysCap is how many keys the store holds; kvMaxKeys the same, a var for tests to lower.
const kvKeysCap = 2000

var kvMaxKeys = kvKeysCap

// kvFileMax is the largest file the store reads: every key and value (KVMaxBytes, as the file
// holds them), the JSON around each of kvKeysCap entries (kvEntryJSON), and a margin. What the
// store writes always fits it.
const (
	kvEntryJSON = 68 // `,"<key>":{"value":<value>,"expires":<20 digits>}` around a key and a value
	kvFileMax   = KVMaxBytes + kvKeysCap*kvEntryJSON + 4<<10
)

// kvEntry is one key's value and, when it expires, when (Unix seconds).
type kvEntry struct {
	Untrusted bool            `json:"untrusted,omitempty"`
	Value     json.RawMessage `json:"value"`
	Expires   int64           `json:"expires,omitempty"`
}

func (e kvEntry) expired(now time.Time) bool { return e.Expires != 0 && now.Unix() >= e.Expires }

// kvFile is kv.json.
type kvFile struct {
	V       int                `json:"v"`
	Entries map[string]kvEntry `json:"entries"`
}

// kvStore is the agent's store as this process holds it: read from the file when the file changed
// since (another process, such as a oneshot beside serve, may write it: the last writer wins),
// written whole and atomically after every change.
type kvStore struct {
	mu      sync.Mutex
	loaded  bool
	gen     string // the file's size and mtime when last read or written
	entries map[string]kvEntry
	// err: the file of generation gen could not be loaded (damaged, another version, too big):
	// every call fails with it, and nothing is written, until the file changes
	err error
}

func fileGen(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", st.ModTime().UnixNano(), st.Size())
}

// load reads the file when it changed since it was last read or written (callers hold mu). The
// entries and the generation are taken only from a file that loaded whole; one that does not
// (damaged, another version, over kvFileMax) is remembered with its error, which every call then
// answers, and nothing writes over it until it changes. A read that failed (I/O) is tried again
// at the next call.
func (s *kvStore) load(home string) error {
	p := filepath.Join(home, filepath.FromSlash(KVFile))
	gen := fileGen(p)
	if s.loaded && gen == s.gen {
		return s.err
	}
	if gen == "" {
		s.entries, s.gen, s.loaded, s.err = map[string]kvEntry{}, gen, true, nil
		return nil
	}
	refuse := func(err error) error {
		s.entries, s.gen, s.loaded, s.err = nil, gen, true, err
		return err
	}
	body, err := fsx.ReadFileBelow(home, KVFile, kvFileMax)
	switch {
	case errors.Is(err, fsx.ErrTooBig):
		return refuse(fmt.Errorf("the store's file is over %d bytes: it is left as it is, and the store is read-only until it is fixed", kvFileMax))
	case err != nil:
		s.entries, s.loaded = nil, false
		return fmt.Errorf("the store's file could not be read: %v", err)
	}
	var f kvFile
	if err := json.Unmarshal(body, &f); err != nil {
		return refuse(errors.New("the store's file is damaged: it is left as it is, and the store is read-only until it is fixed"))
	}
	if f.V != kvVersion {
		return refuse(fmt.Errorf("the store's file is version %d, this runtime reads %d: it is left as it is", f.V, kvVersion))
	}
	if f.Entries == nil {
		f.Entries = map[string]kvEntry{}
	}
	s.entries, s.gen, s.loaded, s.err = f.Entries, gen, true, nil
	return nil
}

// prune drops the expired entries (callers hold mu).
func (s *kvStore) prune(now time.Time) {
	for k, e := range s.entries {
		if e.expired(now) {
			delete(s.entries, k)
		}
	}
}

// kvEncode is v as the store writes it: JSON with no HTML escaping (a '<' stays one byte), no
// trailing newline.
func kvEncode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// kvKeySize is the bytes a key takes in the file (quoted and escaped as kvEncode writes it).
func kvKeySize(key string) int {
	b, _ := kvEncode(key)
	return len(b)
}

// save writes the store whole through the home's temp dir (callers hold mu).
func (s *kvStore) save(home string) error {
	if s.err != nil {
		return s.err
	}
	body, err := kvEncode(kvFile{V: kvVersion, Entries: s.entries})
	if err != nil {
		return err
	}
	p := filepath.Join(home, filepath.FromSlash(KVFile))
	if err := fsx.WriteAtomicIn(p, filepath.Join(home, fsx.TmpDir), body, 0o600); err != nil {
		return err
	}
	s.gen = fileGen(p)
	return nil
}

// size is the bytes of every key and value as the file holds them (callers hold mu).
func (s *kvStore) size() int {
	n := 0
	for k, e := range s.entries {
		n += kvKeySize(k) + len(e.Value)
	}
	return n
}

// kv is the run's store, once its capability is checked.
func (c *Call) kv(binding, op string) (*kvStore, error) {
	if !c.Caps.KV(op) {
		return nil, c.deny(binding, "kv:"+op)
	}
	if c.rt.Home == "" {
		return nil, errf("unavailable", "%s: the store is not available", binding)
	}
	return &c.rt.kv, nil
}

// kvKey reads a call's key: a string of 1 to KVMaxKey bytes of UTF-8 with no control character.
func kvKey(binding string, raw any) (string, error) {
	k, ok := raw.(string)
	switch {
	case !ok || k == "":
		return "", errf("bad_args", "%s needs a key (a non-empty string)", binding)
	case len(k) > KVMaxKey:
		return "", errf("bad_args", "%s: a key is at most %d bytes", binding, KVMaxKey)
	case !utf8.ValidString(k):
		return "", errf("bad_args", "%s: the key is not UTF-8", binding)
	}
	for _, r := range k {
		if r < 0x20 || r == 0x7f {
			return "", errf("bad_args", "%s: the key has a control character", binding)
		}
	}
	return k, nil
}

func kvGet(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	st, err := c.kv("kv.get", "read")
	if err != nil {
		return nil, err
	}
	var a struct {
		Key any `json:"key"`
	}
	_ = json.Unmarshal(in, &a)
	key, err := kvKey("kv_get", a.Key)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.load(c.rt.Home); err != nil {
		return nil, errf("error", "kv_get: %v", err)
	}
	st.prune(c.rt.now())
	e, ok := st.entries[key]
	if !ok {
		return nil, nil
	}
	if e.Untrusted {
		if err := messaging.MarkUntrusted(ctx, "a Luau key-value entry contains untrusted data"); err != nil {
			return nil, err
		}
	}
	return e.Value, nil
}

func kvSet(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	st, err := c.kv("kv.set", "write")
	if err != nil {
		return nil, err
	}
	var a struct {
		Key   any             `json:"key"`
		Value json.RawMessage `json:"value"`
		TTL   any             `json:"ttl_seconds"`
	}
	_ = json.Unmarshal(in, &a)
	key, err := kvKey("kv_set", a.Key)
	if err != nil {
		return nil, err
	}
	value := bytes.TrimSpace(a.Value)
	if len(value) == 0 || string(value) == "null" {
		return nil, errf("bad_args", "kv_set needs a value (kv_delete removes a key)")
	}
	// the value as the file will hold it (compact, written with no HTML escaping), which the
	// bounds measure
	if v, err := kvEncode(json.RawMessage(value)); err == nil {
		value = v
	} else {
		return nil, errf("bad_args", "kv_set: the value is not JSON: %v", err)
	}
	if len(value) > KVMaxValue {
		return nil, errf("too_large", "kv_set: a value is at most %d bytes as JSON (this one is %d)", KVMaxValue, len(value))
	}
	if err := c.noSecret("kv_set", key, string(value)); err != nil {
		return nil, err
	}
	now := c.rt.now()
	var expires int64
	if a.TTL != nil {
		ttl, ok := a.TTL.(float64)
		if !ok || math.IsNaN(ttl) || ttl <= 0 || ttl > KVMaxTTL {
			return nil, errf("bad_args", "kv_set: ttl_seconds is a number of seconds, more than 0 and at most %d", KVMaxTTL)
		}
		expires = now.Add(time.Duration(ttl * float64(time.Second))).Unix()
		if expires <= now.Unix() {
			expires = now.Unix() + 1
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.load(c.rt.Home); err != nil {
		return nil, errf("error", "kv_set: %v", err)
	}
	st.prune(now)
	old, had := st.entries[key]
	if !had && len(st.entries) >= kvMaxKeys {
		return nil, errf("full", "kv_set: the store holds %d keys, its most; kv_delete some or give them a ttl", kvMaxKeys)
	}
	size := st.size() + kvKeySize(key) + len(value)
	if had {
		size -= kvKeySize(key) + len(old.Value)
	}
	if size > KVMaxBytes {
		return nil, errf("full", "kv_set: the store would hold over %d bytes; kv_delete some or give them a ttl", KVMaxBytes)
	}
	st.entries[key] = kvEntry{Value: append(json.RawMessage(nil), value...), Expires: expires, Untrusted: c.quarantined(ctx) != ""}
	if err := st.save(c.rt.Home); err != nil {
		if had {
			st.entries[key] = old
		} else {
			delete(st.entries, key)
		}
		return nil, errf("error", "kv_set: %v", err)
	}
	return nil, nil
}

func kvDelete(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	st, err := c.kv("kv.delete", "write")
	if err != nil {
		return nil, err
	}
	var a struct {
		Key any `json:"key"`
	}
	_ = json.Unmarshal(in, &a)
	key, err := kvKey("kv_delete", a.Key)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.load(c.rt.Home); err != nil {
		return nil, errf("error", "kv_delete: %v", err)
	}
	st.prune(c.rt.now())
	old, had := st.entries[key]
	if !had {
		return false, nil
	}
	if why := c.quarantined(ctx); why != "" && !old.Untrusted {
		return nil, errf("refused", "%v", messaging.Deferred("kv_delete of an unmarked key", why))
	}
	delete(st.entries, key)
	if err := st.save(c.rt.Home); err != nil {
		st.entries[key] = old
		return nil, errf("error", "kv_delete: %v", err)
	}
	return true, nil
}

// hashSHA256 is the hex SHA-256 of a string (no grant: it reads and reaches nothing).
func hashSHA256(_ context.Context, _ *Call, in json.RawMessage) (any, error) {
	var a struct {
		Text any `json:"text"`
	}
	_ = json.Unmarshal(in, &a)
	s, ok := a.Text.(string)
	if !ok {
		return nil, errf("bad_args", "sha256 hashes a string")
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:]), nil
}
