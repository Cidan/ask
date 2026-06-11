package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runVCS must (a) return at the deadline instead of blocking forever — the
// guarantee that keeps a stalled git/jj from hanging ask's launch — and
// (b) reap the wedged process's descendants rather than orphaning them.
//
// The command forks a `sleep` grandchild that inherits the stdout pipe and
// records its PID. That models a real wedged git, which spawns helpers
// (fsmonitor daemon, credential helper) that hold the pipe open: killing
// only the direct child leaves Output() blocking AND leaks the helper. We
// assert both that runVCS returns promptly and that the grandchild is dead
// afterward (proving the process-group kill, not just WaitDelay, fired).
// (`sh`/`sleep` are the lone non-git subprocesses in the suite — the only
// portable stand-in for a never-returning process; the timings keep it fast.)
func TestRunVCS_TimesOutAndReapsGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := fmt.Sprintf("sleep 30 & echo $! > %s; wait", pidFile)

	start := time.Now()
	out, err := runVCS(80*time.Millisecond, 1*time.Second, "", "sh", []string{"-c", script}, false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a timeout error, got nil (out=%q)", out)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
	// timeout (80ms) + waitDelay (1s) + slack. A regression dropping the
	// kill/WaitDelay would block here until the 30s sleep ends.
	if elapsed > 3*time.Second {
		t.Fatalf("runVCS did not return promptly despite a pipe-holding grandchild: took %s", elapsed)
	}

	pid := readPID(t, pidFile)
	// The grandchild is orphaned to init after sh dies; a process-group kill
	// reaps it within milliseconds. Poll briefly to avoid races; a regression
	// that kills only the direct child leaves it alive for the full 30s.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return // reaped — good
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid %d still alive: process group was not killed on timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	// The grandchild PID is written at sh start; give it a beat to land.
	deadline := time.Now().Add(1 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				pid, perr := strconv.Atoi(s)
				if perr == nil {
					return pid
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild never recorded its pid at %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The happy path returns the command's stdout unchanged so the timeout
// wrapper is transparent to healthy callers.
func TestRunVCS_ReturnsOutputOnSuccess(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	dir := initGitRepo(t)
	out, err := vcsOutput(dir, "git", "rev-parse", "--is-inside-work-tree")
	if err != nil {
		t.Fatalf("vcsOutput: %v", err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("unexpected output: %q", out)
	}
}
