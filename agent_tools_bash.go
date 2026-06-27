package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/fantasy"
)

const (
	agentBashDefaultTimeout = 120 * time.Second
	agentBashMaxTimeout     = 600 * time.Second
	// agentBashRawCap bounds in-memory accumulation before the final
	// middle-out truncation; keeps a runaway command from ballooning
	// the process while still preserving head and tail.
	agentBashRawCap = 4 * agentMaxToolOutput
)

// shellResult is the terminal state of a shell invocation.
type shellResult struct {
	exitCode int
	err      error
}

// shellHandle is a running shell command: a stream of output chunks, a
// terminal result, and a kill switch that takes the whole process
// group down. The exec layer is behind a var so tests can fake
// processes without spawning (per the repo's no-subprocess test rule).
type shellHandle struct {
	output <-chan string
	done   <-chan shellResult
	kill   func()
}

var agentRunShell = runShellProcess

// runShellProcess forks $SHELL -c <command> in dir with its own
// process group (same conventions as shell.go: Setpgid without Setsid
// — combining them makes setpgid fail with EPERM). stdout and stderr
// are interleaved as they arrive.
func runShellProcess(dir, command string) (*shellHandle, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out := make(chan string, 64)
	done := make(chan shellResult, 1)
	var wg sync.WaitGroup
	scan := func(r interface{ Read([]byte) (int, error) }) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			out <- sc.Text() + "\n"
		}
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)
	go func() {
		wg.Wait()
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = -1
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
				err = nil
			}
		}
		done <- shellResult{exitCode: code, err: err}
		close(out)
	}()
	pid := cmd.Process.Pid
	return &shellHandle{
		output: out,
		done:   done,
		kill: func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		},
	}, nil
}

// agentJob is one background shell command. Output accumulates into a
// capped buffer the model polls with job_output.
type agentJob struct {
	id      string
	command string

	mu        sync.Mutex
	buf       strings.Builder
	truncated bool
	done      bool
	result    shellResult

	disableSavings  bool
	savingsRecorded bool

	kill   func()
	doneCh chan struct{}
}

func (j *agentJob) appendOutput(chunk string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.buf.Len() >= agentBashRawCap {
		j.truncated = true
		return
	}
	j.buf.WriteString(chunk)
}

func (j *agentJob) snapshot() (output string, truncated, done bool, result shellResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buf.String(), j.truncated, j.done, j.result
}

func (j *agentJob) finish(r shellResult) {
	j.mu.Lock()
	j.done = true
	j.result = r
	j.mu.Unlock()
	close(j.doneCh)
}

type agentJobManager struct {
	mu   sync.Mutex
	seq  int
	jobs map[string]*agentJob
}

func newAgentJobManager() *agentJobManager {
	return &agentJobManager{jobs: map[string]*agentJob{}}
}

func (m *agentJobManager) add(command string, disableSavings bool, kill func()) *agentJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	job := &agentJob{
		id:             fmt.Sprintf("job-%d", m.seq),
		command:        command,
		disableSavings: disableSavings,
		kill:           kill,
		doneCh:         make(chan struct{}),
	}
	m.jobs[job.id] = job
	return job
}

