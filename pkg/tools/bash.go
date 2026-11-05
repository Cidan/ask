package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/herdr"
	"github.com/Cidan/ask/pkg/tools/filters"
)

const (
	BashDefaultTimeout = 120 * time.Second
	BashMaxTimeout     = 600 * time.Second
	BashRawCap         = 4 * MaxToolOutput

	JobWaitDefault = 600 * time.Second
	JobWaitMax     = 3600 * time.Second

	// bashSleepNudgeSeconds is the shortest sleep that earns the wait
	// nudge; shorter ones are pauses (a UI settling), not waits.
	bashSleepNudgeSeconds = 10
)

// jobUntilPoll is how often job_output re-checks a waiting job's output
// against until.
var jobUntilPoll = 250 * time.Millisecond

const bashFileReadNotice = "You read or searched files through bash (%s). Use the read tool instead (files, each with any number of line ranges, plus grep searches, all in one call) and the grep tool (several patterns, context lines): one call does what took several bash commands, and a read brings in the project rules for the files it touches."

const bashSleepNotice = "This command waited with sleep, and every wait like it costs a full model call. Start the slow thing, or a loop that checks for it, with run_in_background, then call job_output with wait: true and until: a regexp for the line you are waiting for. One call then waits up to an hour and returns the moment the job exits, its output matches, or the user sends a message."

// ShellResult is the terminal state of a shell invocation.
type ShellResult struct {
	ExitCode int
	Err      error
}

// ShellHandle represents a running shell command.
type ShellHandle struct {
	// Pid is the shell's process id; zero when no real process backs the
	// handle.
	Pid    int
	Output <-chan string
	Done   <-chan ShellResult
	Kill   func()
}

// RunShell is the swappable execution hook for shell commands.
var RunShell = RunShellProcess

// agentShell is the shell the bash tool forks. It is resolved once at
// process startup (package initialization, before main runs) rather than
// on the first command, so detection happens at boot. bash and zsh are
// preferred over the user's $SHELL because LLM agents emit POSIX/bash
// syntax that non-POSIX interactive shells like fish reject; the agent
// tool therefore never inherits the user's interactive shell.
var agentShell = ResolveAgentShell(exec.LookPath, os.Getenv)

// ResolveAgentShell picks the shell for the agent bash tool, preferring
// bash, then zsh, then the user's $SHELL, then /bin/sh. lookPath and
// getenv are injected so the preference order is testable without
// depending on the host's PATH or environment.
func ResolveAgentShell(lookPath func(string) (string, error), getenv func(string) string) string {
	for _, name := range []string{"bash", "zsh"} {
		if path, err := lookPath(name); err == nil && path != "" {
			return path
		}
	}
	if sh := getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// agentShellEnv is the environment of a bash tool command: base without
// herdr's pane variables, plus extra.
func agentShellEnv(base []string, extra ...string) []string {
	return append(herdr.WithoutPaneEnv(base), extra...)
}

// shellOutputGrace is how long a command's output is still read after its
// shell exits or is killed. Without a bound, a process the command started
// that keeps the output pipes open — still in the shell's process group, or
// one that left it and so survives the group's SIGKILL — holds the call open
// for as long as that process lives.
var shellOutputGrace = 2 * time.Second

const shellOutputHeldNote = "(the shell exited, but a process it started still held its output open %s later: ask stopped reading and killed the command's process group; start long-running processes with run_in_background)\n"

// RunShellProcess forks the resolved agent shell (see agentShell) with
// -c <command> as the leader of a new session, so the command has no
// controlling terminal and its process group is the shell's pid. Output and
// Done finish at most shellOutputGrace after the shell exits.
func RunShellProcess(dir, command string, extraEnv ...string) (*ShellHandle, error) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, err
	}
	cmd := exec.Command(agentShell, "-c", command)
	cmd.Dir = dir
	cmd.Env = agentShellEnv(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	err = cmd.Start()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		stdoutR.Close()
		stderrR.Close()
		return nil, err
	}
	pid := cmd.Process.Pid
	kill := func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }
	out := make(chan string, 64)
	done := make(chan ShellResult, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go streamShellOutput(stdoutR, out, &wg)
	go streamShellOutput(stderrR, out, &wg)
	read := make(chan struct{})
	go func() {
		wg.Wait()
		close(read)
	}()
	go func() {
		err := cmd.Wait()
		select {
		case <-read:
		case <-time.After(shellOutputGrace):
			kill()
			stdoutR.Close()
			stderrR.Close()
			<-read
			out <- fmt.Sprintf(shellOutputHeldNote, shellOutputGrace)
		}
		stdoutR.Close()
		stderrR.Close()
		code := 0
		if err != nil {
			code = -1
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
				err = nil
			}
		}
		done <- ShellResult{ExitCode: code, Err: err}
		close(out)
	}()
	return &ShellHandle{
		Pid:    pid,
		Output: out,
		Done:   done,
		Kill:   kill,
	}, nil
}

