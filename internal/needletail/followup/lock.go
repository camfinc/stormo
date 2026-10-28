package followup

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lock takes the queue's mutex, then the home's lock file (LockFile, an exclusive flock: serve and
// a oneshot, or any two processes on one home, hold it from reading the file through changing it
// to writing it back, so neither writes over what the other just saved), and reads the file again
// whatever its generation says. The returned func gives both back. Without the lock file (a
// read-only home, a file another user made) the queue still works, guarded by its mutex alone,
// and says so once.
func (q *Queue) lock() (unlock func(), err error) {
	q.mu.Lock()
	unlock = q.flock()
	q.loaded = false // under the lock, the file as it is now
	if err := q.load(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// flock takes the lock file, q.mu held: the returned func gives both back.
func (q *Queue) flock() func() {
	if q.lk == nil {
		f, err := openLock(filepath.Join(q.o.Home, filepath.FromSlash(LockFile)))
		if err != nil {
			if !q.lockWarned {
				q.lockWarned = true
				q.logf("follow-ups: no lock on %s (%v): another process on this home could write over a change", LockFile, err)
			}
			return q.mu.Unlock
		}
		q.lk = f
	}
	fd := int(q.lk.Fd())
	for {
		err := unix.Flock(fd, unix.LOCK_EX)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EINTR) {
			q.logf("follow-ups: could not lock %s: %v", LockFile, err)
			return q.mu.Unlock
		}
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		q.mu.Unlock()
	}
}

func openLock(p string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_RDWR|os.O_CREATE|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
}
