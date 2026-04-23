package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// claudeProvider implements the Provider interface for Anthropic's
// claude CLI (stream-json mode). The struct is stateless — every
// per-session piece of state lives on the providerProc handle.
type claudeProvider struct{}

func init() { registerProvider(claudeProvider{}) }

func (claudeProvider) ID() string          { return "claude" }
func (claudeProvider) DisplayName() string { return "Claude" }

func (claudeProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		NativeWorktree:      true,
		Resume:              true,
		ModelPicker:         true,
		EffortPicker:        true,
		AskUserQuestionMCP:  true,
		PermissionPromptMCP: true,
	}
}

func (claudeProvider) ModelPicker() ProviderPicker {
	return ProviderPicker{
		Prompt:      "Select Claude model",
		Options:     []string{"default", "haiku", "sonnet", "sonnet[1m]", "opus", "opus[1m]", ollamaModelOption},
		AllowCustom: true,
		SubConfig:   map[string]string{ollamaModelOption: "ollama"},
	}
}

func (claudeProvider) EffortOptions() []string {
	return []string{"default", "low", "medium", "high", "xhigh", "max"}
}

func (claudeProvider) BaseSlashCommands() []slashCmd {
	return []slashCmd{
		{"/resume", "resume a previous Claude session"},
		{"/new", "start a new Claude session"},
		{"/clear", "start a new Claude session"},
		{"/model", "select the Claude model"},
		{"/effort", "select the Claude reasoning effort"},
	}
}

// askUserQuestionHookSettings redirects the built-in AskUserQuestion
// tool to our MCP bridge via a PreToolUse hook. Claude's MCP tool calls
// block on the user-question modal so the default timeout is too short.
const askUserQuestionHookSettings = `{"hooks":{"PreToolUse":[{"matcher":"AskUserQuestion","hooks":[{"type":"command","command":"echo 'BLOCKED: the built-in AskUserQuestion tool is disabled here. Use the mcp__ask__ask_user_question MCP tool instead. It supports pick_one, pick_many, and pick_diagram question kinds and lets you bundle multiple questions in a single call; the user sees them as tabs and submits all answers together.' >&2; exit 2"}]}]}}`

const mcpTimeoutMillis = "86400000"

// claudeEnv returns the claude subprocess environment, routing Anthropic
// traffic through a local ollama host when model == "ollama".
func claudeEnv(args ProviderSessionArgs) []string {
	env := append(os.Environ(), "MCP_TIMEOUT="+mcpTimeoutMillis)
	if strings.EqualFold(args.Model, "ollama") {
		env = append(env,
			"ANTHROPIC_BASE_URL="+ollamaBaseURL(args.OllamaHost),
			"ANTHROPIC_AUTH_TOKEN=ollama",
		)
	}
	return env
}

// claudeCLIArgs builds the argv for `claude -p`. Passing probe=true
// omits --include-partial-messages and the permission-prompt tool so
// the short-lived init probe doesn't trip permissions.
func claudeCLIArgs(args ProviderSessionArgs, probe bool) []string {
	out := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if !probe {
		out = append(out, "--include-partial-messages")
	}
	if args.SkipAllPermissions {
		out = append(out, "--dangerously-skip-permissions")
	}
	if args.MCPPort > 0 {
		out = append(out, "--mcp-config",
			fmt.Sprintf(`{"mcpServers":{"ask":{"type":"http","url":"http://127.0.0.1:%d/"}}}`, args.MCPPort))
		out = append(out, "--settings", askUserQuestionHookSettings)
		if !probe {
			out = append(out, "--permission-prompt-tool", "mcp__ask__approval_prompt")
		}
	}
	switch {
	case strings.EqualFold(args.Model, "ollama"):
		if args.OllamaModel != "" {
			out = append(out, "--model", args.OllamaModel)
		}
	case args.Model != "":
		out = append(out, "--model", args.Model)
	}
	if args.Effort != "" {
		out = append(out, "--effort", args.Effort)
	}
	if !probe {
		if args.SessionID != "" {
			out = append(out, "--resume", args.SessionID)
		} else if args.Worktree && inGitCheckout() {
			// claude refuses to start with --worktree outside a git checkout,
			// so drop the flag silently in that case.
			out = append(out, "--worktree")
		}
	}
	return out
}