// streamShellOutput sends r to out line by line until r ends or is closed.
// A line longer than the read buffer goes out in pieces, so no line length
// stops the read and leaves the command blocked on a full pipe.
func streamShellOutput(r *os.File, out chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			s := string(chunk)
			switch {
			case strings.HasSuffix(s, "\r\n"):
				s = s[:len(s)-2] + "\n"
			case err != nil && err != bufio.ErrBufferFull:
				s += "\n"
			}
			out <- s
		}
		if err != nil && err != bufio.ErrBufferFull {
			return
		}
	}
}

// Job represents one background shell command, or one background sub-agent
// (Agent) whose output is its final report.
type Job struct {
	ID      string
	Command string
	Agent   bool

	Mu        sync.Mutex
	Buf       strings.Builder
	Truncated bool
	Done      bool
	Result    ShellResult
	// Delivered records that the model has the job's final output: it read
	// it with job_output, killed the job itself, or was handed it.
	Delivered bool

	DisableSavings  bool
	SavingsRecorded bool

	Kill   func()
	DoneCh chan struct{}

	seq int
}

func (j *Job) AppendOutput(chunk string) {
	j.Mu.Lock()
	defer j.Mu.Unlock()
	if j.Buf.Len() >= BashRawCap {
		j.Truncated = true
		return
	}
	j.Buf.WriteString(chunk)
}

func (j *Job) Snapshot() (output string, truncated, done bool, result ShellResult) {
	j.Mu.Lock()
	defer j.Mu.Unlock()
	return j.Buf.String(), j.Truncated, j.Done, j.Result
}

func (j *Job) Finish(r ShellResult) {
	j.Mu.Lock()
	j.Done = true
	j.Result = r
	j.Mu.Unlock()
	close(j.DoneCh)
}

// JobManager tracks background execution jobs.
type JobManager struct {
	mu   sync.Mutex
	seq  int
	jobs map[string]*Job
}

func NewJobManager() *JobManager {
	return &JobManager{jobs: make(map[string]*Job)}
}

func (m *JobManager) Add(command string, disableSavings bool, kill func()) *Job {
	return m.add(command, disableSavings, false, kill)
}

// AddAgent tracks a background sub-agent; label names it in job listings.
func (m *JobManager) AddAgent(label string, kill func()) *Job {
	return m.add(label, true, true, kill)
}

func (m *JobManager) add(command string, disableSavings, agent bool, kill func()) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	job := &Job{
		ID:             fmt.Sprintf("job-%d", m.seq),
		Command:        command,
		Agent:          agent,
		DisableSavings: disableSavings,
		Kill:           kill,
		DoneCh:         make(chan struct{}),
		seq:            m.seq,
	}
	m.jobs[job.ID] = job
	return job
}

// UndeliveredAgents returns the background sub-agents whose report the
// model does not have yet — still running, or finished but never read —
// oldest first.
func (m *JobManager) UndeliveredAgents() []*Job {
	m.mu.Lock()
	var out []*Job
	for _, j := range m.jobs {
		if j.Agent {
			out = append(out, j)
		}
	}
	m.mu.Unlock()
	kept := out[:0]
	for _, j := range out {
		j.Mu.Lock()
		delivered := j.Delivered
		j.Mu.Unlock()
		if !delivered {
			kept = append(kept, j)
		}
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].seq < kept[b].seq })
	return kept
}

// MarkDelivered records that the model has the job's output.
func (j *Job) MarkDelivered() {
	j.Mu.Lock()
	j.Delivered = true
	j.Mu.Unlock()
}

