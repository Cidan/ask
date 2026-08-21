package tools

import (
	"bufio"
	"errors"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cidan/ask/pkg/engine"
)

const (
	BashDefaultTimeout = 120 * time.Second
	BashMaxTimeout     = 600 * time.Second
	BashRawCap         = 4 * MaxToolOutput
)

// ShellResult is the terminal state of a shell invocation.
type ShellResult struct {
	ExitCode int
	Err      error
}

// ShellHandle represents a running shell command.
type ShellHandle struct {
	Output <-chan string
	Done   <-chan ShellResult
	Kill   func()
}

// RunShell is the swappable execution hook for shell commands.
var RunShell = RunShellProcess

// RunShellProcess forks $SHELL -c <command> with its own process group.
func RunShellProcess(dir, command string, extraEnv ...string) (*ShellHandle, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
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
	done := make(chan ShellResult, 1)
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
		done <- ShellResult{ExitCode: code, Err: err}
		close(out)
	}()
	pid := cmd.Process.Pid
	return &ShellHandle{
		Output: out,
		Done:   done,
		Kill: func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		},
	}, nil
}

// Job represents one background shell command.
type Job struct {
	ID      string
	Command string

	Mu        sync.Mutex
	Buf       strings.Builder
	Truncated bool
	Done      bool
	Result    ShellResult

	DisableSavings  bool
	SavingsRecorded bool

	Kill   func()
	DoneCh chan struct{}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	job := &Job{
		ID:             fmt.Sprintf("job-%d", m.seq),
		Command:        command,
		DisableSavings: disableSavings,
		Kill:           kill,
		DoneCh:         make(chan struct{}),
	}
	m.jobs[job.ID] = job
	return job
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

const BashToolDescription = `Run a shell command in the working directory and return its combined stdout/stderr (interleaved, truncated middle-out past 30000 chars). Standard noisy command output is automatically compressed to save tokens; set disable_token_savings to true if you strictly need raw uncompressed output. Commands run in independent shells — no state persists between calls, so prefer absolute paths over cd. Set run_in_background for servers and long builds, then poll with job_output and stop with job_kill. Quote paths containing spaces.`

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
}

// JobOutputResult is the job_output tool's response.
type JobOutputResult struct {
	JobID   string `json:"job_id,omitempty"`
	Command string `json:"command,omitempty"`
	Status  string `json:"status,omitempty" jsonschema:"running or exited"`
	Output  string `json:"output,omitempty"`
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
			if server := EnsureSudoIPCServer(env.Interaction); server != nil {
				askExe, err := os.Executable()
				if err != nil || askExe == "" {
					askExe = "ask"
				}
				extraEnv = append(extraEnv,
					"SUDO_ASKPASS="+askExe,
					"ASK_SUDO_SOCKET="+server.SocketPath,
					"ASK_SUDO_TOKEN="+server.Token,
					fmt.Sprintf("ASK_SUDO_TABID=%d", env.TabID),
				)
			}

			handle, err := RunShell(env.Cwd, command, extraEnv...)
			if err != nil {
				return BashResult{}, errors.New("could not start shell: " + err.Error())
			}

			if p.RunInBackground {
				job := env.Jobs.Add(command, p.DisableTokenSavings, handle.Kill)
				go func() {
					for chunk := range handle.Output {
						job.AppendOutput(chunk)
					}
					job.Finish(<-handle.Done)
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
				return BashResult{JobID: job.ID, Output: "started background job " + job.ID + "; poll it with job_output and stop it with job_kill"}, nil
			}

			timeout := BashDefaultTimeout
			if p.Timeout > 0 {
				timeout = min(time.Duration(p.Timeout)*time.Second, BashMaxTimeout)
			}
			timer := time.NewTimer(timeout)
			defer timer.Stop()

			var buf strings.Builder
			handleFinalOutput := func(rawStr string) string {
				if p.DisableTokenSavings {
					return rawStr
				}
				filteredStr, tokensSaved := ApplyBashFilter(command, rawStr)
				if tokensSaved > 0 {
					_ = RecordSavings(ExtractBaseCommand(command), tokensSaved)
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
						return bashResponse(handleFinalOutput(buf.String()), rawTruncated, res), nil
					}
					collect(chunk)
				case <-timer.C:
					handle.Kill()
					drainShellOutput(handle.Output, collect)
					return BashResult{TimedOut: true, Output: TruncateMiddle(handleFinalOutput(buf.String()))},
						fmt.Errorf("command timed out after %s and was killed", timeout)
				case <-ctx.Done():
					handle.Kill()
					drainShellOutput(handle.Output, collect)
					return BashResult{Cancelled: true, Output: TruncateMiddle(handleFinalOutput(buf.String()))}, nil
				}
			}
		},
	)
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

