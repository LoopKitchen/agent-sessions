//go:build !unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Lock is a best-effort exclusive claim for platforms without flock.
//
// The agent ships for macOS and Linux, so this exists to keep the package
// buildable rather than to be relied on. It is weaker in one specific way and it
// is worth naming: the claim is an exclusive file creation, so a process killed
// while holding it strands the file and nothing can take the lock until somebody
// deletes it. On unix the kernel releases the lock when the holder dies and that
// failure cannot happen.
type Lock struct{ path string }

// TryLock takes the lock without waiting, reporting ErrLocked when the claim
// file already exists.
func TryLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("daemon: open lock %s: %w", path, err)
	}
	_ = f.Close()
	return &Lock{path: path}, nil
}

// Close releases the lock by removing the claim file.
func (l *Lock) Close() error {
	if l == nil || l.path == "" {
		return nil
	}
	path := l.path
	l.path = ""
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("daemon: release lock: %w", err)
	}
	return nil
}
