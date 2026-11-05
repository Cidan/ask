package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/engine"
)

func TestResolveAgentShell(t *testing.T) {
	lookPath := func(paths map[string]string) func(string) (string, error) {
		return func(name string) (string, error) {
			if p, ok := paths[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		}
	}
	getenv := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}

	tests := []struct {
		name    string
		paths   map[string]string
		environ map[string]string
		want    string
	}{
		{
			name:    "prefers bash over zsh and $SHELL",
			paths:   map[string]string{"bash": "/usr/bin/bash", "zsh": "/usr/bin/zsh"},
			environ: map[string]string{"SHELL": "/usr/bin/fish"},
			want:    "/usr/bin/bash",
		},
		{
			name:    "falls back to zsh when bash missing",
			paths:   map[string]string{"zsh": "/usr/bin/zsh"},
			environ: map[string]string{"SHELL": "/usr/bin/fish"},
			want:    "/usr/bin/zsh",
		},
		{
			name:    "uses $SHELL when neither bash nor zsh present",
			paths:   map[string]string{},
			environ: map[string]string{"SHELL": "/usr/bin/fish"},
			want:    "/usr/bin/fish",
		},
		{
			name:    "ignores empty lookPath result and falls through",
			paths:   map[string]string{"bash": ""},
			environ: map[string]string{"SHELL": "/usr/bin/fish"},
			want:    "/usr/bin/fish",
		},
		{
			name:    "falls back to /bin/sh when nothing is available",
			paths:   map[string]string{},
			environ: map[string]string{},
			want:    "/bin/sh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveAgentShell(lookPath(tt.paths), getenv(tt.environ))
			if got != tt.want {
				t.Errorf("ResolveAgentShell() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBashToolSync(t *testing.T) {
	env, _ := newTestToolEnv(t)
	oldRun := RunShell
	defer func() { RunShell = oldRun }()

	RunShell = func(dir, command string, extraEnv ...string) (*ShellHandle, error) {
		out := make(chan string, 1)
		done := make(chan ShellResult, 1)
		out <- "command executed\n"
		close(out)
		done <- ShellResult{ExitCode: 0}
		return &ShellHandle{Output: out, Done: done, Kill: func() {}}, nil
	}

	tool := BashTool(env)
	resp := runTool(t, tool, BashParams{Command: "echo hello"})
	if resp.IsError {
		t.Fatalf("bash failed: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "command executed") {
		t.Errorf("expected output, got: %s", resp.Content)
	}
}

func TestBashToolBackground(t *testing.T) {
	env, events := newTestToolEnvSnapshot(t)
	oldRun := RunShell
	defer func() { RunShell = oldRun }()

	doneCh := make(chan struct{})
	RunShell = func(dir, command string, extraEnv ...string) (*ShellHandle, error) {
		out := make(chan string, 2)
		done := make(chan ShellResult, 1)
		go func() {
			out <- "first chunk\n"
			<-doneCh
			out <- "second chunk\n"
			close(out)
			done <- ShellResult{ExitCode: 0}
		}()
		return &ShellHandle{
			Output: out,
			Done:   done,
			Kill:   func() { close(doneCh) },
		}, nil
	}

	bashTool := BashTool(env)
	outputTool := JobOutputTool(env)
	killTool := JobKillTool(env)

	resp := runTool(t, bashTool, BashParams{Command: "long task", RunInBackground: true})
	if resp.IsError {
		t.Fatalf("bg bash failed: %s", resp.Content)
	}
	jobID := "job-1"

	// Check output while still running
	time.Sleep(20 * time.Millisecond)
	resp = runTool(t, outputTool, JobOutputParams{JobID: jobID})
	if resp.IsError || !strings.Contains(resp.Content, "first chunk") {
		t.Errorf("expected first chunk, got: %s", resp.Content)
	}

	// Kill job
	resp = runTool(t, killTool, JobKillParams{JobID: jobID})
	if resp.IsError {
		t.Fatalf("kill failed: %s", resp.Content)
	}

	// The ended event is emitted after the job is marked done, which is
	// what job_kill waits for.
	startedFound := false
	endedFound := false
	for deadline := time.Now().Add(2 * time.Second); !endedFound && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		for _, ev := range events() {
			if _, ok := ev.(engine.BgTaskStartedEvent); ok {
				startedFound = true
			}
			if _, ok := ev.(engine.BgTaskEndedEvent); ok {
				endedFound = true
			}
		}
	}
	if !startedFound || !endedFound {
		t.Errorf("expected started and ended bg task events, got: %+v", events())
	}
}

func setShellOutputGrace(t *testing.T, d time.Duration) {
	t.Helper()
	prev := shellOutputGrace
	shellOutputGrace = d
	t.Cleanup(func() { shellOutputGrace = prev })
}

func requireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not on PATH")
	}
}

// detachedPid reads the pid a test command wrote to path and kills that
// process when the test ends: it left the shell's process group, so nothing
// else will.
func detachedPid(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		b, _ := os.ReadFile(path)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			return pid
		}
	}
	t.Fatalf("the command never wrote a pid to %s", path)
	return 0
}

