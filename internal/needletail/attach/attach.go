// Package attach is the runtime's store of the files people send the agent (docs/needletail.md
// §4.9, §6): each conversation's uploads in files/<conversation>/, with an index
// (files/<conversation>/.files-v1.json) of what each one is, who sent it and whether it is marked
// (an author who may not instruct the agent sent it: reading it marks the reader, §7). The store
// is the runtime's own (files/ is private: the agent's commands never reach it, the file tools
// refuse it); the model sees an upload through the turn that brought it and view_file. Old
// uploads go after the retention (Sweep).
package attach

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"golang.org/x/sys/unix"
)

// Root is the store's directory in the home.
const Root = "files"

// IndexFile is a conversation's index, in its directory (a persisted format, version 1).
const IndexFile = ".files-v1.json"

// IndexVersion is the index's version.
const IndexVersion = 1

// DefaultRetention is how long an upload is kept when the config says nothing (14 days).
const DefaultRetention = 14 * 24 * time.Hour

// DefaultMaxBytes bounds the whole store (every conversation's uploads): past it the oldest go.
const DefaultMaxBytes = 2 << 30

// MaxIndexBytes is the most a conversation's index may hold: every write keeps it within (the
// oldest transcripts, then the oldest uploads, go first), and an index past it is rebuilt.
const MaxIndexBytes = 8 << 20

// indexSlack is what a new entry is taken to add to its index, reserved against MaxBytes.
const indexSlack = 1 << 10

// MaxFileBytes is the most one stored upload may hold.
const MaxFileBytes = 20 << 20

// Kinds of upload.
const (
	KindImage = "image"
	KindText  = "text"
	KindPDF   = "pdf"
	KindAudio = "audio"
	// KindVideo is a video: kept for a capability job (cap_attach), never shown to the model.
	KindVideo = "video"
)

// MaxVideoBytes is the most one stored video may hold (the store's bound holds as for any upload).
const MaxVideoBytes = 200 << 20

// MaxBytesOf is the most one stored upload of a kind may hold.
func MaxBytesOf(kind string) int64 {
	if kind == KindVideo {
		return MaxVideoBytes
	}
	return MaxFileBytes
}

