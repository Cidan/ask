//go:build windows

package watcher

import (
	"errors"
	"os"
)

func lockExclusive(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }

// tryLockExclusive never grants the lead on Windows: without an advisory
// lock every window would run every watcher.
func tryLockExclusive(*os.File) error {
	return errors.New("watchers are not supported on windows")
}