// collectShell reads h to the end and returns its output and result,
// failing when either is still open after within.
func collectShell(t *testing.T, h *ShellHandle, within time.Duration) (string, ShellResult) {
	t.Helper()
	var b strings.Builder
	deadline := time.After(within)
	for {
		select {
		case chunk, ok := <-h.Output:
			if !ok {
				select {
				case res := <-h.Done:
					return b.String(), res
				case <-deadline:
					t.Fatalf("output closed but no result after %s", within)
				}
			}
			b.WriteString(chunk)
		case <-deadline:
			h.Kill()
			t.Fatalf("output still open after %s; read so far: %q", within, b.String())
		}
	}
}

func TestRunShellProcess_ReturnsWhileADetachedProcessHoldsTheOutput(t *testing.T) {
	requireSetsid(t)
	setShellOutputGrace(t, 100*time.Millisecond)
	pidfile := filepath.Join(t.TempDir(), "pid")
	h, err := RunShellProcess(t.TempDir(), `setsid sleep 30 & echo $! > "$PIDFILE"; echo started`, "PIDFILE="+pidfile)
	if err != nil {
		t.Fatal(err)
	}
	detachedPid(t, pidfile)
	out, res := collectShell(t, h, 5*time.Second)
	if res.ExitCode != 0 || res.Err != nil {
		t.Errorf("result = %+v, want the shell's own exit 0", res)
	}
	if !strings.HasPrefix(out, "started\n") {
		t.Errorf("output = %q, want what the command printed first", out)
	}
	if !strings.Contains(out, "run_in_background") {
		t.Errorf("output = %q, want the note that the output was still held", out)
	}
}

func TestRunShellProcess_KillsALeftoverChildThatHoldsTheOutput(t *testing.T) {
	setShellOutputGrace(t, 100*time.Millisecond)
	pidfile := filepath.Join(t.TempDir(), "pid")
	h, err := RunShellProcess(t.TempDir(), `sleep 30 & echo $! > "$PIDFILE"; echo started`, "PIDFILE="+pidfile)
	if err != nil {
		t.Fatal(err)
	}
	child := detachedPid(t, pidfile)
	out, res := collectShell(t, h, 5*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want the shell's own exit 0", res.ExitCode)
	}
	if !strings.Contains(out, "run_in_background") {
		t.Errorf("output = %q, want the note that the output was still held", out)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if err := syscall.Kill(child, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the backgrounded child %d is still alive after the call returned", child)
		}
	}
}

func TestRunShellProcess_KillReturnsWhileADetachedProcessHoldsTheOutput(t *testing.T) {
	requireSetsid(t)
	setShellOutputGrace(t, 100*time.Millisecond)
	pidfile := filepath.Join(t.TempDir(), "pid")
	h, err := RunShellProcess(t.TempDir(), `setsid sleep 30 & echo $! > "$PIDFILE"; sleep 30`, "PIDFILE="+pidfile)
	if err != nil {
		t.Fatal(err)
	}
	detachedPid(t, pidfile)
	h.Kill()
	_, res := collectShell(t, h, 5*time.Second)
	if res.ExitCode != -1 {
		t.Errorf("exit = %d, want -1 for a shell killed by a signal", res.ExitCode)
	}
}