func (m *agentJobManager) get(id string) *agentJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// killAll tears down every still-running job; called when the session
// goroutine exits so background processes never outlive their tab.
func (m *agentJobManager) killAll() {
	m.mu.Lock()
	jobs := make([]*agentJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	for _, j := range jobs {
		j.mu.Lock()
		running := !j.done
		j.mu.Unlock()
		if running && j.kill != nil {
			j.kill()
		}
	}
}

const agentBashToolDescription = `Run a shell command in the working directory and return its combined stdout/stderr (interleaved, truncated middle-out past 30000 chars). Standard noisy command output is automatically compressed to save tokens; set disable_token_savings to true if you strictly need raw uncompressed output. Commands run in independent shells — no state persists between calls, so prefer absolute paths over cd. Set run_in_background for servers and long builds, then poll with job_output and stop with job_kill. Quote paths containing spaces.`

type agentBashParams struct {
	Command             string `json:"command" description:"the shell command to execute"`
	Description         string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this command does"`
	Timeout             int    `json:"timeout,omitempty" description:"max seconds to wait before the command is killed (default 120, max 600)"`
	RunInBackground     bool   `json:"run_in_background,omitempty" description:"start the command as a background job and return its job id immediately"`
	DisableTokenSavings bool   `json:"disable_token_savings,omitempty" description:"set to true to disable standard output filtering for this command if raw uncompressed output is strictly needed"`
}

func agentBashTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"bash",
		agentBashToolDescription,
		func(ctx context.Context, p agentBashParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			command := strings.TrimSpace(p.Command)
			if command == "" {
				return fantasy.NewTextErrorResponse("command is required"), nil
			}
			if !agentSafeShellCommand(command) {
				if denied := env.requestApproval(ctx, "bash", map[string]any{
					"command":     command,
					"description": p.Description,
				}); denied != nil {
					return *denied, nil
				}
			}

			handle, err := agentRunShell(env.cwd, command)
			if err != nil {
				return fantasy.NewTextErrorResponse("could not start shell: " + err.Error()), nil
			}

			if p.RunInBackground {
				job := env.jobs.add(command, p.DisableTokenSavings, handle.kill)
				go func() {
					for chunk := range handle.output {
						job.appendOutput(chunk)
					}
					job.finish(<-handle.done)
					if env.emit != nil {
						env.emit(bgTaskEndedMsg{taskID: job.id})
					}
				}()
				if env.emit != nil {
					env.emit(bgTaskStartedMsg{taskID: job.id})
				}
				return fantasy.NewTextResponse(fmt.Sprintf(
					"started background job %s; poll it with job_output and stop it with job_kill", job.id)), nil
			}

			timeout := agentBashDefaultTimeout
			if p.Timeout > 0 {
				timeout = min(time.Duration(p.Timeout)*time.Second, agentBashMaxTimeout)
			}
			timer := time.NewTimer(timeout)
			defer timer.Stop()

			var buf strings.Builder
			handleFinalOutput := func(rawStr string) string {
				if p.DisableTokenSavings {
					return rawStr
				}
				filteredStr, tokensSaved := applyBashFilter(command, rawStr)
				if tokensSaved > 0 {
					_ = RecordSavings(extractBaseCommand(command), tokensSaved)
				}
				return filteredStr
			}
			rawTruncated := false
			collect := func(chunk string) {
				if buf.Len() >= agentBashRawCap {
					rawTruncated = true
					return
				}
				buf.WriteString(chunk)
			}
			for {
				select {
				case chunk, ok := <-handle.output:
					if !ok {
						res := <-handle.done
						return bashResponse(handleFinalOutput(buf.String()), rawTruncated, res), nil
					}
					collect(chunk)
				case <-timer.C:
					handle.kill()
					drainShellOutput(handle.output, collect)
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"command timed out after %s and was killed\n%s",
						timeout, truncateMiddle(handleFinalOutput(buf.String())))), nil
				case <-ctx.Done():
					handle.kill()
					drainShellOutput(handle.output, collect)
					return fantasy.NewTextErrorResponse("command cancelled\n" + truncateMiddle(handleFinalOutput(buf.String()))), nil
				}
			}
		},
	)
}

// drainShellOutput consumes the remainder of a killed command's output
// channel so its scanner goroutines can exit.
func drainShellOutput(ch <-chan string, collect func(string)) {
	for chunk := range ch {
		collect(chunk)
	}
}

func bashResponse(output string, rawTruncated bool, res shellResult) fantasy.ToolResponse {
	body := truncateMiddle(output)
	if rawTruncated {
		body += "\n(output exceeded the in-memory cap; middle portions were dropped)"
	}
	if res.err != nil {
		return fantasy.NewTextErrorResponse(body + "\nshell error: " + res.err.Error())
	}
	if res.exitCode != 0 {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%sExit code %d", body, res.exitCode))
	}
	if strings.TrimSpace(body) == "" {
		return fantasy.NewTextResponse("(no output)")
	}
	return fantasy.NewTextResponse(body)
}

const agentJobOutputToolDescription = `Read the accumulated output of a background job started with bash run_in_background. Set wait to block until the job exits (up to 30s).`

