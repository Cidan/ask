package watcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Cidan/ask/pkg/herdr"
)

// MaxOutputBytes caps the script output handed to the model; longer output
// keeps its head and tail.
const MaxOutputBytes = 100 * 1024

// Environment a watcher script runs with, on top of ask's own.
const (
	EnvName     = "ASK_WATCHER_NAME"
	EnvStateDir = "ASK_WATCHER_STATE_DIR"
	EnvLastRun  = "ASK_WATCHER_LAST_RUN"
)

// ScriptSpec is one script invocation.
type ScriptSpec struct {
	Dir string
	// Command runs through Shell -c; when empty, Path is executed directly.
	Command string
	Path    string
	Shell   string
	Env     []string
	Timeout time.Duration
}

// ScriptResult is how a script ended and what it printed.
type ScriptResult struct {
	ExitCode int
	// Stdout is what the script printed, cut to MaxOutputBytes;
	// StdoutBytes is how much it printed in all.
	Stdout      string
	StdoutBytes int
	// Stderr is the tail of what the script wrote to stderr.
	Stderr    string
	TimedOut  bool
	Cancelled bool
	// Err is set when the script could not be started.
	Err error
}

// Failed reports whether the check failed: it did not start, timed out,
// was stopped, or exited non-zero.
func (r ScriptResult) Failed() bool {
	return r.Err != nil || r.TimedOut || r.Cancelled || r.ExitCode != 0
}

// FailureText describes a failed check in a few words.
func (r ScriptResult) FailureText() string {
	switch {
	case r.Err != nil:
		return "could not start: " + r.Err.Error()
	case r.TimedOut:
		return "timed out"
	case r.Cancelled:
		return "stopped"
	case r.ExitCode != 0:
		return "exit " + strconv.Itoa(r.ExitCode)
	}
	return ""
}

// SpecFor builds the invocation for d: its command or stored script, in
// its working directory, with its timeout, and the watcher environment —
// its name, its scratch directory (created here), and when it last ran
// (RFC 3339, empty the first time).
func SpecFor(d Def, shell string, lastCheck time.Time) ScriptSpec {
	stateDir := StateDir(d.Ref())
	_ = os.MkdirAll(stateDir, 0o700)
	last := ""
	if !lastCheck.IsZero() {
		last = lastCheck.Format(time.RFC3339)
	}
	spec := ScriptSpec{
		Dir:     d.Cwd,
		Command: strings.TrimSpace(d.Command),
		Shell:   shell,
		Timeout: d.Timeout(),
		Env: []string{
			EnvName + "=" + d.Name,
			EnvStateDir + "=" + stateDir,
			EnvLastRun + "=" + last,
		},
	}
	if spec.Command == "" {
		spec.Path = ScriptPath(d.Ref())
	}
	return spec
}

// RunScript runs one check. Swappable so tests never start a process.
var RunScript = runScript

func runScript(ctx context.Context, spec ScriptSpec) ScriptResult {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var cmd *exec.Cmd
	if spec.Command != "" {
		shell := spec.Shell
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.Command(shell, "-c", spec.Command)
	} else {
		cmd = exec.Command(spec.Path)
	}
	cmd.Dir = spec.Dir
	cmd.Env = append(herdr.WithoutPaneEnv(os.Environ()), spec.Env...)
	stdout := &headTailBuffer{max: MaxOutputBytes}
	stderr := &headTailBuffer{max: MaxStoredStderr, tailOnly: true}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// A script that leaves a child holding its stdout open must not hold
	// the check open once the script itself has exited.
	cmd.WaitDelay = 2 * time.Second
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return ScriptResult{ExitCode: -1, Err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var res ScriptResult
	var waitErr error
	select {
	case waitErr = <-done:
	case <-timer.C:
		killProcessGroup(cmd)
		waitErr = <-done
		res.TimedOut = true
	case <-ctx.Done():
		killProcessGroup(cmd)
		waitErr = <-done
		res.Cancelled = true
	}
	res.ExitCode = 0
	if waitErr != nil {
		res.ExitCode = -1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		}
	}
	res.Stdout = stdout.String()
	res.StdoutBytes = stdout.total
	res.Stderr = stderr.String()
	return res
}

// headTailBuffer keeps the first and last max/2 bytes of everything written
// to it (only the last max with tailOnly), and counts the whole.
type headTailBuffer struct {
	max      int
	tailOnly bool
	head     []byte
	tail     []byte
	total    int
}

func (b *headTailBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	rest := p
	if !b.tailOnly {
		if room := b.max/2 - len(b.head); room > 0 {
			n := min(room, len(rest))
			b.head = append(b.head, rest[:n]...)
			rest = rest[n:]
		}
	}
	keep := b.max
	if !b.tailOnly {
		keep = b.max - b.max/2
	}
	b.tail = append(b.tail, rest...)
	if len(b.tail) > keep {
		b.tail = append(b.tail[:0], b.tail[len(b.tail)-keep:]...)
	}
	return len(p), nil
}

func (b *headTailBuffer) String() string {
	kept := len(b.head) + len(b.tail)
	if kept >= b.total {
		return string(b.head) + string(b.tail)
	}
	// The cuts can split a multi-byte character; drop the pieces.
	tail := strings.ToValidUTF8(string(b.tail), "")
	if b.tailOnly {
		return "[truncated] …\n" + tail
	}
	head := strings.ToValidUTF8(string(b.head), "")
	return head + "\n… [" + strconv.Itoa(b.total-kept) + " bytes cut] …\n" + tail
}
