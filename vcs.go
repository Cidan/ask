package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// vcsCommandTimeout bounds a single git/jj subprocess that runs on ask's
// startup critical path — before the TUI is on screen (pruneWorktrees and
// the memory-tenant root canonicalization in newTab → setCwd). Those
// commands are cheap, read-mostly metadata queries (`git rev-parse
// --git-common-dir`, `git worktree list`, `git worktree remove`, `jj root`,
// …) that return in milliseconds on a healthy repo. The only way one runs
// long is a wedged VCS — a hung fsmonitor/credential helper, an NFS or
// Spotlight stall, a lock held by a crashed process. Unbounded, such a
// stall hangs ask before it ever draws: a blank-at-launch freeze with no
// recourse. (This is the concrete bug it guards: a second `ask` launched in
// a busy checkout while the first was running never opened.) The ceiling is
// generous enough never to trip on a healthy-but-slow machine, tight enough
// that a wedge degrades to "launches a few seconds late, minus
// prune/canonicalization" rather than "hangs forever."
const vcsCommandTimeout = 5 * time.Second

// vcsWaitDelay bounds how long, after the timeout fires and the VCS process
// is killed, Output/CombinedOutput waits for the process's stdout/stderr
// pipes to drain before forcibly closing them and returning. This is
// essential, not cosmetic: exec.CommandContext kills only the direct child,
// but a wedged git often leaves a grandchild holding the pipe open (an
// fsmonitor daemon, a credential helper, a `sleep` in a hook). Without a
// WaitDelay, Output() blocks reading that pipe until the grandchild exits —
// so the command "times out" yet the call still hangs forever, defeating
// the entire guard. WaitDelay makes Wait close the pipes and return. See
// exec.Cmd.WaitDelay (Go 1.20+).
const vcsWaitDelay = 2 * time.Second

// vcsOutput runs `name args...` in dir and returns its stdout, bounded by
// vcsCommandTimeout. Drop-in for exec.Cmd.Output on the startup path.
func vcsOutput(dir, name string, args ...string) ([]byte, error) {
	return runVCS(vcsCommandTimeout, vcsWaitDelay, dir, name, args, false)
}

// vcsCombined is vcsOutput but returns combined stdout+stderr, mirroring
// exec.Cmd.CombinedOutput for callers that fold the tool's stderr into
// their own error message.
func vcsCombined(dir, name string, args ...string) ([]byte, error) {
	return runVCS(vcsCommandTimeout, vcsWaitDelay, dir, name, args, true)
}

// vcsRunner runs a VCS command in dir and returns combined output. It has
// two implementations that differ only in whether a hang is bounded, so a
// shared helper (e.g. jjWorkspaceTargets) can be timeout-guarded on ask's
// startup path yet patient on a foreground action:
//   - vcsCombined          — timeout-bounded; for the startup/prune path,
//     where a wedged VCS must never block the launch.
//   - unboundedVCSCombined — no timeout; for foreground actions like
//     /resume, where a legitimately slow `jj workspace` op must be waited
//     out, not aborted at 5s. A hang there stalls one user-initiated action
//     (Ctrl+C-able), not the whole UI coming up.
type vcsRunner func(dir, name string, args ...string) ([]byte, error)

// unboundedVCSCombined runs a VCS command with no timeout. Used only off the
// startup critical path (see vcsRunner); the bounded vcsCombined is the
// default everywhere prune/launch can reach.
func unboundedVCSCombined(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// runVCS executes a VCS subprocess under a hard timeout. On expiry the
// process is killed (exec.CommandContext sends SIGKILL) and a timeout error
// is returned instead of blocking forever; waitDelay then bounds the pipe
// drain so a grandchild holding the output pipe can't keep the call blocked
// past the timeout. timeout and waitDelay are parameters so tests can
// exercise the deadline path with sub-second values.
func runVCS(timeout, waitDelay time.Duration, dir, name string, args []string, combined bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Run the child in its own process group so the timeout can SIGKILL the
	// whole group, reaping helper grandchildren a wedged VCS spawns
	// (credential helper, hook, fsmonitor) instead of orphaning them to run
	// on. Setpgid alone is safe — the child is not a session leader, so it
	// avoids the Setpgid+Setsid EPERM trap documented in shell.go.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the group (== child pid, since Setpgid made
		// the child its leader). Fall back to the lone process if the group
		// signal fails.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	// Backstop: WaitDelay guarantees Output/CombinedOutput returns even if a
	// fully daemonized escapee (its own session) survives the group kill and
	// keeps the output pipe open (see vcsWaitDelay).
	cmd.WaitDelay = waitDelay
	var (
		out []byte
		err error
	)
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s %s: timed out after %s (vcs unresponsive)", name, strings.Join(args, " "), timeout)
	}
	return out, err
}
