package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	tea "charm.land/bubbletea/v2"
)

const shellOutputCap = 100

type shellLineMsg struct {
	text string
	err  bool
}

type shellDoneMsg struct {
	input  string
	newCwd string
	err    error
}

// shellBatchMsg delivers a run of streamed output (and optionally the trailing
// done signal) to Update in a single render cycle. nextShellStreamCmd drains
// every message currently queued on the shell channel into one batch so large
// outputs don't line-by-line re-render.
type shellBatchMsg struct {
	tabID int
	lines []shellLineMsg
	done  *shellDoneMsg
}

type shellStreamState struct {
	cap    int
	count  atomic.Int64
	marked atomic.Bool
}

func userShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// POSIX single-quote escaping — bash, zsh, and fish all treat single-quoted
// strings as literal, so one helper covers every supported user shell.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// startShellCmd forks $SHELL -c with a `pwd > tmpfile` suffix, streams the
// command's stdout/stderr line-by-line into ask via shellBatchMsg, and finishes
// with shellDoneMsg carrying the subshell's final cwd so `cd` persists.
func (m *model) startShellCmd(input string) tea.Cmd {
	tabID := m.id
	tmp, err := os.CreateTemp("", "ask-shell-cwd-*")
	if err != nil {
		return oneShellDone(tabID, input, "", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()

	wrapped := input + "\npwd > " + shellSingleQuote(tmpPath) + "\n"
	cmd := exec.Command(userShell(), "-c", wrapped)
	// Own process group so we can SIGKILL children (e.g. sleep 100) on cancel.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.Remove(tmpPath)
		return oneShellDone(tabID, input, "", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = os.Remove(tmpPath)
		return oneShellDone(tabID, input, "", err)
	}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(tmpPath)
		return oneShellDone(tabID, input, "", err)
	}

	ch := make(chan tea.Msg, 256)
	m.shellCh = ch
	m.shellProc = cmd

	st := &shellStreamState{cap: shellOutputCap}
	var wg sync.WaitGroup
	wg.Add(2)
	go streamShellPipe(stdout, ch, false, &wg, st)
	go streamShellPipe(stderr, ch, true, &wg, st)
	go func() {
		wg.Wait()
		waitErr := cmd.Wait()
		data, readErr := os.ReadFile(tmpPath)
		_ = os.Remove(tmpPath)
		cwd := ""
		if readErr == nil {
			cwd = strings.TrimRight(string(data), "\n\r")
		}
		ch <- shellDoneMsg{input: input, newCwd: cwd, err: waitErr}
		close(ch)
	}()

	return nextShellStreamCmd(ch, tabID)
}

func streamShellPipe(r io.Reader, ch chan tea.Msg, isErr bool, wg *sync.WaitGroup, st *shellStreamState) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		if st.count.Add(1) > int64(st.cap) {
			// Keep draining so the child doesn't block on a full pipe buffer,
			// but only emit the truncation notice once.
			if st.marked.CompareAndSwap(false, true) {
				ch <- shellLineMsg{
					text: fmt.Sprintf("… output truncated at %d lines", st.cap),
					err:  false,
				}
			}
			continue
		}
		ch <- shellLineMsg{text: sc.Text(), err: isErr}
	}
}

// nextShellStreamCmd blocks on the first queued message, then non-blockingly
// drains everything else that's already available into a single shellBatchMsg
// so Update re-renders once per batch, not once per line.
func nextShellStreamCmd(ch chan tea.Msg, tabID int) tea.Cmd {
	return func() tea.Msg {
		first, ok := <-ch
		if !ok {
			return nil
		}
		batch := shellBatchMsg{tabID: tabID}
		switch v := first.(type) {
		case shellLineMsg:
			batch.lines = append(batch.lines, v)
		case shellDoneMsg:
			batch.done = &v
			return batch
		default:
			return first
		}
	drain:
		for len(batch.lines) < 500 {
			select {
			case msg, ok := <-ch:
				if !ok {
					break drain
				}
				switch v := msg.(type) {
				case shellLineMsg:
					batch.lines = append(batch.lines, v)
				case shellDoneMsg:
					batch.done = &v
					break drain
				}
			default:
				break drain
			}
		}
		return batch
	}
}

func oneShellDone(tabID int, input, cwd string, err error) tea.Cmd {
	return func() tea.Msg {
		return shellBatchMsg{tabID: tabID, done: &shellDoneMsg{input: input, newCwd: cwd, err: err}}
	}
}

// killShellProc SIGKILLs the whole process group so children outlive nothing.
// Safe to call when no shell command is running.
func (m *model) killShellProc() {
	if m.shellProc == nil || m.shellProc.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(m.shellProc.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = m.shellProc.Process.Kill()
	}
}