const JobOutputToolDescription = `Read the accumulated output of a background job started with bash run_in_background. Set wait to block until the job exits (up to 30s).`

type JobOutputParams struct {
	JobID       string `json:"job_id" jsonschema:"the job id returned when the background command started"`
	Wait        bool   `json:"wait,omitempty" jsonschema:"block until the job finishes (30s cap) before returning output"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// JobOutputTool returns the native job_output tool.
func JobOutputTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"job_output",
		JobOutputToolDescription,
		func(ctx agent.Context, p JobOutputParams) (JobOutputResult, error) {
			job := env.Jobs.Get(p.JobID)
			if job == nil {
				return JobOutputResult{}, errors.New("no such job: " + p.JobID)
			}
			if p.Wait {
				select {
				case <-job.DoneCh:
				case <-time.After(30 * time.Second):
				case <-ctx.Done():
					return JobOutputResult{}, errors.New("cancelled while waiting for " + p.JobID)
				}
			}
			output, truncated, done, res := job.Snapshot()
			if !job.DisableSavings {
				filtered, saved := ApplyBashFilter(job.Command, output)
				if done {
					var shouldRecord bool
					job.Mu.Lock()
					if !job.SavingsRecorded && saved > 0 {
						job.SavingsRecorded = true
						shouldRecord = true
					}
					job.Mu.Unlock()
					if shouldRecord {
						_ = RecordSavings(ExtractBaseCommand(job.Command), saved)
					}
				}
				output = filtered
			}
			body := TruncateMiddle(output)
			if truncated {
				body += "\n(output exceeded the in-memory cap; middle portions were dropped)"
			}
			status := "still running"
			if done {
				status = fmt.Sprintf("exited with code %d", res.ExitCode)
				if res.Err != nil {
					status = "failed: " + res.Err.Error()
				}
			}
			if strings.TrimSpace(body) == "" {
				body = "(no output yet)"
			}
			return JobOutputResult{JobID: p.JobID, Command: job.Command, Status: status, Output: body}, nil
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

var errSudoRequiresAskpass = fmt.Errorf("sudo: commands invoking sudo must use -A as the first argument (e.g., 'sudo -A <command>'). Plain sudo without -A is blocked because ask requires -A to trigger the secure password prompt modal.")

// ValidateSudoCommand checks if command invokes sudo and verifies that -A or --askpass is passed as its first argument.
func ValidateSudoCommand(command string) error {
	return parseAndValidateSudo(command)
}

func parseAndValidateSudo(input string) error {
	var (
		foundCmd      bool
		isSudo        bool
		sudoValidated bool
		skipNext      bool
		i             int
		n             = len(input)
	)

	checkReset := func() error {
		if isSudo && !sudoValidated {
			return errSudoRequiresAskpass
		}
		foundCmd = false
		isSudo = false
		sudoValidated = false
		skipNext = false
		return nil
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

		if !foundCmd {
			if isEnvAssignment(unquotedStr) || isEnvAssignment(rawWord) {
				continue
			}
			foundCmd = true
			base := filepath.Base(unquotedStr)
			if base == "sudo" || unquotedStr == "sudo" || strings.HasSuffix(unquotedStr, "/sudo") {
				isSudo = true
			}
			continue
		}

		if isSudo && !sudoValidated {
			if unquotedStr == "-A" || unquotedStr == "--askpass" || rawWord == "-A" || rawWord == "--askpass" {
				sudoValidated = true
			} else {
				return errSudoRequiresAskpass
			}
		}
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