// Entry is one stored upload.
type Entry struct {
	// ID is the platform's id of the file (Slack's F…), unique in the conversation.
	ID string `json:"id"`
	// Name is the file's name as sent (one line).
	Name string `json:"name"`
	// Path is where it is, relative to the home: files/<conversation>/<id>-<safe name>.
	Path string `json:"path"`
	MIME string `json:"mime"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
	// Width and Height are an image's, when it could be decoded.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Author is who sent it (a Slack user or bot id); Source the platform ("slack").
	Author string `json:"author,omitempty"`
	Source string `json:"source,omitempty"`
	// Marked: its author may not instruct the agent (unverified, a guest, an external user, an
	// unlisted or relay bot); Why says so. Reading it marks the reader's conversation.
	Marked bool   `json:"marked,omitempty"`
	Why    string `json:"why,omitempty"`
	// At is when it was stored (RFC 3339, UTC).
	At string `json:"at"`
	// Transcript is a voice note's transcript, once made.
	Transcript string `json:"transcript,omitempty"`
}

type index struct {
	Version int     `json:"version"`
	Files   []Entry `json:"files"`
}

// Store is the agent's uploads.
type Store struct {
	Home string
	// Retention is how long an upload is kept (0: DefaultRetention); MaxBytes bounds the store
	// (0: DefaultMaxBytes).
	Retention time.Duration
	MaxBytes  int64
	// MaxIndex bounds a conversation's index (0: MaxIndexBytes).
	MaxIndex int
	Now      func() time.Time
	Log      func(string)
	mu       sync.Mutex
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, args...))
	}
}

func (s *Store) retention() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return DefaultRetention
}

func (s *Store) maxBytes() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return DefaultMaxBytes
}

func (s *Store) maxIndex() int {
	if s.MaxIndex > 0 {
		return s.MaxIndex
	}
	return MaxIndexBytes
}

func (s *Store) tmpDir() string { return filepath.Join(s.Home, fsx.TmpDir) }

// Dir is a conversation's directory, relative to the home.
func Dir(conv string) string { return Root + "/" + conv }

// validConv is a conversation id that names one directory.
func validConv(conv string) error {
	if conv == "" || len(conv) > 200 || strings.ContainsAny(conv, "/\\\x00") || conv == "." || conv == ".." || strings.HasPrefix(conv, ".") {
		return fmt.Errorf("no conversation to keep files for")
	}
	return nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SafeName is a file name as stored: letters, digits, dot, dash and underscore, at most 80 bytes,
// never hidden.
func SafeName(name string) string {
	n := strings.Trim(unsafeName.ReplaceAllString(name, "_"), "._")
	if len(n) > 80 {
		ext := path.Ext(n)
		if len(ext) > 10 {
			ext = ""
		}
		n = n[:80-len(ext)] + ext
	}
	if n == "" {
		n = "file"
	}
	return n
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// OneLine is text on one line without control or format characters, at most n runes.
func OneLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

// ensureDir makes files/ and files/<conv>/ (the runtime's own, 0700) and never goes through a
// symlink.
func (s *Store) ensureDir(conv string) error {
	root, err := unix.Open(s.Home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: s.Home, Err: err}
	}
	defer unix.Close(root)
	fd := root
	for _, name := range []string{Root, conv} {
		if err := unix.Mkdirat(fd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return &os.PathError{Op: "mkdir", Path: name, Err: err}
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("%s is not a directory the runtime can use: %w", name, err)
		}
		var st unix.Stat_t
		if unix.Fstat(next, &st) == nil && int(st.Uid) == os.Geteuid() && st.Mode&0o077 != 0 {
			_ = unix.Fchmod(next, 0o700)
		}
		if fd != root {
			unix.Close(fd)
		}
		fd = next
	}
	unix.Close(fd)
	return nil
}

// errUnreadable is an index the store cannot read (damaged, or past its bound): it is rebuilt
// from the files on disk. An index of another version is left as it is (a newer runtime's).
var errUnreadable = errors.New("unreadable index")

// read is a conversation's index (none: empty).
func (s *Store) read(conv string) (index, error) {
	idx := index{Version: IndexVersion}
	b, err := fsx.ReadFileBelow(s.Home, Dir(conv)+"/"+IndexFile, int64(s.maxIndex()))
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if errors.Is(err, fsx.ErrTooBig) {
		return idx, fmt.Errorf("%s: over %d bytes: %w", Dir(conv)+"/"+IndexFile, s.maxIndex(), errUnreadable)
	}
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return index{Version: IndexVersion}, fmt.Errorf("%s: %v: %w", Dir(conv)+"/"+IndexFile, err, errUnreadable)
	}
	if idx.Version != IndexVersion {
		return index{Version: IndexVersion}, fmt.Errorf("%s: version %d (this runtime reads %d)", Dir(conv)+"/"+IndexFile, idx.Version, IndexVersion)
	}
	return idx, nil
}

// load is a conversation's index, rebuilt from the files on disk when it cannot be read (damaged,
// or past MaxIndex): access and retention never stop at a bad index. The caller holds s.mu.
func (s *Store) load(conv string) (index, error) {
	idx, err := s.read(conv)
	if !errors.Is(err, errUnreadable) {
		return idx, err
	}
	s.logf("files: %v: rebuilt from the files on disk (they read as marked: who sent them is no longer known)", err)
	idx = s.rebuild(conv)
	if err := s.write(conv, &idx); err != nil {
		return idx, err
	}
	return idx, nil
}

// rebuild is an index of the uploads in a conversation's directory, from the files themselves:
// each <id>-<name>, its kind sniffed, its time the file's, marked (its sender is not known).
func (s *Store) rebuild(conv string) index {
	idx := index{Version: IndexVersion}
	ents, _ := fsx.ReadDirBelow(s.Home, Dir(conv))
	for _, f := range ents {
		name := f.Name()
		id, rest, ok := strings.Cut(name, "-")
		if !ok || name == IndexFile || strings.HasPrefix(name, ".") || !idRe.MatchString(id) || !f.Type().IsRegular() {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		e := Entry{ID: id, Name: rest, Path: Dir(conv) + "/" + name, Size: info.Size(), At: info.ModTime().UTC().Format(time.RFC3339),
			Marked: true, Why: "its index was rebuilt: who sent it is no longer known"}
		if body, err := fsx.ReadFileBelow(s.Home, e.Path, MaxFileBytes); err == nil {
			e.MIME = Sniff(body)
			switch {
			case strings.HasPrefix(e.MIME, "image/"):
				e.Kind = KindImage
			case e.MIME == "application/pdf":
				e.Kind = KindPDF
			case strings.HasPrefix(e.MIME, "video/"):
				e.Kind = KindVideo
			case strings.HasPrefix(e.MIME, "audio/"):
				e.Kind = KindAudio
			case strings.HasPrefix(e.MIME, "text/"):
				e.Kind = KindText
			}
		}
		idx.Files = append(idx.Files, e)
	}
	slices.SortStableFunc(idx.Files, func(a, b Entry) int { return strings.Compare(a.At, b.At) })
	return idx
}

// write keeps a conversation's index within MaxIndex: while it would be larger, the oldest
// transcript goes, then the oldest upload (and its file); the newest entry is kept. The caller
// holds s.mu.
func (s *Store) write(conv string, idx *index) error {
	idx.Version = IndexVersion
	for {
		b, err := json.MarshalIndent(idx, "", " ")
		if err != nil {
			return err
		}
		if len(b) <= s.maxIndex() {
			return fsx.WriteAtomicBelow(s.Home, Dir(conv)+"/"+IndexFile, s.tmpDir(), b, 0o600)
		}
		dropped := false
		for i := 0; i < len(idx.Files)-1; i++ {
			if idx.Files[i].Transcript != "" {
				idx.Files[i].Transcript = ""
				dropped = true
				break
			}
		}
		if dropped {
			continue
		}
		if len(idx.Files) <= 1 {
			return fmt.Errorf("%s: one entry is over the index's %d bytes", Dir(conv)+"/"+IndexFile, s.maxIndex())
		}
		_ = fsx.RemoveBelow(s.Home, idx.Files[0].Path, false)
		s.logf("files: %s: the index's bound reached: the oldest upload %s removed", Dir(conv), idx.Files[0].ID)
		idx.Files = idx.Files[1:]
	}
}

// usage is the store's size: every kept upload and every index, by conversation's index (read
// or rebuilt). The caller holds s.mu.
func (s *Store) usage() (int64, map[string]index) {
	indexes := map[string]index{}
	var total int64
	convs, _ := fsx.ReadDirBelow(s.Home, Root)
	for _, d := range convs {
		conv := d.Name()
		if !d.IsDir() || validConv(conv) != nil {
			continue
		}
		idx, err := s.load(conv)
		if err != nil {
			continue
		}
		indexes[conv] = idx
		for _, e := range idx.Files {
			total += e.Size
		}
		if raw, err := fsx.ReadFileBelow(s.Home, Dir(conv)+"/"+IndexFile, int64(s.maxIndex())); err == nil {
			total += int64(len(raw))
		}
	}
	return total, indexes
}

// reserve makes room for n more bytes in the store before a write: the oldest uploads (any
// conversation's) go while the store would pass MaxBytes; an upload larger than the store is
// refused. The caller holds s.mu.
func (s *Store) reserve(n int64) error {
	limit := s.maxBytes()
	if n > limit {
		return fmt.Errorf("over the file store's %d bytes", limit)
	}
	total, indexes := s.usage()
	if total+n <= limit {
		return nil
	}
	type kept struct {
		conv string
		e    Entry
	}
	var all []kept
	for conv, idx := range indexes {
		for _, e := range idx.Files {
			all = append(all, kept{conv, e})
		}
	}
	slices.SortStableFunc(all, func(a, b kept) int { return strings.Compare(a.e.At, b.e.At) })
	changed := map[string]bool{}
	for _, k := range all {
		if total+n <= limit {
			break
		}
		if err := fsx.RemoveBelow(s.Home, k.e.Path, false); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		total -= k.e.Size
		idx := indexes[k.conv]
		idx.Files = slices.DeleteFunc(idx.Files, func(e Entry) bool { return e.ID == k.e.ID })
		indexes[k.conv] = idx
		changed[k.conv] = true
		s.logf("files: the store's bound reached: the oldest upload %s (%s) removed", k.e.ID, Dir(k.conv))
	}
	for conv := range changed {
		idx := indexes[conv]
		if err := s.write(conv, &idx); err != nil {
			s.logf("files: %s: index not updated: %v", Dir(conv), err)
		}
	}
	if total+n > limit {
		return fmt.Errorf("the file store is full (%d bytes)", limit)
	}
	return nil
}

// Save stores body as an upload of conversation conv and records it (e.ID, e.Name, e.MIME,
// e.Kind, the author and the mark as given; Path, Size and At are set). A file already stored
// under its id in the conversation is not stored again: its entry is returned.
func (s *Store) Save(conv string, e Entry, body []byte) (Entry, error) {
	if err := validConv(conv); err != nil {
		return Entry{}, err
	}
	if !idRe.MatchString(e.ID) {
		return Entry{}, fmt.Errorf("file id %q is not one the store keeps", e.ID)
	}
	if int64(len(body)) > MaxBytesOf(e.Kind) {
		return Entry{}, fmt.Errorf("over %d bytes", MaxBytesOf(e.Kind))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(conv); err != nil {
		return Entry{}, err
	}
	idx, err := s.load(conv)
	if err != nil {
		return Entry{}, err
	}
	for _, x := range idx.Files {
		if x.ID == e.ID {
			return x, nil
		}
	}
	// the store's bound holds at every write (finding 4): room is made first, the index's growth
	// counted
	if err := s.reserve(int64(len(body)) + indexSlack); err != nil {
		return Entry{}, err
	}
	if idx, err = s.load(conv); err != nil {
		return Entry{}, err
	}
	e.Name = OneLine(e.Name, 200)
	e.Path = Dir(conv) + "/" + e.ID + "-" + SafeName(e.Name)
	e.Size = int64(len(body))
	e.At = s.now().UTC().Format(time.RFC3339)
	if err := fsx.WriteAtomicBelow(s.Home, e.Path, s.tmpDir(), body, 0o600); err != nil {
		return Entry{}, err
	}
	idx.Files = append(idx.Files, e)
	if err := s.write(conv, &idx); err != nil {
		_ = fsx.RemoveBelow(s.Home, e.Path, false)
		return Entry{}, err
	}
	return e, nil
}

// SetTranscript keeps a voice note's transcript in its entry. The index it grows counts against
// the store's bound as an upload does: room is made for the growth first (the transcript it
// replaces counted off; the oldest uploads go), and one the store could not hold even alone with
// its file is refused.
func (s *Store) SetTranscript(conv, id, text string) error {
	if err := validConv(conv); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.load(conv)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(idx.Files, func(e Entry) bool { return e.ID == id })
	if i < 0 {
		return fmt.Errorf("no file %s in this conversation", id)
	}
	idx.Version = IndexVersion
	before, err := json.MarshalIndent(idx, "", " ")
	if err != nil {
		return err
	}
	next := index{Version: IndexVersion, Files: slices.Clone(idx.Files)}
	next.Files[i].Transcript = text
	after, err := json.MarshalIndent(next, "", " ")
	if err != nil {
		return err
	}
	if grow := int64(len(after) - len(before)); grow > 0 {
		if int64(len(after))+idx.Files[i].Size > s.maxBytes() {
			return fmt.Errorf("the transcript is not kept: over the file store's %d bytes", s.maxBytes())
		}
		if err := s.reserve(grow); err != nil {
			return fmt.Errorf("the transcript is not kept: %w", err)
		}
		// reserve may have removed uploads of this conversation (this one too)
		if idx, err = s.load(conv); err != nil {
			return err
		}
		if i = slices.IndexFunc(idx.Files, func(e Entry) bool { return e.ID == id }); i < 0 {
			return fmt.Errorf("no file %s in this conversation (removed to keep the file store within its bound)", id)
		}
	}
	idx.Files[i].Transcript = text
	return s.write(conv, &idx)
}

// List is a conversation's uploads, oldest first.
func (s *Store) List(conv string) ([]Entry, error) {
	if err := validConv(conv); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.load(conv)
	return idx.Files, err
}

// Find is a conversation's upload by id, by name (case-insensitive) or by its path.
func (s *Store) Find(conv, ref string) (Entry, error) {
	files, err := s.List(conv)
	if err != nil {
		return Entry{}, err
	}
	ref = strings.TrimSpace(ref)
	var found []Entry
	for _, e := range files {
		switch {
		case e.ID == ref || e.Path == ref || strings.TrimPrefix(ref, "./") == e.Path:
			return e, nil
		case strings.EqualFold(e.Name, ref) || strings.EqualFold(path.Base(e.Path), ref):
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return Entry{}, fmt.Errorf("no file %q in this conversation (list them with view_file and no id)", ref)
	case 1:
		return found[0], nil
	}
	ids := []string{}
	for _, e := range found {
		ids = append(ids, e.ID)
	}
	return Entry{}, fmt.Errorf("%d files are named %q: name one by id (%s)", len(found), ref, strings.Join(ids, ", "))
}

// Read is an upload's bytes.
func (s *Store) Read(e Entry) ([]byte, error) {
	if !strings.HasPrefix(e.Path, Root+"/") {
		return nil, fmt.Errorf("%s is not in the file store", e.Path)
	}
	return fsx.ReadFileBelow(s.Home, e.Path, MaxBytesOf(e.Kind))
}

// Remove deletes one of a conversation's uploads and its entry (cap_attach consume: the file
// went to the job and is not kept).
func (s *Store) Remove(conv, id string) error {
	if err := validConv(conv); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.load(conv)
	if err != nil {
		return err
	}
	for i, e := range idx.Files {
		if e.ID != id {
			continue
		}
		idx.Files = append(idx.Files[:i], idx.Files[i+1:]...)
		if err := s.write(conv, &idx); err != nil {
			return err
		}
		return fsx.RemoveBelow(s.Home, e.Path, false)
	}
	return fmt.Errorf("no file %s in this conversation", id)
}

// orphanAge is how old a file the index does not name must be before Sweep removes it (a write
// whose index update a crash cut off; a younger one may be a Save still running).
const orphanAge = time.Hour

// Sweep removes the uploads older than the retention (and files the indexes do not name), then
// the oldest ones while the store is over MaxBytes, and conversations' directories left empty.
// It returns how many files it removed.
func (s *Store) Sweep() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	convs, err := fsx.ReadDirBelow(s.Home, Root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	now := s.now()
	cutoff := now.Add(-s.retention())
	removed := 0
	type kept struct {
		conv string
		e    Entry
		at   time.Time
	}
	var all []kept
	indexes := map[string]index{}
	changed := map[string]bool{}
	for _, d := range convs {
		conv := d.Name()
		if !d.IsDir() || validConv(conv) != nil {
			continue
		}
		idx, err := s.load(conv) // an unreadable index is rebuilt; another version's is left
		if err != nil {
			s.logf("files: %s: %v (left as it is)", Dir(conv), err)
			continue
		}
		named := map[string]bool{IndexFile: true}
		keep := idx.Files[:0:0]
		for _, e := range idx.Files {
			at, err := time.Parse(time.RFC3339, e.At)
			if err != nil || at.Before(cutoff) {
				if rmErr := fsx.RemoveBelow(s.Home, e.Path, false); rmErr == nil || errors.Is(rmErr, fs.ErrNotExist) {
					removed++
					changed[conv] = true
					continue
				}
			}
			named[path.Base(e.Path)] = true
			keep = append(keep, e)
			all = append(all, kept{conv, e, at})
		}
		ents, _ := fsx.ReadDirBelow(s.Home, Dir(conv))
		for _, f := range ents {
			if named[f.Name()] {
				continue
			}
			if info, err := f.Info(); err == nil && !f.IsDir() && now.Sub(info.ModTime()) > orphanAge {
				if fsx.RemoveBelow(s.Home, Dir(conv)+"/"+f.Name(), false) == nil {
					removed++
				}
			}
		}
		idx.Files = keep
		indexes[conv] = idx
	}
	// over the store's bound: the oldest go first
	var total int64
	for _, k := range all {
		total += k.e.Size
	}
	slices.SortStableFunc(all, func(a, b kept) int { return a.at.Compare(b.at) })
	for _, k := range all {
		if total <= s.maxBytes() {
			break
		}
		if err := fsx.RemoveBelow(s.Home, k.e.Path, false); err != nil && !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		removed++
		total -= k.e.Size
		idx := indexes[k.conv]
		idx.Files = slices.DeleteFunc(idx.Files, func(e Entry) bool { return e.ID == k.e.ID })
		indexes[k.conv] = idx
		changed[k.conv] = true
	}
	for conv, idx := range indexes {
		if len(idx.Files) == 0 {
			_ = fsx.RemoveBelow(s.Home, Dir(conv)+"/"+IndexFile, false)
			if err := fsx.RemoveBelow(s.Home, Dir(conv), true); err != nil && !errors.Is(err, fs.ErrNotExist) {
				s.logf("files: %s left: %v", Dir(conv), err)
			}
			continue
		}
		if changed[conv] {
			if err := s.write(conv, &idx); err != nil {
				s.logf("files: %s: index not updated: %v", Dir(conv), err)
			}
		}
	}
	return removed, nil
}
