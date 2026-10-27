package watcher

import (
	"errors"
	"os"
	"path/filepath"
)

const leadLockName = "watchers.lock"

var errLockHeld = errors.New("lock held by another process")

// Lead is held by the one ask process that runs watchers. It is an
// advisory lock on ~/.config/ask/watchers.lock kept for as long as the
// process leads; the kernel drops it when the process dies, however it
// dies, so there is never a stale lead to clean up.
type Lead struct {
	f *os.File
}

// TryLead takes the lead without waiting. It returns (nil, nil) when
// another process holds it.
func TryLead() (*Lead, error) {
	base := configDir()
	if base == "" {
		return nil, errors.New("no home directory for the watcher lock")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(base, leadLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLockExclusive(f); err != nil {
		f.Close()
		if errors.Is(err, errLockHeld) {
			return nil, nil
		}
		return nil, err
	}
	return &Lead{f: f}, nil
}

// Release gives the lead up.
func (l *Lead) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
}