func (m *JobManager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

func (m *JobManager) KillAll() {
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	for _, j := range jobs {
		j.Mu.Lock()
		running := !j.Done
		j.Mu.Unlock()
		if running && j.Kill != nil {
			j.Kill()
		}
	}
}

const BashToolDescription = `Run a shell command in the working directory and return its combined stdout/stderr (interleaved, truncated middle-out past 30000 chars). Output is automatically filtered to save tokens: progress/bookkeeping noise is stripped, and commands whose output is noise when they succeed (make, go build, cargo build, docker build, and similar) collapse to a one-line summary like "make: ok" on success while still showing the full error on failure. When filtering was lossy the untouched output is written to a file named in the result's raw_path field (read it to recover anything dropped). Set disable_token_savings to true if you strictly need the raw uncompressed output for a single command. Commands run in independent shells — no state persists between calls, so prefer absolute paths over cd. Set run_in_background for servers, long builds, and anything you would otherwise wait on (CI, a deploy, a remote machine: background a loop that checks it), then wait for it with one job_output call (wait: true, until: a regexp) and stop it with job_kill — never wait with sleep. Never use bash to read or search files (cat, head, tail, sed -n, grep, rg, find): the read, grep, and glob tools do that, many files and patterns in one call. Quote paths containing spaces.`

type BashParams struct {
	Command             string `json:"command" jsonschema:"the shell command to execute"`
	Description         string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this command does"`
	Timeout             int    `json:"timeout,omitempty" jsonschema:"max seconds to wait before the command is killed (default 120, max 600)"`
	RunInBackground     bool   `json:"run_in_background,omitempty" jsonschema:"start the command as a background job and return its job id immediately"`
	DisableTokenSavings bool   `json:"disable_token_savings,omitempty" jsonschema:"set to true to disable standard output filtering for this command if raw uncompressed output is strictly needed"`
}

// BashResult is the bash tool's response.
type BashResult struct {
	Output    string `json:"output,omitempty" jsonschema:"combined stdout and stderr"`
	ExitCode  int    `json:"exit_code" jsonschema:"process exit code"`
	JobID     string `json:"job_id,omitempty" jsonschema:"set when the command was started in the background"`
	TimedOut  bool   `json:"timed_out,omitempty" jsonschema:"true when the command was killed for exceeding its timeout"`
	Cancelled bool   `json:"cancelled,omitempty" jsonschema:"true when the command was cancelled"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"true when output exceeded the in-memory cap"`
	RawPath   string `json:"raw_path,omitempty" jsonschema:"path to the untouched raw output when filtering was lossy; read it with the read tool (offset/limit) to recover anything the summary dropped"`
	RawLines  int    `json:"raw_lines,omitempty" jsonschema:"line count of the raw_path file"`
	Notice    string `json:"notice,omitempty" jsonschema:"a better way to do what this command did; follow it from now on"`
}

// JobOutputResult is the job_output tool's response.
type JobOutputResult struct {
	JobID    string `json:"job_id,omitempty"`
	Command  string `json:"command,omitempty"`
	Status   string `json:"status,omitempty" jsonschema:"running or exited"`
	Output   string `json:"output,omitempty"`
	RawPath  string `json:"raw_path,omitempty" jsonschema:"path to the untouched raw output when filtering was lossy; read it with the read tool to recover anything the summary dropped"`
	RawLines int    `json:"raw_lines,omitempty" jsonschema:"line count of the raw_path file"`
	Matched  bool   `json:"matched,omitempty" jsonschema:"true when the wait ended because the output matched until"`
	Note     string `json:"note,omitempty" jsonschema:"why the wait ended while the job still runs"`
}

// JobKillResult is the job_kill tool's response.
type JobKillResult struct {
	JobID    string `json:"job_id,omitempty"`
	Killed   bool   `json:"killed,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// BashTool returns the native bash tool.
func BashTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"bash",
		BashToolDescription,
		func(ctx agent.Context, p BashParams) (BashResult, error) {
			command := strings.TrimSpace(p.Command)
			if command == "" {
				return BashResult{}, errors.New("command is required")
			}
			if err := ValidateSudoCommand(command); err != nil {
				return BashResult{}, errors.New(err.Error())
			}
			if !SafeShellCommand(command) {
				if denied := env.ApprovalDenied(ctx, "bash", map[string]any{
					"command":     command,
					"description": p.Description,
				}); denied != "" {
					return BashResult{}, errors.New(denied)
				}
			}

			var extraEnv []string
			sudoServer := EnsureSudoIPCServer()
			if sudoServer != nil {
				extraEnv = append(extraEnv,
					"SUDO_ASKPASS="+askPassExecutable(),
					AskSudoSocketEnv+"="+sudoServer.SocketPath,
				)
			}

			handle, err := RunShell(env.Cwd, command, extraEnv...)
			if err != nil {
				return BashResult{}, errors.New("could not start shell: " + err.Error())
			}
			unregisterSudo := func() {}
			if sudoServer != nil && handle.Pid > 0 {
				unregisterSudo = sudoServer.RegisterShell(handle.Pid, env.Interaction, env.TabID)
			}

			if p.RunInBackground {
				job := env.Jobs.Add(command, p.DisableTokenSavings, handle.Kill)
				go func() {
					for chunk := range handle.Output {
						job.AppendOutput(chunk)
					}
					job.Finish(<-handle.Done)
					unregisterSudo()
					if env.Emit != nil {
						env.Emit(engine.BgTaskEndedEvent{
							BaseEvent: engine.BaseEvent{TabID: env.TabID},
							JobID:     job.ID,
						})
					}
				}()
				if env.Emit != nil {
					env.Emit(engine.BgTaskStartedEvent{
						BaseEvent:   engine.BaseEvent{TabID: env.TabID},
						JobID:       job.ID,
						Description: p.Description,
					})
				}
				return BashResult{JobID: job.ID, Output: "started background job " + job.ID + "; wait for it with job_output (wait: true, until: a regexp) and stop it with job_kill"}, nil
			}
			defer unregisterSudo()
			notice := env.bashNotice(command)

			timeout := BashDefaultTimeout
			if p.Timeout > 0 {
				timeout = min(time.Duration(p.Timeout)*time.Second, BashMaxTimeout)
			}
			timer := time.NewTimer(timeout)
			defer timer.Stop()

			var buf strings.Builder
			handleFinalOutput := func(rawStr string, exit int) string {
				if p.DisableTokenSavings {
					return rawStr
				}
				filteredStr, tokensSaved := ApplyBashFilter(command, rawStr, exit)
				if !IsTrivialCommand(command) {
					_ = RecordSavings(ExtractBaseCommand(command), len(rawStr)/4, tokensSaved)
				}
				return filteredStr
			}
			rawTruncated := false
			collect := func(chunk string) {
				if buf.Len() >= BashRawCap {
					rawTruncated = true
					return
				}
				buf.WriteString(chunk)
			}
			for {
				select {
				case chunk, ok := <-handle.Output:
					if !ok {
						res := <-handle.Done
						rawStr := buf.String()
						result := bashResponse(handleFinalOutput(rawStr, res.ExitCode), rawTruncated, res)
						MaybeSpill(&result, command, rawStr)
						result.Notice = notice
						return result, nil
					}
					collect(chunk)
				case <-timer.C:
					handle.Kill()
					drainShellOutput(handle.Output, collect)
					return BashResult{TimedOut: true, Output: TruncateMiddle(handleFinalOutput(buf.String(), -1)), Notice: notice},
						fmt.Errorf("command timed out after %s and was killed", timeout)
				case <-ctx.Done():
					handle.Kill()
					drainShellOutput(handle.Output, collect)
					return BashResult{Cancelled: true, Output: TruncateMiddle(handleFinalOutput(buf.String(), -1))}, nil
				}
			}
		},
	)
}

// bashNotice is the nudge a foreground command earns, if any: reading or
// searching files through the shell, or waiting with sleep.
func (e *ToolEnv) bashNotice(command string) string {
	var notes []string
	if reads := filters.FileReadCommands(command); len(reads) > 0 && e.shouldNudge("file-read") {
		notes = append(notes, fmt.Sprintf(bashFileReadNotice, strings.Join(reads, ", ")))
	}
	if filters.LongestSleep(command) >= bashSleepNudgeSeconds && e.shouldNudge("sleep") {
		notes = append(notes, bashSleepNotice)
	}
	return strings.Join(notes, " ")
}

func drainShellOutput(ch <-chan string, collect func(string)) {
	for chunk := range ch {
		collect(chunk)
	}
}

func bashResponse(output string, rawTruncated bool, res ShellResult) BashResult {
	body := TruncateMiddle(output)
	if rawTruncated {
		body += "\n(output exceeded the in-memory cap; middle portions were dropped)"
	}
	if res.Err != nil {
		return BashResult{Output: body, ExitCode: res.ExitCode, Truncated: rawTruncated}
	}
	return BashResult{Output: body, ExitCode: res.ExitCode, Truncated: rawTruncated}
}

const JobOutputToolDescription = `Read the output of background jobs: bash commands started with run_in_background, or background sub-agents. Name one job with job_id or several with job_ids. Set wait to block until the jobs exit (all of them, or the first one with first: true), their output matches the until regexp, or timeout seconds pass (default 600, max 3600), whichever comes first; a message from the user also ends the wait. One waiting call replaces any number of polls, so never wait on a job, a build, CI, a deploy, or a remote machine with bash sleep or by calling job_output again and again, and wait on several jobs in one call rather than one after another. To wait on something that is not a job, start a bash loop that checks for it with run_in_background and wait on that job here.`

type JobOutputParams struct {
	JobID       string   `json:"job_id,omitempty" jsonschema:"the job id returned when the background command started"`
	JobIDs      []string `json:"job_ids,omitempty" jsonschema:"several job ids to read, and to wait on together, in one call"`
	Wait        bool     `json:"wait,omitempty" jsonschema:"block until the jobs exit, their output matches until, or timeout passes, then return their output"`
	First       bool     `json:"first,omitempty" jsonschema:"with several jobs, stop waiting as soon as one of them is done (default: wait for all)"`
	Until       string   `json:"until,omitempty" jsonschema:"regular expression; a job is done waiting once its output matches it (a server's ready line, a test's FAIL); implies wait"`
	Timeout     int      `json:"timeout,omitempty" jsonschema:"seconds to wait at most (default 600, max 3600); implies wait"`
	Description string   `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// jobWaitTimeout is how long a job_output wait of the given seconds may
// block: JobWaitDefault when unset, never past JobWaitMax.
func jobWaitTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return JobWaitDefault
	}
	return min(time.Duration(seconds)*time.Second, JobWaitMax)
}

