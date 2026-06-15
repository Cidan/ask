package main

import (
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestShellSeams_DefaultUnchanged: project policy requires every
// seam to default to the real function so we never ship a stubbed
// production path.
func TestShellSeams_DefaultUnchanged(t *testing.T) {
	if reflect.ValueOf(syscallGetpgid).Pointer() != reflect.ValueOf(syscall.Getpgid).Pointer() {
		t.Fatal("syscallGetpgid seam defaults away from syscall.Getpgid")
	}
	if reflect.ValueOf(syscallKill).Pointer() != reflect.ValueOf(syscall.Kill).Pointer() {
		t.Fatal("syscallKill seam defaults away from syscall.Kill")
	}
}

// TestUserShell_FallsBackToSh: with $SHELL cleared, userShell must
// return the POSIX fallback /bin/sh.
func TestUserShell_FallsBackToSh(t *testing.T) {
	t.Setenv("SHELL", "")
	if got := userShell(); got != "/bin/sh" {
		t.Errorf("userShell()=%q want /bin/sh", got)
	}
}

// TestUserShell_UsesShellEnv: when $SHELL is set, userShell must
// return it verbatim.
func TestUserShell_UsesShellEnv(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/zsh")
	if got := userShell(); got != "/usr/bin/zsh" {
		t.Errorf("userShell()=%q want /usr/bin/zsh", got)
	}
}

// TestShellSingleQuote covers the POSIX single-quote escaping rule:
// wrap the string in `'…'`, replace every `'` with `'\''` so the
// surrounding shell sees a literal-quote, close, literal-quote-
// escape-quote, re-open pattern.
func TestShellSingleQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"abc", "'abc'"},
		{"it's", "'it'\\''s'"},
		{"'", "''\\'''"},
		{"a'b'c", "'a'\\''b'\\''c'"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := shellSingleQuote(tc.in); got != tc.want {
				t.Errorf("shellSingleQuote(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNextShellStreamCmd_ClosedChannelReturnsNil pins the early
// return: an already-closed empty channel must produce a nil msg
// (so the next-stream cmd is a no-op for Update).
func TestNextShellStreamCmd_ClosedChannelReturnsNil(t *testing.T) {
	ch := make(chan tea.Msg)
	close(ch)
	cmd := nextShellStreamCmd(ch, 1)
	if msg := cmd(); msg != nil {
		t.Errorf("closed channel should yield nil msg; got %T: %+v", msg, msg)
	}
}

// TestNextShellStreamCmd_DrainsUpTo500 covers the brief: the
// command blocks on the first message, then non-blockingly drains
// up to 500 already-queued messages into one shellBatchMsg so
// Update re-renders once per batch.
func TestNextShellStreamCmd_DrainsUpTo500(t *testing.T) {
	ch := make(chan tea.Msg, 600)
	for i := 0; i < 600; i++ {
		ch <- shellLineMsg{text: "line"}
	}
	// Close the channel so the drain terminates at the end.
	close(ch)
	cmd := nextShellStreamCmd(ch, 7)
	msg := cmd()
	batch, ok := msg.(shellBatchMsg)
	if !ok {
		t.Fatalf("expected shellBatchMsg; got %T", msg)
	}
	if batch.tabID != 7 {
		t.Errorf("tabID=%d want 7", batch.tabID)
	}
	if len(batch.lines) != 500 {
		t.Errorf("drain should stop at 500; got %d", len(batch.lines))
	}
	if batch.done != nil {
		t.Errorf("done should be nil when channel is not yet closed at drain end; got %+v", batch.done)
	}
}

// TestNextShellStreamCmd_DoneOnFirstMsg: when the first message is
// a shellDoneMsg, the batch contains no lines and the done field
// is set.
func TestNextShellStreamCmd_DoneOnFirstMsg(t *testing.T) {
	ch := make(chan tea.Msg, 4)
	ch <- shellDoneMsg{input: "cmd", newCwd: "/x", err: nil}
	close(ch)
	cmd := nextShellStreamCmd(ch, 1)
	msg := cmd()
	batch, ok := msg.(shellBatchMsg)
	if !ok {
		t.Fatalf("expected shellBatchMsg; got %T", msg)
	}
	if len(batch.lines) != 0 {
		t.Errorf("done-first should yield zero lines; got %d", len(batch.lines))
	}
	if batch.done == nil {
		t.Fatal("done field should be populated")
	}
	if batch.done.input != "cmd" {
		t.Errorf("done.input=%q want cmd", batch.done.input)
	}
}

// TestNextShellStreamCmd_DoneDuringDrain: lines first, then a
// done — the batch has both, and drain stops at the done boundary
// (does not pull more lines off the closed channel).
func TestNextShellStreamCmd_DoneDuringDrain(t *testing.T) {
	ch := make(chan tea.Msg, 8)
	ch <- shellLineMsg{text: "first"}
	ch <- shellLineMsg{text: "second"}
	ch <- shellDoneMsg{input: "cmd", newCwd: "/x"}
	close(ch)
	cmd := nextShellStreamCmd(ch, 1)
	msg := cmd()
	batch, ok := msg.(shellBatchMsg)
	if !ok {
		t.Fatalf("expected shellBatchMsg; got %T", msg)
	}
	if len(batch.lines) != 2 {
		t.Errorf("want 2 lines; got %d", len(batch.lines))
	}
	if batch.done == nil {
		t.Fatal("done field should be populated when a done is drained")
	}
}

// TestStreamShellPipe_EmitsOnePerLine: every scanner line becomes
// a shellLineMsg; the truncation notice is NOT emitted when
// output stays under the cap.
func TestStreamShellPipe_EmitsOnePerLine(t *testing.T) {
	in := strings.NewReader("a\nb\nc\n")
	ch := make(chan tea.Msg, 8)
	var wg sync.WaitGroup
	wg.Add(1)
	st := &shellStreamState{cap: 100}
	go streamShellPipe(in, ch, false, &wg, st)
	wg.Wait()
	close(ch)

	var got []string
	for msg := range ch {
		lm, ok := msg.(shellLineMsg)
		if !ok {
			t.Errorf("unexpected msg type: %T", msg)
			continue
		}
		got = append(got, lm.text)
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d; got %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d]=%q want %q", i, got[i], w)
		}
	}
}

// TestStreamShellPipe_TruncatesAfterCap: after `cap` lines, the
// stream emits ONE truncation notice and keeps draining without
// emitting more lines.
func TestStreamShellPipe_TruncatesAfterCap(t *testing.T) {
	cap := 3
	// 6 lines, cap=3 → expect 3 line msgs + 1 truncation notice.
	in := strings.NewReader("a\nb\nc\nd\ne\nf\n")
	ch := make(chan tea.Msg, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	st := &shellStreamState{cap: cap}
	go streamShellPipe(in, ch, false, &wg, st)
	wg.Wait()
	close(ch)

	var lines, trunc int
	for msg := range ch {
		lm, ok := msg.(shellLineMsg)
		if !ok {
			continue
		}
		if strings.HasPrefix(lm.text, "… output truncated") {
			trunc++
			continue
		}
		lines++
	}
	if lines != cap {
		t.Errorf("lines=%d want cap=%d (post-cap lines are dropped)", lines, cap)
	}
	if trunc != 1 {
		t.Errorf("truncation notice count=%d want 1", trunc)
	}
}

// TestKillShellProc_NilSafe: nil proc, nil process — must not
// panic.
func TestKillShellProc_NilSafe(t *testing.T) {
	cases := []struct {
		name string
		m    model
	}{
		{"nil proc", model{}},
		{"nil process", model{shellProc: &exec.Cmd{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("killShellProc panicked: %v", r)
				}
			}()
			(&tc.m).killShellProc()
		})
	}
}

// TestKillShellProc_PgroupKillOnHappyPath: with the seam pointed
// at a fake that records its (pid, signal) and pretends Getpgid
// succeeded, killShellProc should call syscall.Kill with the
// negated pid and SIGKILL.
//
// We never call cmd.Start — killShellProc only consults
// m.shellProc.Process.Pid, so a struct literal is enough. This
// keeps the test free of real subprocesses (project policy:
// only git/jj may be spawned).
func TestKillShellProc_PgroupKillOnHappyPath(t *testing.T) {
	prevGet, prevKill := syscallGetpgid, syscallKill
	t.Cleanup(func() {
		syscallGetpgid = prevGet
		syscallKill = prevKill
	})

	var gotPid int
	var gotSig syscall.Signal
	syscallGetpgid = func(pid int) (int, error) { gotPid = pid; return 100, nil }
	syscallKill = func(pid int, sig syscall.Signal) error {
		if pid == -100 && sig == syscall.SIGKILL {
			gotSig = sig
		}
		return nil
	}

	cmd := &exec.Cmd{Process: &os.Process{Pid: 42}}
	m := model{shellProc: cmd}
	(&m).killShellProc()
	if gotPid != 42 {
		t.Errorf("syscallGetpgid received pid=%d want 42", gotPid)
	}
	if gotSig != syscall.SIGKILL {
		t.Errorf("syscallKill sig=%v want SIGKILL", gotSig)
	}
}

// TestKillShellProc_FallsBackToProcessKill: when Getpgid errors,
// the seam records it as "shouldn't reach" and the fallback path
// is taken. We verify by injecting an error in Getpgid and
// asserting the pgroup-Kill seam was NOT consulted — the fallback
// is m.shellProc.Process.Kill.
//
// We never call cmd.Start, so a struct literal with a fake Process
// is sufficient (project policy: only git/jj may be spawned).
func TestKillShellProc_FallsBackToProcessKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pgroup semantics differ on Windows")
	}
	prevGet, prevKill := syscallGetpgid, syscallKill
	t.Cleanup(func() {
		syscallGetpgid = prevGet
		syscallKill = prevKill
	})

	syscallGetpgid = func(_ int) (int, error) { return 0, syscall.EINVAL }
	var killCalled bool
	syscallKill = func(_ int, _ syscall.Signal) error {
		killCalled = true
		return nil
	}

	cmd := &exec.Cmd{Process: &os.Process{Pid: 7}}
	m := model{shellProc: cmd}
	(&m).killShellProc()
	if killCalled {
		t.Error("pgroup-Kill should NOT be called when Getpgid errors; the fallback is Process.Kill")
	}
}