func (claudeProvider) StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	cliArgs := claudeCLIArgs(args, false)
	cmd := exec.Command("claude", cliArgs...)
	cmd.Env = claudeEnv(args)
	// When resuming a worktree session, the jsonl lives under
	// `~/.claude/projects/<main-encoded>--claude-worktrees-<name>/` — claude
	// looks it up via its own cwd, so the subprocess has to run inside that
	// worktree for `--resume` to find the file. For fresh sessions we honour
	// args.Cwd, which the app sets either to the user's cwd or (for
	// externally-managed worktrees, driven by providers with NativeWorktree
	// false) to the ask-created worktree directory.
	switch {
	case args.SessionID != "" && args.ResumeCwd != "":
		if err := ensureResumeWorktree(args.ResumeCwd); err != nil {
			return nil, nil, err
		}
		cmd.Dir = args.ResumeCwd
	case args.Cwd != "":
		cmd.Dir = args.Cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr := &stderrBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	ch := make(chan tea.Msg, 32)
	proc := &providerProc{cmd: cmd, stdin: stdin, stderr: stderr}
	go readClaudeStream(stdout, proc, ch)
	return proc, ch, nil
}

func (claudeProvider) Send(p *providerProc, text string, attachments []pendingAttachment) error {
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": userContent(text, attachments),
		},
	}
	b, _ := json.Marshal(payload)
	_, err := p.stdin.Write(append(b, '\n'))
	return err
}

func (claudeProvider) EnsureResumeCwd(resumeCwd string) error { return ensureResumeWorktree(resumeCwd) }

func (claudeProvider) LoadSettings() ProviderSettings {
	cfg, _ := loadConfig()
	return ProviderSettings{
		Model:         cfg.Claude.Model,
		Effort:        cfg.Claude.Effort,
		SlashCommands: cfg.Claude.SlashCommands,
	}
}

func (claudeProvider) SaveSettings(s ProviderSettings) error {
	cfg, _ := loadConfig()
	cfg.Claude.Model = s.Model
	cfg.Claude.Effort = s.Effort
	cfg.Claude.SlashCommands = s.SlashCommands
	return saveConfig(cfg)
}

func (claudeProvider) ListSessions(cwd string) ([]sessionEntry, error) {
	return loadClaudeSessions(cwd)
}

func (claudeProvider) LoadHistory(sessionID string, opts HistoryOpts) ([]historyEntry, error) {
	return loadClaudeHistory(sessionID, opts)
}

func (claudeProvider) ProbeInit(args ProviderSessionArgs) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		cliArgs := claudeCLIArgs(args, true)
		cmd := exec.CommandContext(ctx, "claude", cliArgs...)
		cmd.Env = claudeEnv(args)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return providerInitLoadedMsg{err: err}
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return providerInitLoadedMsg{err: err}
		}
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			return providerInitLoadedMsg{err: err}
		}
		killed := false
		kill := func() {
			if killed {
				return
			}
			killed = true
			_ = cmd.Process.Kill()
		}
		defer func() {
			kill()
			_ = stdin.Close()
			_ = cmd.Wait()
		}()

		// init isn't emitted until at least one user message arrives on stdin;
		// send a minimal ping and kill the process the moment init lands so the
		// LLM dispatch never happens.
		go func() {
			_, _ = stdin.Write([]byte(`{"type":"user","message":{"role":"user","content":"."}}` + "\n"))
		}()

		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		for sc.Scan() {
			var ev struct {
				Type          string   `json:"type"`
				Subtype       string   `json:"subtype"`
				SlashCommands []string `json:"slash_commands"`
			}
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue
			}
			if ev.Type == "system" && ev.Subtype == "init" {
				kill()
				debugLog("claude ProbeInit got %d slash commands", len(ev.SlashCommands))
				return providerInitLoadedMsg{slashCmds: enrichSlashCommands(ev.SlashCommands)}
			}
		}
		return providerInitLoadedMsg{err: fmt.Errorf("claude init event not seen")}
	}
}

