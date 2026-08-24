package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bigRaw(n int) string { return strings.Repeat("x\n", n) }

func TestSpillRaw_WritesRecoverableFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(SpillDirEnv, dir)

	raw := "line1\nline2\nline3\n"
	path, lines, err := SpillRaw("go test ./...", raw)
	if err != nil {
		t.Fatalf("SpillRaw: %v", err)
	}
	if lines != 3 {
		t.Errorf("lines = %d, want 3", lines)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("spilled outside ASK_SPILL_DIR: %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != raw {
		t.Errorf("recovered content = %q (err %v), want %q", got, err, raw)
	}
}

// MaybeSpill attaches the recovery path only when the result was lossy and
// the raw was large enough to matter.
func TestMaybeSpill_OnlyWhenLossyAndLarge(t *testing.T) {
	t.Setenv(SpillDirEnv, t.TempDir())

	// Large + lossy → spill.
	raw := bigRaw(SpillThreshold) // well over the byte threshold
	r := BashResult{Output: "go test: 5 passed"}
	MaybeSpill(&r, "go test", raw)
	if r.RawPath == "" || r.RawLines == 0 {
		t.Errorf("lossy large output should spill; got path=%q lines=%d", r.RawPath, r.RawLines)
	}

	// Large but not lossy (visible >= raw) → no spill.
	r2 := BashResult{Output: raw}
	MaybeSpill(&r2, "cmd", raw)
	if r2.RawPath != "" {
		t.Errorf("non-lossy output should not spill: %q", r2.RawPath)
	}

	// Lossy but small → no spill.
	r3 := BashResult{Output: "x"}
	MaybeSpill(&r3, "cmd", "small raw output\n")
	if r3.RawPath != "" {
		t.Errorf("small output should not spill: %q", r3.RawPath)
	}
}

func TestPruneSpills_RemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.txt")
	fresh := filepath.Join(dir, "fresh.txt")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, aged, aged); err != nil {
		t.Fatal(err)
	}

	pruneSpills(dir, SpillPruneAge)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old spill not pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh spill wrongly pruned: %v", err)
	}
}
