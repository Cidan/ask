package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SpillDirEnv overrides the spill directory (tests, sandboxes).
const SpillDirEnv = "ASK_SPILL_DIR"

// SpillThreshold is the raw-output size past which a lossy filter result
// spills the untouched bytes to a file for recovery.
const SpillThreshold = 30 * 1024

// SpillPruneAge is how long spilled files live before the next spill
// opportunistically deletes them.
const SpillPruneAge = 7 * 24 * time.Hour

// SpillDir returns where oversized raw outputs spill: $ASK_SPILL_DIR, else
// $TMPDIR/ask-spill.
func SpillDir() string {
	if d := os.Getenv(SpillDirEnv); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "ask-spill")
}

// SpillRaw writes raw output to a uniquely-named file in SpillDir and
// returns the path plus line count. This is the recovery path for lossy
// filtering: the model pages the untouched bytes back through the read
// tool's offset/limit continuation.
func SpillRaw(command, raw string) (path string, lines int, err error) {
	dir := SpillDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("spill mkdir: %w", err)
	}
	pruneSpills(dir, SpillPruneAge)

	name := fmt.Sprintf("%d-%s.txt", time.Now().UnixNano(), sanitizeSpillName(command))
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		return "", 0, fmt.Errorf("spill write: %w", err)
	}
	return path, countLines(raw), nil
}

// spillIfLossy returns the recovery path and line count for raw when the
// filter was lossy — the visible output is shorter than the raw bytes —
// and raw is large enough to be worth recovering. It returns empty on a
// non-lossy result or any spill error: recovery is best-effort and never
// fails the tool call.
func spillIfLossy(command, raw, visible string) (path string, lines int) {
	if len(raw) <= SpillThreshold || len(visible) >= len(raw) {
		return "", 0
	}
	path, lines, err := SpillRaw(command, raw)
	if err != nil {
		return "", 0
	}
	return path, lines
}

// MaybeSpill records the recovery path on a BashResult when its visible
// Output dropped bytes the raw output had.
func MaybeSpill(r *BashResult, command, raw string) {
	r.RawPath, r.RawLines = spillIfLossy(command, raw, r.Output)
}

func countLines(s string) int {
	n := strings.Count(s, "\n")
	if len(s) > 0 && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// sanitizeSpillName turns a command into a filesystem-safe fragment:
// alphanumerics and -_. survive, everything else becomes _, capped at 48
// runes.
func sanitizeSpillName(cmd string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(cmd) {
		if b.Len() >= 48 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "output"
	}
	return b.String()
}

// pruneSpills deletes spill files older than maxAge. Errors are ignored —
// pruning is opportunistic housekeeping, never a reason to fail a spill.
func pruneSpills(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