func userContent(line string, attachments []pendingAttachment) any {
	if len(attachments) == 0 {
		return line
	}
	blocks := []map[string]any{}
	if line != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": line})
	}
	for _, a := range attachments {
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": a.mime,
				"data":       base64.StdEncoding.EncodeToString(a.data),
			},
		})
	}
	return blocks
}

func userBarText(line string, n int) string {
	switch {
	case n == 0:
		return line
	case n == 1 && line == "":
		return "[image attached]"
	case n == 1:
		return line + "  [image attached]"
	case line == "":
		return fmt.Sprintf("[%d images attached]", n)
	default:
		return line + fmt.Sprintf("  [%d images attached]", n)
	}
}

// readClaudeStream translates claude's stream-json wire protocol into
// the provider-neutral tea.Msg set. Each event emitted here carries the
// proc pointer so the UI can drop messages from stale subprocesses.
func readClaudeStream(stdout io.Reader, proc *providerProc, ch chan tea.Msg) {
	defer close(ch)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	var pending providerResult
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch t, _ := ev["type"].(string); t {
		case "system":
			subtype, _ := ev["subtype"].(string)
			switch subtype {
			case "init":
				if cwd, _ := ev["cwd"].(string); cwd != "" {
					ch <- providerCwdMsg{cwd: cwd, proc: proc}
				}
			case "task_started":
				// task_type is optional on the wire and only populated for
				// newer CLIs; task_started itself is only emitted when the
				// Agent tool is invoked with run_in_background=true, so any
				// occurrence counts as a live background worker.
				if id, _ := ev["task_id"].(string); id != "" {
					debugLog("system task_started id=%s task_type=%q desc=%q",
						id, ev["task_type"], ev["description"])
					ch <- bgTaskStartedMsg{taskID: id, proc: proc}
				}
			case "task_notification":
				status, _ := ev["status"].(string)
				id, _ := ev["task_id"].(string)
				debugLog("system task_notification id=%s status=%q", id, status)
				switch status {
				case "completed", "failed", "stopped":
					if id != "" {
						ch <- bgTaskEndedMsg{taskID: id, proc: proc}
					}
				}
			default:
				if subtype != "" {
					debugLog("system subtype=%q keys=%v", subtype, mapKeys(ev))
				}
			}
		case "assistant":
			if status := assistantStatus(ev); status != "" {
				ch <- streamStatusMsg{status: status, proc: proc}
			}
			if todos, ok := assistantTodos(ev); ok {
				ch <- todoUpdatedMsg{todos: todos, proc: proc}
			}
			if text := assistantText(ev); text != "" {
				ch <- assistantTextMsg{text: text, proc: proc}
			}
		case "user":
			if path, hunks, ok := userToolDiff(ev); ok {
				ch <- toolDiffMsg{filePath: path, hunks: hunks, proc: proc}
			}
		case "stream_event":
			if streamEventEndTurn(ev) {
				ch <- turnCompleteMsg{proc: proc}
			}
		case "result":
			pending = providerResult{}
			if r, _ := ev["result"].(string); r != "" {
				pending.Result = r
			}
			if id, _ := ev["session_id"].(string); id != "" {
				pending.SessionID = id
			}
			pending.IsError, _ = ev["is_error"].(bool)
			ch <- providerDoneMsg{res: pending, proc: proc}
			ch <- turnCompleteMsg{proc: proc}
		}
	}
	var err error
	if proc.cmd != nil {
		err = proc.cmd.Wait()
	}
	ch <- providerExitedMsg{err: err, proc: proc}
}

