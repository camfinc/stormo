package memory

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// LockFile is the home's memory lock, relative to the home (runtime-private state): serve and a
// oneshot (or any two processes on one home) hold it, exclusively, from reading the files through
// reconciling and changing them to writing them back, so neither writes over what the other just
// saved. It holds nothing; a nap may keep it, empty.
const LockFile = "state/needletail/memory.lock"

// lock takes the store's mutex, then the home's lock file (flock): the returned func gives both
// back. Without the lock file (a read-only home, a file another user made) the store still works,
// guarded by its mutex alone, and says so once.
func (s *Store) lock() func() {
	s.mu.Lock()
	return s.flock()
}

// lockRead is lock for a read (a search, a recall, the hot entries): when the three files are
// still the generation the store holds, it answers from memory under its mutex alone, with no
// flock (a few microseconds on Linux, much more on macOS, on every turn); else it takes the lock
// file too, to read them again.
func (s *Store) lockRead() func() {
	s.mu.Lock()
	if s.st != nil && s.st.prints == s.prints() {
		s.current = true // load needs not look again
		return func() {
			s.current = false
			s.mu.Unlock()
		}
	}
	return s.flock()
}

// flock takes the home's lock file, s.mu held: the returned func gives both back.
func (s *Store) flock() func() {
	if s.lk == nil {
		p := filepath.Join(s.Home, filepath.FromSlash(LockFile))
		f, err := openLock(p)
		if err != nil {
			if !s.lockWarned {
				s.lockWarned = true
				s.logf("memory: no lock on %s (%v): another process on this home could write over a change", LockFile, err)
			}
			return s.mu.Unlock
		}
		s.lk = f
	}
	fd := int(s.lk.Fd())
	for {
		err := unix.Flock(fd, unix.LOCK_EX)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EINTR) {
			s.logf("memory: could not lock %s: %v", LockFile, err)
			return s.mu.Unlock
		}
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		s.mu.Unlock()
	}
}

func openLock(p string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_RDWR|os.O_CREATE|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
}

// fprint is a file's generation: a change by anyone (an atomic rename gives a new inode, a write
// in place a new ctime) shows in it, even one that keeps the size and the mtime.
type fprint struct {
	ok          bool
	size        int64
	mtime, ctim int64 // ns
	dev, ino    uint64
}

func fprintOf(st *unix.Stat_t) fprint {
	return fprint{ok: true, size: st.Size, mtime: st.Mtim.Nano(), ctim: st.Ctim.Nano(), dev: uint64(st.Dev), ino: uint64(st.Ino)}
}

// statPrint is path's generation now (ok false when it is missing).
func statPrint(path string) fprint {
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil {
		return fprint{}
	}
	return fprintOf(&st)
}

// readGen reads path and the generation it read (from the descriptor it read: a rename after
// the read does not pass for what was read). A missing file is no content and no generation.
func readGen(path string) ([]byte, fprint, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, fprint{}, nil
	}
	if err != nil {
		return nil, fprint{}, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return nil, fprint{}, err
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, fprint{}, err
	}
	return body, fprintOf(&st), nil
}

// write writes one of the store's files (rel to the home), atomically.
func (s *Store) write(rel string, body []byte) error {
	if beforeWrite != nil {
		if err := beforeWrite(rel); err != nil {
			return err
		}
	}
	return fsx.WriteAtomicIn(filepath.Join(s.Home, filepath.FromSlash(rel)), s.tmp(), body, 0o640)
}