// waitForJobs blocks until every job is ready (or, with first, any one is),
// timeout passes, the user steers the turn, or ctx ends — which last is the
// only error. A job is ready once it exits or, with until set, once its raw
// output matches. matched names the jobs whose output matched; note says why
// a wait ended while a job it waited for still runs.
func waitForJobs(ctx context.Context, jobs []*Job, until *regexp.Regexp, first bool, timeout time.Duration, steered <-chan struct{}) (matched map[string]bool, note string, err error) {
	matched = map[string]bool{}
	ready := map[*Job]bool{}
	scanned := map[*Job]int{}
	check := func(j *Job) {
		if ready[j] {
			return
		}
		out, _, done, _ := j.Snapshot()
		if until != nil && len(out) > scanned[j] {
			from := strings.LastIndexByte(out[:scanned[j]], '\n') + 1
			scanned[j] = len(out)
			if until.MatchString(out[from:]) {
				matched[j.ID] = true
				ready[j] = true
			}
		}
		if done {
			ready[j] = true
		}
	}
	finished := func() bool {
		if first {
			return len(ready) > 0
		}
		return len(ready) == len(jobs)
	}
	for _, j := range jobs {
		check(j)
	}
	if finished() {
		return matched, "", nil
	}

	exited := make(chan *Job, len(jobs))
	stop := make(chan struct{})
	defer close(stop)
	for _, j := range jobs {
		go func() {
			select {
			case <-j.DoneCh:
				exited <- j
			case <-stop:
			}
		}()
	}
	var tick <-chan time.Time
	if until != nil {
		t := time.NewTicker(jobUntilPoll)
		defer t.Stop()
		tick = t.C
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case j := <-exited:
			check(j)
		case <-tick:
			for _, j := range jobs {
				check(j)
			}
		case <-deadline.C:
			if len(jobs) == 1 {
				return matched, fmt.Sprintf("waited %s and the job is still running; wait again only if it is still worth it", timeout), nil
			}
			return matched, fmt.Sprintf("waited %s and %d of %d jobs are still running; wait again only if it is still worth it", timeout, len(jobs)-len(ready), len(jobs)), nil
		case <-steered:
			return matched, "the user sent a message, which arrives with this result; answer it, then call job_output again if you still need to wait", nil
		case <-ctx.Done():
			return matched, "", ctx.Err()
		}
		if finished() {
			return matched, "", nil
		}
	}
}