func assistantTodos(ev map[string]any) ([]todoItem, bool) {
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, item := range content {
		b, _ := item.(map[string]any)
		if b["type"] != "tool_use" {
			continue
		}
		if name, _ := b["name"].(string); name != "TodoWrite" {
			continue
		}
		input, _ := b["input"].(map[string]any)
		raw, _ := input["todos"].([]any)
		out := make([]todoItem, 0, len(raw))
		for _, t := range raw {
			tm, _ := t.(map[string]any)
			cnt, _ := tm["content"].(string)
			af, _ := tm["activeForm"].(string)
			st, _ := tm["status"].(string)
			out = append(out, todoItem{Content: cnt, ActiveForm: af, Status: st})
		}
		return out, true
	}
	return nil, false
}

func userToolDiff(ev map[string]any) (string, []diffHunk, bool) {
	result, _ := ev["tool_use_result"].(map[string]any)
	return parseStructuredPatch(result)
}

func parseStructuredPatch(result map[string]any) (string, []diffHunk, bool) {
	if result == nil {
		return "", nil, false
	}
	rawPatch, _ := result["structuredPatch"].([]any)
	if len(rawPatch) == 0 {
		return "", nil, false
	}
	path, _ := result["filePath"].(string)
	hunks := make([]diffHunk, 0, len(rawPatch))
	for _, h := range rawPatch {
		hm, _ := h.(map[string]any)
		if hm == nil {
			continue
		}
		rawLines, _ := hm["lines"].([]any)
		lines := make([]string, 0, len(rawLines))
		for _, l := range rawLines {
			if s, ok := l.(string); ok {
				lines = append(lines, s)
			}
		}
		hunks = append(hunks, diffHunk{
			oldStart: jsonInt(hm["oldStart"]),
			oldLines: jsonInt(hm["oldLines"]),
			newStart: jsonInt(hm["newStart"]),
			newLines: jsonInt(hm["newLines"]),
			lines:    lines,
		})
	}
	if len(hunks) == 0 {
		return "", nil, false
	}
	return path, hunks, true
}

func jsonInt(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func streamEventEndTurn(ev map[string]any) bool {
	event, _ := ev["event"].(map[string]any)
	if event == nil {
		return false
	}
	if t, _ := event["type"].(string); t != "message_delta" {
		return false
	}
	delta, _ := event["delta"].(map[string]any)
	reason, _ := delta["stop_reason"].(string)
	return reason == "end_turn"
}

func assistantText(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	var parts []string
	for _, item := range content {
		b, _ := item.(map[string]any)
		if b["type"] != "text" {
			continue
		}
		if txt, _ := b["text"].(string); txt != "" {
			parts = append(parts, txt)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n")
}

func assistantStatus(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, item := range content {
		m, _ := item.(map[string]any)
		switch m["type"] {
		case "thinking":
			return "thinking…"
		case "tool_use":
			name, _ := m["name"].(string)
			input, _ := m["input"].(map[string]any)
			return formatToolStatus(name, input)
		}
	}
	return ""
}

func formatToolStatus(name string, input map[string]any) string {
	switch name {
	case "Bash":
		if d, _ := input["description"].(string); d != "" {
			return name + ": " + d
		}
		if c, _ := input["command"].(string); c != "" {
			return name + ": " + truncate(c, 60)
		}
	case "Read", "Edit", "Write", "NotebookEdit":
		if p, _ := input["file_path"].(string); p != "" {
			return name + ": " + filepath.Base(p)
		}
	case "Glob", "Grep":
		if p, _ := input["pattern"].(string); p != "" {
			return name + ": " + truncate(p, 60)
		}
	case "WebFetch", "WebSearch":
		if u, _ := input["url"].(string); u != "" {
			return name + ": " + truncate(u, 60)
		}
		if q, _ := input["query"].(string); q != "" {
			return name + ": " + truncate(q, 60)
		}
	case "Task":
		if p, _ := input["subagent_type"].(string); p != "" {
			return name + ": " + p
		}
	case "TaskOutput":
		return "waiting for background task…"
	}
	return name
}

func nextStreamCmd(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}
