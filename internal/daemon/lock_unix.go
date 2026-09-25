//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is an exclusive advisory lock on a file, held for as long as the process
// holding it lives.
//
// It exists because "ask whether a daemon is running, then become the daemon" is
// a check followed by an act, and two SessionStart hooks firing in the same
// millisecond both read "nobody is running" before either writes. flock closes
// that window inside the kernel: the lock is taken whole or not at all. It also
// releases on process death by any means, including the SIGKILL that leaves a
// state file behind claiming a daemon is alive, so a lock can never be
// permanently stranded by a crash the way a lock FILE can.
type Lock struct{ f *os.File }

// TryLock takes the lock without waiting, reporting ErrLocked when somebody else
// holds it.
//
// Never blocking is the point. The two callers are a daemon deciding whether it
// is the one that shadows a session and a flush deciding whether it is the one
// delivering right now; in both cases the correct answer to "somebody else has
// it" is to do nothing and let them, not to queue up behind them.
func TryLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("daemon: lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Close releases the lock. The file itself is left in place: unlinking it would
// let a later contender create a fresh inode and lock that instead, which is the
// classic way a lock file stops being a lock.
func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	if err := f.Close(); err != nil {
		return fmt.Errorf("daemon: release lock: %w", err)
	}
	return nil
}