// jobReport is what job_output says about one job.
type jobReport struct {
	status   string
	body     string
	done     bool
	rawPath  string
	rawLines int
}

// reportJob reads job's output through the bash filter, marks a finished
// job delivered, and spills a finished job's raw output when filtering lost
// some of it.
func reportJob(job *Job) jobReport {
	output, truncated, done, res := job.Snapshot()
	rawOutput := output
	if !job.DisableSavings {
		// A still-running job has partial output; treat it as a
		// non-clean exit so semantic filters preserve it verbatim
		// rather than collapsing an incomplete run to a summary.
		exit := -1
		if done {
			exit = res.ExitCode
		}
		filtered, saved := ApplyBashFilter(job.Command, output, exit)
		if done && !IsTrivialCommand(job.Command) {
			var shouldRecord bool
			job.Mu.Lock()
			if !job.SavingsRecorded {
				job.SavingsRecorded = true
				shouldRecord = true
			}
			job.Mu.Unlock()
			if shouldRecord {
				_ = RecordSavings(ExtractBaseCommand(job.Command), len(output)/4, saved)
			}
		}
		output = filtered
	}
	body := TruncateMiddle(output)
	if truncated {
		body += "\n(output exceeded the in-memory cap; middle portions were dropped)"
	}
	r := jobReport{status: "still running", done: done}
	if done {
		r.status = fmt.Sprintf("exited with code %d", res.ExitCode)
		if res.Err != nil {
			r.status = "failed: " + res.Err.Error()
		}
		job.MarkDelivered()
	}
	if strings.TrimSpace(body) == "" {
		body = "(no output yet)"
	}
	r.body = body
	if done {
		r.rawPath, r.rawLines = spillIfLossy(job.Command, rawOutput, body)
	}
	return r
}