// TestOneShellDone shapes: a one-off failure cmd emits a
// shellBatchMsg with only the done field set, no lines.
func TestOneShellDone(t *testing.T) {
	cmd := oneShellDone(3, "input", "/cwd", io.EOF)
	msg := cmd()
	batch, ok := msg.(shellBatchMsg)
	if !ok {
		t.Fatalf("expected shellBatchMsg; got %T", msg)
	}
	if batch.tabID != 3 {
		t.Errorf("tabID=%d want 3", batch.tabID)
	}
	if batch.done == nil {
		t.Fatal("done should be populated")
	}
	if batch.done.input != "input" {
		t.Errorf("done.input=%q want input", batch.done.input)
	}
	if batch.done.newCwd != "/cwd" {
		t.Errorf("done.newCwd=%q want /cwd", batch.done.newCwd)
	}
	if batch.done.err != io.EOF {
		t.Errorf("done.err=%v want io.EOF", batch.done.err)
	}
}

// TestStreamShellPipe_ChannelNotBlockingOnCapDrain: under the cap
// (no truncation), every line is delivered. This protects the
// drain contract: never block forever waiting for the consumer.
func TestStreamShellPipe_NoBlockOnNormalStream(t *testing.T) {
	// 5 lines, cap=100, channel buffer 8 — if drain is correct
	// the goroutine completes within a short time.
	in := strings.NewReader("a\nb\nc\nd\ne\n")
	ch := make(chan tea.Msg, 8)
	var wg sync.WaitGroup
	wg.Add(1)
	st := &shellStreamState{cap: 100}
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamShellPipe(in, ch, false, &wg, st)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("streamShellPipe did not return within 2s")
	}
}