type agentJobOutputParams struct {
	JobID       string `json:"job_id" description:"the job id returned when the background command started"`
	Wait        bool   `json:"wait,omitempty" description:"block until the job finishes (30s cap) before returning output"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentJobOutputTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"job_output",
		agentJobOutputToolDescription,
		func(ctx context.Context, p agentJobOutputParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			job := env.jobs.get(p.JobID)
			if job == nil {
				return fantasy.NewTextErrorResponse("no such job: " + p.JobID), nil
			}
			if p.Wait {
				select {
				case <-job.doneCh:
				case <-time.After(30 * time.Second):
				case <-ctx.Done():
					return fantasy.NewTextErrorResponse("cancelled while waiting for " + p.JobID), nil
				}
			}
			output, truncated, done, res := job.snapshot()
			if !job.disableSavings {
				filtered, saved := applyBashFilter(job.command, output)
				if done {
					var shouldRecord bool
					job.mu.Lock()
					if !job.savingsRecorded && saved > 0 {
						job.savingsRecorded = true
						shouldRecord = true
					}
					job.mu.Unlock()
					if shouldRecord {
						_ = RecordSavings(extractBaseCommand(job.command), saved)
					}
				}
				output = filtered
			}
			body := truncateMiddle(output)
			if truncated {
				body += "\n(output exceeded the in-memory cap; middle portions were dropped)"
			}
			status := "still running"
			if done {
				status = fmt.Sprintf("exited with code %d", res.exitCode)
				if res.err != nil {
					status = "failed: " + res.err.Error()
				}
			}
			if strings.TrimSpace(body) == "" {
				body = "(no output yet)"
			}
			return fantasy.NewTextResponse(fmt.Sprintf("[%s %s — %s]\n%s", p.JobID, job.command, status, body)), nil
		},
	)
}

const agentJobKillToolDescription = `Kill a background job started with bash run_in_background. The job's whole process group receives SIGKILL.`

type agentJobKillParams struct {
	JobID       string `json:"job_id" description:"the job id to kill"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentJobKillTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"job_kill",
		agentJobKillToolDescription,
		func(ctx context.Context, p agentJobKillParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			job := env.jobs.get(p.JobID)
			if job == nil {
				return fantasy.NewTextErrorResponse("no such job: " + p.JobID), nil
			}
			_, _, done, res := job.snapshot()
			if done {
				return fantasy.NewTextResponse(fmt.Sprintf("%s already exited with code %d", p.JobID, res.exitCode)), nil
			}
			if job.kill != nil {
				job.kill()
			}
			select {
			case <-job.doneCh:
			case <-time.After(5 * time.Second):
				return fantasy.NewTextErrorResponse(p.JobID + " did not exit within 5s of SIGKILL"), nil
			case <-ctx.Done():
				return fantasy.NewTextErrorResponse("cancelled while waiting for " + p.JobID + " to die"), nil
			}
			return fantasy.NewTextResponse("killed " + p.JobID), nil
		},
	)
}

// agentSafeShellCommands are read-only commands that run without an
// approval prompt. A command qualifies only when it both starts with a
// safe entry AND contains no chaining/substitution metacharacters.
var agentSafeShellCommands = []string{
	"cat", "date", "df", "du", "echo", "env", "file", "find", "free",
	"git blame", "git branch", "git diff", "git log", "git ls-files",
	"git remote", "git show", "git status", "git stash list", "git tag",
	"go env", "go list", "go version", "go vet",
	"grep", "head", "hostname", "id", "ls", "ps", "pwd", "rg", "sort",
	"stat", "tail", "tree", "uname", "uniq", "uptime", "wc", "which",
	"whoami",
}

// agentSafeShellCommand reports whether command may run without an
// approval prompt: read-only prefix, no shell metacharacters that
// could chain a mutating command behind it.
func agentSafeShellCommand(command string) bool {
	if strings.ContainsAny(command, ";|&`$<>(){}") {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(command))
	for _, safe := range agentSafeShellCommands {
		if lower == safe {
			return true
		}
		if strings.HasPrefix(lower, safe+" ") {
			return true
		}
	}
	return false
}