// JobOutputTool returns the native job_output tool.
func JobOutputTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"job_output",
		JobOutputToolDescription,
		func(ctx agent.Context, p JobOutputParams) (JobOutputResult, error) {
			ids := p.JobIDs
			if p.JobID != "" && !slices.Contains(ids, p.JobID) {
				ids = append([]string{p.JobID}, ids...)
			}
			if len(ids) == 0 {
				return JobOutputResult{}, errors.New("job_id or job_ids is required")
			}
			var jobs []*Job
			for _, id := range ids {
				job := env.Jobs.Get(id)
				if job == nil {
					return JobOutputResult{}, errors.New("no such job: " + id)
				}
				if !slices.Contains(jobs, job) {
					jobs = append(jobs, job)
				}
			}
			var matched map[string]bool
			var note string
			if p.Wait || p.Until != "" || p.Timeout > 0 {
				var until *regexp.Regexp
				if p.Until != "" {
					re, err := regexp.Compile(p.Until)
					if err != nil {
						return JobOutputResult{}, fmt.Errorf("invalid until pattern: %v", err)
					}
					until = re
				}
				var err error
				matched, note, err = waitForJobs(ctx, jobs, until, p.First, jobWaitTimeout(p.Timeout), env.steerSignal())
				if err != nil {
					return JobOutputResult{}, errors.New("cancelled while waiting for " + strings.Join(ids, ", "))
				}
			}

			if len(jobs) == 1 {
				job := jobs[0]
				r := reportJob(job)
				result := JobOutputResult{JobID: job.ID, Command: job.Command, Status: r.status, Output: r.body, Matched: matched[job.ID], RawPath: r.rawPath, RawLines: r.rawLines}
				if !r.done {
					result.Note = note
				}
				return result, nil
			}

			var out strings.Builder
			running := 0
			for i, job := range jobs {
				r := reportJob(job)
				if !r.done {
					running++
				}
				if i > 0 {
					out.WriteByte('\n')
				}
				heading := job.ID + " (" + job.Command + "): " + r.status
				if matched[job.ID] {
					heading += ", output matched until"
				}
				fmt.Fprintf(&out, "==> %s <==\n%s\n", heading, strings.TrimRight(r.body, "\n"))
				if r.rawPath != "" {
					fmt.Fprintf(&out, "(raw output: %s, %d lines)\n", r.rawPath, r.rawLines)
				}
			}
			result := JobOutputResult{
				Status: fmt.Sprintf("%d of %d jobs exited", len(jobs)-running, len(jobs)),
				Output: out.String(),
			}
			if running > 0 {
				result.Note = note
			}
			return result, nil
		},
	)
}