func TestBashTool_CancelReturnsWhileADetachedProcessHoldsTheOutput(t *testing.T) {
	requireSetsid(t)
	setShellOutputGrace(t, 100*time.Millisecond)
	env, _ := newTestToolEnv(t)
	pidfile := filepath.Join(t.TempDir(), "pid")
	args, err := json.Marshal(BashParams{
		Command:     `setsid sleep 30 & echo $! > '` + pidfile + `'; sleep 30`,
		Description: "hold the output open",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan ToolResponse, 1)
	go func() {
		resp, _ := RunToolWithJSON(engine.NewStandaloneAgentContext(ctx), BashTool(env), string(args))
		got <- resp
	}()
	detachedPid(t, pidfile)
	cancel()
	select {
	case resp := <-got:
		if !strings.Contains(resp.Content, `"cancelled":true`) {
			t.Errorf("response = %s, want a cancelled result", resp.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bash tool did not return after its turn was cancelled")
	}
}

func TestRunShellProcess_RunsInItsOwnSessionWithoutATerminal(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	h, err := RunShellProcess(t.TempDir(), `read -r _ _ _ _ pgrp sid _ < /proc/$$/stat; echo "$pgrp $sid"; { : </dev/tty; } 2>/dev/null && echo tty || echo no-tty`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := collectShell(t, h, 5*time.Second)
	if want := fmt.Sprintf("%d %d\nno-tty\n", h.Pid, h.Pid); out != want {
		t.Errorf("output = %q, want %q: the shell leads its own session and process group and cannot open a terminal", out, want)
	}
}

func TestRunShellProcess_StreamsOutputVerbatim(t *testing.T) {
	const long = 2 << 20
	h, err := RunShellProcess(t.TempDir(), fmt.Sprintf(`printf 'crlf\r\n'; head -c %d /dev/zero | tr '\0' a; printf '\nlast'`, long))
	if err != nil {
		t.Fatal(err)
	}
	out, res := collectShell(t, h, 5*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if want := "crlf\n" + strings.Repeat("a", long) + "\nlast\n"; out != want {
		t.Errorf("output is %d bytes starting %q, want %d bytes: a line longer than the read buffer arrives whole, CRLF ends a line like LF, and an unterminated last line is ended", len(out), out[:min(len(out), 16)], len(want))
	}
}

func TestValidateSudoCommand(t *testing.T) {
	tests := []struct {
		cmd   string
		valid bool
	}{
		{"sudo -A apt update", true},
		{"sudo --askpass make install", true},
		{"sudo apt update", false},
		{"echo hi && sudo -A cat /etc/shadow", true},
		{"echo hi && sudo cat /etc/shadow", false},
		{"FOO=1 sudo -A systemctl restart nginx", true},
		{"FOO=1 sudo systemctl restart nginx", false},
	}

	for _, tt := range tests {
		err := ValidateSudoCommand(tt.cmd)
		if (err == nil) != tt.valid {
			t.Errorf("ValidateSudoCommand(%q) valid=%v, want %v (err=%v)", tt.cmd, err == nil, tt.valid, err)
		}
	}
}

func TestValidateSudoCommand_FollowsSudosOptionGrammar(t *testing.T) {
	tests := []struct {
		cmd   string
		valid bool
	}{
		{"sudo -u postgres -A psql", true},
		{"sudo -E -A make install", true},
		{"sudo -nA true", true},
		{"sudo -uroot -A id", true},
		{"sudo --user=root --askpass id", true},
		{"sudo --user root --ask id", true},
		{"sudo -p 'pw: ' -A id", true},
		{"sudo FOO=1 -A env", true},
		{"sudo -n true", true},
		{"sudo --non-interactive true", true},
		{"sudo -k", true},
		{"sudo -K", true},
		{"sudo -V", true},
		{"sudo --version", true},
		{"sudo -h", true},
		{"sudo -k ls", false},
		{"sudo -ks", false},
		{"sudo -l", false},
		{"sudo -v", false},
		{"sudo -S cat /etc/shadow", false},
		{"sudo -u root apt update", false},
		{"sudo -p -A id", false},
		{"sudo -h host id", false},
		{"sudo -- -A", false},
		{"sudo --bogus -A id", false},
		{"sudo", false},
	}
	for _, tt := range tests {
		if err := ValidateSudoCommand(tt.cmd); (err == nil) != tt.valid {
			t.Errorf("ValidateSudoCommand(%q) valid=%v, want %v (err=%v)", tt.cmd, err == nil, tt.valid, err)
		}
	}
}

func TestValidateSudoCommand_SeesSudoBehindWrappers(t *testing.T) {
	tests := []struct {
		cmd   string
		valid bool
	}{
		{"env sudo apt update", false},
		{"env -i PATH=/usr/bin sudo apt update", false},
		{"env -u HOME sudo apt update", false},
		{"nice -n 10 sudo make install", false},
		{"nice -10 sudo make install", false},
		{"nohup sudo make install", false},
		{"timeout 60 sudo zypper refresh", false},
		{"timeout -s KILL 60 sudo zypper refresh", false},
		{"command sudo id", false},
		{"exec sudo id", false},
		{"time -p sudo id", false},
		{"stdbuf -oL sudo journalctl", false},
		{"setsid sudo id", false},
		{"ionice -c 3 sudo id", false},
		{"taskset 0x1 sudo id", false},
		{"find . -name '*.o' | xargs sudo rm", false},
		{"xargs -I {} sudo cp {} /opt", false},
		{"xargs -I{} sudo cp {} /opt", false},
		{"find /tmp -name x -exec sudo rm {} \\;", false},
		{"find /tmp -name x -execdir sudo rm {} +", false},
		{`bash -c "sudo zypper refresh"`, false},
		{`bash -lc 'sudo zypper refresh'`, false},
		{`sh -c "echo hi; sudo id"`, false},
		{`bash -c "bash -c 'sudo id'"`, false},
		{"timeout 60 nice sudo id", false},
		{"env sudo -A apt update", true},
		{"timeout 60 sudo -A zypper refresh", true},
		{"xargs -0 sudo -A rm", true},
		{`bash -lc "sudo -A make install"`, true},
		{"find . -exec sudo -A rm {} \\;", true},
		{"command -v sudo", true},
		{"timeout 5 grep sudo /var/log/messages", true},
		{"env FOO=sudo ls", true},
		{"echo sudo", true},
		{"xargs -I{} echo sudo {}", true},
		{"bash script.sh sudo", true},
		{"ionice -p 1234", true},
	}
	for _, tt := range tests {
		if err := ValidateSudoCommand(tt.cmd); (err == nil) != tt.valid {
			t.Errorf("ValidateSudoCommand(%q) valid=%v, want %v (err=%v)", tt.cmd, err == nil, tt.valid, err)
		}
	}
}

func TestAgentShellEnv_DropsHerdrPaneVariables(t *testing.T) {
	got := agentShellEnv(
		[]string{"PATH=/bin", "HERDR_ENV=1", "HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH=/s", "HOME=/u"},
		"SUDO_ASKPASS=/x",
	)
	want := []string{"PATH=/bin", "HOME=/u", "SUDO_ASKPASS=/x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("agentShellEnv = %v, want %v: a nested agent in a bash command must not claim ask's herdr pane", got, want)
	}
}

// A background sub-agent stays owed to the model until the model has its
// output: read after it finished, killed by the model, or handed over.
// Reading it while it runs does not count, and shell jobs are never owed.
func TestJobManager_UndeliveredAgents(t *testing.T) {
	env, _ := newTestToolEnv(t)
	jobs := env.Jobs
	jobs.Add("sleep 100", false, nil)
	first := jobs.AddAgent("agent: one", nil)
	second := jobs.AddAgent("agent reviewer: two", nil)
	var third *Job
	third = jobs.AddAgent("agent: three", func() { third.Finish(ShellResult{ExitCode: 1}) })
	owed := func() string {
		var ids []string
		for _, j := range jobs.UndeliveredAgents() {
			ids = append(ids, j.ID)
		}
		return strings.Join(ids, ",")
	}
	if got := owed(); got != "job-2,job-3,job-4" {
		t.Fatalf("owed = %q, want the three agents in start order", got)
	}

	read := JobOutputTool(env)
	runTool(t, read, JobOutputParams{JobID: first.ID})
	if got := owed(); got != "job-2,job-3,job-4" {
		t.Errorf("reading a running agent must not count as delivered: %q", got)
	}
	first.AppendOutput("report one")
	first.Finish(ShellResult{ExitCode: 0})
	runTool(t, read, JobOutputParams{JobID: first.ID})
	if got := owed(); got != "job-3,job-4" {
		t.Errorf("reading a finished agent delivers it: %q", got)
	}

	runTool(t, JobKillTool(env), JobKillParams{JobID: third.ID})
	if got := owed(); got != "job-3" {
		t.Errorf("an agent the model killed is not owed: %q", got)
	}
	second.MarkDelivered()
	if got := owed(); got != "" {
		t.Errorf("owed after the last delivery = %q", got)
	}
}

type fakeSteering struct{ ch chan struct{} }

func (f fakeSteering) SteerSignal() <-chan struct{} { return f.ch }

// One job_output call waits as long as it takes: it returns when the
// output matches until, when the job exits, or when the user steers the
// turn, and says why when the job is still running.
func TestJobOutput_WaitEndsOnMatchExitOrSteer(t *testing.T) {
	env, _ := newTestToolEnv(t)
	old := jobUntilPoll
	jobUntilPoll = 2 * time.Millisecond
	t.Cleanup(func() { jobUntilPoll = old })
	tool := JobOutputTool(env)

	server := env.Jobs.Add("serve", false, nil)
	go func() {
		time.Sleep(20 * time.Millisecond)
		server.AppendOutput("booting\nlisten")
		time.Sleep(20 * time.Millisecond)
		server.AppendOutput("ing on :8080\n")
	}()
	res, err := runTypedTool[JobOutputResult](t, tool, JobOutputParams{JobID: server.ID, Until: `listening on :\d+`})
	if err != nil {
		t.Fatalf("until wait failed: %v", err)
	}
	if !res.Matched || res.Status != "still running" || !strings.Contains(res.Output, "listening on :8080") {
		t.Errorf("until must match a line split across chunks and return with the job running: %+v", res)
	}
	res, _ = runTypedTool[JobOutputResult](t, tool, JobOutputParams{JobID: server.ID, Until: "booting"})
	if !res.Matched {
		t.Errorf("output already there matches at once: %+v", res)
	}

	build := env.Jobs.Add("make", false, nil)
	go func() {
		time.Sleep(20 * time.Millisecond)
		build.AppendOutput("done\n")
		build.Finish(ShellResult{ExitCode: 2})
	}()
	res, err = runTypedTool[JobOutputResult](t, tool, JobOutputParams{JobID: build.ID, Wait: true})
	if err != nil || res.Status != "exited with code 2" || res.Matched || res.Note != "" {
		t.Errorf("a wait ends when the job exits: %+v %v", res, err)
	}

	steer := fakeSteering{ch: make(chan struct{})}
	env.Steering = steer
	stuck := env.Jobs.Add("tail -f log", false, nil)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(steer.ch)
	}()
	res, err = runTypedTool[JobOutputResult](t, tool, JobOutputParams{JobID: stuck.ID, Wait: true, Timeout: 3600})
	if err != nil || !strings.Contains(res.Note, "user sent a message") || res.Matched {
		t.Errorf("a message from the user ends the wait: %+v %v", res, err)
	}

	if _, err := runTypedTool[JobOutputResult](t, tool, JobOutputParams{JobID: stuck.ID, Until: "("}); err == nil {
		t.Error("an invalid until pattern must fail")
	}
}

func TestWaitForJobs_TimesOutAndHonorsCancel(t *testing.T) {
	env, _ := newTestToolEnv(t)
	job := env.Jobs.Add("forever", false, nil)
	matched, note, err := waitForJobs(context.Background(), []*Job{job}, nil, false, 10*time.Millisecond, nil)
	if err != nil || len(matched) != 0 || !strings.Contains(note, "the job is still running") {
		t.Errorf("timeout = %v %q %v", matched, note, err)
	}
	done := env.Jobs.Add("quick", false, nil)
	done.Finish(ShellResult{})
	_, note, _ = waitForJobs(context.Background(), []*Job{job, done}, nil, false, 10*time.Millisecond, nil)
	if !strings.Contains(note, "1 of 2 jobs are still running") {
		t.Errorf("a multi-job timeout counts what still runs: %q", note)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := waitForJobs(ctx, []*Job{job}, nil, false, time.Hour, nil); err == nil {
		t.Error("a cancelled turn ends the wait with its error")
	}
	if got := jobWaitTimeout(0); got != JobWaitDefault {
		t.Errorf("default wait = %v", got)
	}
	if got := jobWaitTimeout(99999); got != JobWaitMax {
		t.Errorf("wait must cap at %v, got %v", JobWaitMax, got)
	}
	if got := jobWaitTimeout(90); got != 90*time.Second {
		t.Errorf("wait of 90s = %v", got)
	}
}

// A foreground bash call that reads files or waits with sleep carries a
// notice pointing at the better tool: the first three times in a session,
// then every tenth.
func TestBashTool_NoticesForFileReadsAndSleeps(t *testing.T) {
	env, _ := newTestToolEnv(t)
	oldRun := RunShell
	t.Cleanup(func() { RunShell = oldRun })
	RunShell = func(dir, command string, extraEnv ...string) (*ShellHandle, error) {
		out := make(chan string, 1)
		done := make(chan ShellResult, 1)
		out <- "ok\n"
		close(out)
		done <- ShellResult{ExitCode: 0}
		return &ShellHandle{Output: out, Done: done, Kill: func() {}}, nil
	}
	tool := BashTool(env)
	run := func(p BashParams) BashResult {
		t.Helper()
		res, err := runTypedTool[BashResult](t, tool, p)
		if err != nil {
			t.Fatalf("bash %q failed: %v", p.Command, err)
		}
		return res
	}

	res := run(BashParams{Command: "sleep 30; tail -2 /tmp/build.log"})
	if !strings.Contains(res.Notice, "through bash (tail)") || !strings.Contains(res.Notice, "job_output") {
		t.Errorf("a sleep then a file read earns both notices: %q", res.Notice)
	}
	if res := run(BashParams{Command: "go test ./..."}); res.Notice != "" {
		t.Errorf("an ordinary command carries no notice: %q", res.Notice)
	}
	if res := run(BashParams{Command: "sleep 2; tmux capture-pane -p"}); res.Notice != "" {
		t.Errorf("a short pause is not a wait: %q", res.Notice)
	}
	if res := run(BashParams{Command: "sleep 300", RunInBackground: true}); res.Notice != "" {
		t.Errorf("a background sleep is the recommended shape, not a nudge: %q", res.Notice)
	}

	var nudged []int
	for i := 2; i <= 20; i++ {
		if run(BashParams{Command: "cd /r && grep -rn Foo ."}).Notice != "" {
			nudged = append(nudged, i)
		}
	}
	if fmt.Sprint(nudged) != "[2 3 10 20]" {
		t.Errorf("file-read notices on calls %v, want [2 3 10 20]", nudged)
	}
}

// Several jobs wait together in one call: for all of them by default, for
// the first one done with first, and each comes back under its own heading.
func TestJobOutput_SeveralJobs(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := JobOutputTool(env)
	slow := env.Jobs.AddAgent("agent: slow", nil)
	fast := env.Jobs.AddAgent("agent: fast", nil)
	go func() {
		time.Sleep(20 * time.Millisecond)
		fast.AppendOutput("fast report")
		fast.Finish(ShellResult{ExitCode: 0})
		time.Sleep(20 * time.Millisecond)
		slow.AppendOutput("slow report")
		slow.Finish(ShellResult{ExitCode: 1})
	}()

	res, err := runTypedTool[JobOutputResult](t, tool, JobOutputParams{JobIDs: []string{slow.ID, fast.ID}, Wait: true, First: true})
	if err != nil {
		t.Fatalf("first-done wait failed: %v", err)
	}
	if res.Status != "1 of 2 jobs exited" || !strings.Contains(res.Output, "==> "+fast.ID+" (agent: fast): exited with code 0 <==\nfast report") ||
		!strings.Contains(res.Output, "==> "+slow.ID+" (agent: slow): still running <==") {
		t.Errorf("first: true returns as the first job ends, with both under headings: %+v", res)
	}
	if got := env.Jobs.UndeliveredAgents(); len(got) != 1 || got[0] != slow {
		t.Errorf("only the finished agent is delivered: %v", got)
	}

	res, err = runTypedTool[JobOutputResult](t, tool, JobOutputParams{JobID: fast.ID, JobIDs: []string{slow.ID, fast.ID}, Wait: true})
	if err != nil {
		t.Fatalf("all-done wait failed: %v", err)
	}
	if res.Status != "2 of 2 jobs exited" || strings.Count(res.Output, "==> ") != 2 || !strings.Contains(res.Output, "slow report") || res.Note != "" {
		t.Errorf("a wait for all returns once both ended, each job once: %+v", res)
	}
	if got := env.Jobs.UndeliveredAgents(); len(got) != 0 {
		t.Errorf("both agents are delivered: %v", got)
	}

	if _, err := runTypedTool[JobOutputResult](t, tool, JobOutputParams{}); err == nil {
		t.Error("naming no job must fail")
	}
	if _, err := runTypedTool[JobOutputResult](t, tool, JobOutputParams{JobIDs: []string{fast.ID, "job-99"}}); err == nil {
		t.Error("an unknown job id must fail")
	}
}

// When the user writes while a foreground command runs, the command is not
// killed: once the grace runs out it keeps running as a job, the call
// returns what it printed so far with the job id, and the job holds the
// rest. A command that finishes within the grace returns as usual.
func TestBashTool_YieldsToUserInput(t *testing.T) {
	env, events := newTestToolEnvSnapshot(t)
	oldRun, oldGrace := RunShell, SteerYieldGrace
	SteerYieldGrace = 20 * time.Millisecond
	t.Cleanup(func() { RunShell, SteerYieldGrace = oldRun, oldGrace })
	steer := fakeSteering{ch: make(chan struct{})}
	close(steer.ch)
	env.Steering = steer

	release := make(chan struct{})
	RunShell = func(dir, command string, extraEnv ...string) (*ShellHandle, error) {
		out := make(chan string, 4)
		done := make(chan ShellResult, 1)
		if command == "quick" {
			out <- "quick done\n"
			close(out)
			done <- ShellResult{}
			return &ShellHandle{Output: out, Done: done, Kill: func() {}}, nil
		}
		go func() {
			out <- "step 1\n"
			<-release
			out <- "step 2\n"
			close(out)
			done <- ShellResult{ExitCode: 3}
		}()
		return &ShellHandle{Output: out, Done: done, Kill: func() {}}, nil
	}
	tool := BashTool(env)

	res, err := runTypedTool[BashResult](t, tool, BashParams{Command: "quick"})
	if err != nil || res.JobID != "" || !strings.Contains(res.Output, "quick done") {
		t.Errorf("a command done within the grace returns as usual: %+v %v", res, err)
	}

	res, err = runTypedTool[BashResult](t, tool, BashParams{Command: "long build", Description: "build it"})
	if err != nil {
		t.Fatalf("yield failed: %v", err)
	}
	if res.JobID == "" || !strings.Contains(res.Output, "step 1") || !strings.Contains(res.Note, "still running as "+res.JobID) {
		t.Fatalf("the call returns the output so far and the job: %+v", res)
	}
	job := env.Jobs.Get(res.JobID)
	close(release)
	select {
	case <-job.DoneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the job never finished")
	}
	out, _, _, jobRes := job.Snapshot()
	if out != "step 1\nstep 2\n" || jobRes.ExitCode != 3 {
		t.Errorf("the job holds all the output and the exit code: %q %+v", out, jobRes)
	}
	var started, ended bool
	for deadline := time.Now().Add(2 * time.Second); !ended && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		for _, ev := range events() {
			switch e := ev.(type) {
			case engine.BgTaskStartedEvent:
				started = started || (e.JobID == job.ID && e.Description == "build it")
			case engine.BgTaskEndedEvent:
				ended = ended || e.JobID == job.ID
			}
		}
	}
	if !started || !ended {
		t.Errorf("the handoff shows as a background job: started=%v ended=%v", started, ended)
	}
}

// A command handed to a job keeps its own timeout.
func TestFinishDetachedShell_KillsAtTheTimeout(t *testing.T) {
	env, _ := newTestToolEnv(t)
	out := make(chan string, 1)
	done := make(chan ShellResult, 1)
	var once sync.Once
	handle := &ShellHandle{Output: out, Done: done, Kill: func() {
		once.Do(func() {
			close(out)
			done <- ShellResult{ExitCode: -1}
		})
	}}
	job := env.Jobs.Add("hang", false, handle.Kill)
	unregistered := false
	finishDetachedShell(env, job, handle, func() { unregistered = true }, 10*time.Millisecond, 90*time.Second)
	output, _, isDone, _ := job.Snapshot()
	if !isDone || !strings.Contains(output, "killed: the command ran past its 1m30s timeout") || !unregistered {
		t.Errorf("done=%v unregistered=%v output=%q", isDone, unregistered, output)
	}
}