const JobKillToolDescription = `Kill a background job started with bash run_in_background. The job's whole process group receives SIGKILL.`

type JobKillParams struct {
	JobID       string `json:"job_id" jsonschema:"the job id to kill"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// JobKillTool returns the native job_kill tool.
func JobKillTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"job_kill",
		JobKillToolDescription,
		func(ctx agent.Context, p JobKillParams) (JobKillResult, error) {
			job := env.Jobs.Get(p.JobID)
			if job == nil {
				return JobKillResult{}, errors.New("no such job: " + p.JobID)
			}
			// The model chose to end it: whatever it would have reported is
			// not wanted.
			job.MarkDelivered()
			_, _, done, res := job.Snapshot()
			if done {
				return JobKillResult{JobID: p.JobID, ExitCode: res.ExitCode}, nil
			}
			if job.Kill != nil {
				job.Kill()
			}
			select {
			case <-job.DoneCh:
			case <-time.After(5 * time.Second):
				return JobKillResult{}, errors.New(p.JobID + " did not exit within 5s of SIGKILL")
			case <-ctx.Done():
				return JobKillResult{}, errors.New("cancelled while waiting for " + p.JobID + " to die")
			}
			return JobKillResult{JobID: p.JobID, Killed: true}, nil
		},
	)
}

var safeShellCommands = []string{
	"cat", "date", "df", "du", "echo", "env", "file", "find", "free",
	"git blame", "git branch", "git diff", "git log", "git ls-files",
	"git remote", "git show", "git status", "git stash list", "git tag",
	"go env", "go list", "go version", "go vet",
	"grep", "head", "hostname", "id", "ls", "ps", "pwd", "rg", "sort",
	"stat", "tail", "tree", "uname", "uniq", "uptime", "wc", "which",
	"whoami",
}

// SafeShellCommand reports whether command may run without an approval prompt.
func SafeShellCommand(command string) bool {
	if strings.ContainsAny(command, ";|&`$<>(){}") {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(command))
	for _, safe := range safeShellCommands {
		if lower == safe {
			return true
		}
		if strings.HasPrefix(lower, safe+" ") {
			return true
		}
	}
	return false
}

var errSudoRequiresAskpass = fmt.Errorf("sudo: commands invoking sudo must pass -A (e.g., 'sudo -A <command>'), including sudo run through env, timeout, xargs, find -exec, or sh -c. Plain sudo without -A is blocked because ask requires -A to trigger the secure password prompt modal.")

// ValidateSudoCommand rejects a command line that runs sudo in a way that
// could prompt for a password without -A (--askpass).
func ValidateSudoCommand(command string) error {
	return parseAndValidateSudo(command)
}

func parseAndValidateSudo(input string) error {
	var (
		words    []string
		skipNext bool
		i        int
		n        = len(input)
	)

	checkReset := func() error {
		err := validateSimpleCommand(words)
		words = words[:0]
		skipNext = false
		return err
	}

	for i < n {
		for i < n && (input[i] == ' ' || input[i] == '\t' || input[i] == '\r') {
			i++
		}
		if i >= n {
			break
		}

		ch := input[i]

		if ch == '\n' || ch == ';' {
			if err := checkReset(); err != nil {
				return err
			}
			i++
			continue
		}

		if ch == '|' || ch == '&' {
			if err := checkReset(); err != nil {
				return err
			}
			i++
			if i < n && (input[i] == '|' || input[i] == '&') {
				i++
			}
			continue
		}

		if ch == '(' || ch == ')' {
			if err := checkReset(); err != nil {
				return err
			}
			i++
			continue
		}

		wordStart := i
		var unquoted strings.Builder

		for i < n {
			c := input[i]

			if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '|' || c == '&' || c == '(' || c == ')' {
				break
			}

			if c == '\\' {
				i++
				if i < n {
					unquoted.WriteByte(input[i])
					i++
				}
				continue
			}

			if c == '\'' {
				i++
				for i < n && input[i] != '\'' {
					unquoted.WriteByte(input[i])
					i++
				}
				if i < n {
					i++
				}
				continue
			}

			if c == '"' {
				i++
				for i < n && input[i] != '"' {
					dc := input[i]
					if dc == '\\' {
						i++
						if i < n {
							unquoted.WriteByte(input[i])
							i++
						}
						continue
					}
					if dc == '$' && i+1 < n && input[i+1] == '(' {
						inner, endIdx, err := extractMatchingParen(input, i+2)
						if err == nil {
							if err := parseAndValidateSudo(inner); err != nil {
								return err
							}
							i = endIdx
							continue
						}
					}
					if dc == '`' {
						inner, endIdx := extractBacktick(input, i+1)
						if err := parseAndValidateSudo(inner); err != nil {
							return err
						}
						i = endIdx
						continue
					}
					unquoted.WriteByte(dc)
					i++
				}
				if i < n {
					i++
				}
				continue
			}

			if c == '$' && i+1 < n && input[i+1] == '(' {
				inner, endIdx, err := extractMatchingParen(input, i+2)
				if err == nil {
					if err := parseAndValidateSudo(inner); err != nil {
						return err
					}
					i = endIdx
					continue
				}
			}

			if c == '`' {
				inner, endIdx := extractBacktick(input, i+1)
				if err := parseAndValidateSudo(inner); err != nil {
					return err
				}
				i = endIdx
				continue
			}

			unquoted.WriteByte(c)
			i++
		}

		rawWord := input[wordStart:i]
		unquotedStr := unquoted.String()

		if rawWord == "" && unquotedStr == "" {
			i++
			continue
		}

		if skipNext {
			skipNext = false
			continue
		}

		if isRedirectionOp(unquotedStr) || isRedirectionOp(rawWord) {
			skipNext = true
			continue
		}

		if isSelfContainedRedirect(unquotedStr) || isSelfContainedRedirect(rawWord) {
			continue
		}

		words = append(words, unquotedStr)
	}

	if err := checkReset(); err != nil {
		return err
	}

	return nil
}

func isEnvAssignment(s string) bool {
	idx := strings.IndexByte(s, '=')
	if idx <= 0 {
		return false
	}
	return isValidShellIdentifier(s[:idx])
}

func isValidShellIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return false
			}
		} else {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				return false
			}
		}
	}
	return true
}

func isRedirectionOp(s string) bool {
	switch s {
	case ">", "<", ">>", "<<", "<<<", ">&", "<&", "1>", "2>", "1>>", "2>>", "&>", "&>>":
		return true
	}
	return false
}

func isSelfContainedRedirect(s string) bool {
	if isRedirectionOp(s) {
		return false
	}
	if strings.HasPrefix(s, ">") || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "1>") || strings.HasPrefix(s, "2>") || strings.HasPrefix(s, "&>") {
		return true
	}
	return false
}

func extractMatchingParen(input string, start int) (string, int, error) {
	depth := 1
	i := start
	n := len(input)
	inSingle := false
	inDouble := false

	for i < n && depth > 0 {
		c := input[i]
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			i++
			continue
		}
		if inDouble {
			if c == '\\' {
				i += 2
				continue
			}
			if c == '"' {
				inDouble = false
			}
			i++
			continue
		}
		if c == '\\' {
			i += 2
			continue
		}
		if c == '\'' {
			inSingle = true
			i++
			continue
		}
		if c == '"' {
			inDouble = true
			i++
			continue
		}
		if c == '(' {
			depth++
			i++
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return input[start:i], i + 1, nil
			}
			i++
			continue
		}
		i++
	}
	if depth != 0 {
		return "", start, fmt.Errorf("unmatched paren")
	}
	return input[start:i], i, nil
}

func extractBacktick(input string, start int) (string, int) {
	i := start
	n := len(input)
	for i < n {
		if input[i] == '\\' {
			i += 2
			continue
		}
		if input[i] == '`' {
			return input[start:i], i + 1
		}
		i++
	}
	return input[start:], n
}
