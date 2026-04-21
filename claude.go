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

const askUserQuestionHookSettings = `{"hooks":{"PreToolUse":[{"matcher":"AskUserQuestion","hooks":[{"type":"command","command":"echo 'BLOCKED: the built-in AskUserQuestion tool is disabled here. Use the mcp__ask__ask_user_question MCP tool instead. It supports pick_one, pick_many, and pick_diagram question kinds and lets you bundle multiple questions in a single call; the user sees them as tabs and submits all answers together.' >&2; exit 2"}]}]}}`

// MCP tool calls block on the user-question modal; default timeout is too short.
const mcpTimeoutMillis = "86400000"

func claudeEnv() []string {
	return append(os.Environ(), "MCP_TIMEOUT="+mcpTimeoutMillis)
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

func (m model) sendToClaude(line string) (tea.Model, tea.Cmd) {
	nAtt := len(m.pending)
	debugLog("sendToClaude line=%q attachments=%d procNil=%v busy=%v sessionID=%q",
		line, nAtt, m.proc == nil, m.busy, m.sessionID)
	m.appendUser(userBarText(line, nAtt))
	newProc := m.proc == nil
	wasIdle := !m.busy
	if err := m.ensureProc(); err != nil {
		debugLog("ensureProc err: %v", err)
		m.appendHistory(outputStyle.Render(errStyle.Render("could not start claude: " + err.Error())))
		return m, nil
	}
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": userContent(line, m.pending),
		},
	}
	m.pending = nil
	b, _ := json.Marshal(payload)
	debugLog("sendToClaude writing payload bytes=%d newProc=%v wasIdle=%v", len(b), newProc, wasIdle)
	if _, err := m.proc.stdin.Write(append(b, '\n')); err != nil {
		debugLog("stdin write err: %v", err)
		m.appendHistory(outputStyle.Render(errStyle.Render("write to claude failed: " + err.Error())))
		m.killProc()
		return m, nil
	}
	m.busy = true
	m.status = "thinking…"
	var cmds []tea.Cmd
	if newProc {
		cmds = append(cmds, nextStreamCmd(m.streamCh))
	}
	if wasIdle {
		cmds = append(cmds, m.spinner.Tick)
	}
	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}

func (m *model) ensureProc() error {
	if m.proc != nil {
		return nil
	}
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
	}
	if m.mcpPort > 0 {
		args = append(args, "--mcp-config", fmt.Sprintf(`{"mcpServers":{"ask":{"type":"http","url":"http://127.0.0.1:%d/"}}}`, m.mcpPort))
		args = append(args, "--settings", askUserQuestionHookSettings)
		args = append(args, "--permission-prompt-tool", "mcp__ask__approval_prompt")
	}
	if m.claudeModel != "" {
		args = append(args, "--model", m.claudeModel)
	}
	if m.sessionID != "" {
		args = append(args, "--resume", m.sessionID)
	}
	cmd := exec.Command("claude", args...)
	cmd.Env = claudeEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &stderrBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	ch := make(chan tea.Msg, 32)
	proc := &claudeProc{cmd: cmd, stdin: stdin, stderr: stderr}
	m.proc = proc
	m.streamCh = ch
	go readClaudeStream(stdout, proc, ch)
	return nil
}

func (m model) cancelTurn() model {
	if !m.busy && m.proc == nil {
		return m
	}
	m.flushTurnBuffer()
	m.killProc()
	m.appendHistory(outputStyle.Render(dimStyle.Render("✗ cancelled")))
	return m
}

func (m *model) flushTurnBuffer() {
	if len(m.turnBuffer) == 0 {
		return
	}
	combined := strings.Join(m.turnBuffer, "\n\n")
	m.turnBuffer = nil
	m.appendResponse(combined)
}

func (m *model) killProc() {
	if m.proc == nil {
		return
	}
	_ = m.proc.stdin.Close()
	_ = m.proc.cmd.Process.Kill()
	m.proc = nil
	m.streamCh = nil
	m.busy = false
	m.status = ""
	m.todos = nil
}

func readClaudeStream(stdout io.Reader, proc *claudeProc, ch chan tea.Msg) {
	defer close(ch)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	var pending claudeResult
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch t, _ := ev["type"].(string); t {
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
			pending = claudeResult{Type: "result"}
			if r, _ := ev["result"].(string); r != "" {
				pending.Result = r
			}
			if id, _ := ev["session_id"].(string); id != "" {
				pending.SessionID = id
			}
			pending.IsError, _ = ev["is_error"].(bool)
			ch <- claudeDoneMsg{res: pending, proc: proc}
			ch <- turnCompleteMsg{proc: proc}
		}
	}
	err := proc.cmd.Wait()
	ch <- claudeExitedMsg{err: err, proc: proc}
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

func probeClaudeInitCmd(mcpPort int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		args := []string{"-p",
			"--input-format", "stream-json",
			"--output-format", "stream-json",
			"--verbose",
			"--dangerously-skip-permissions",
		}
		if mcpPort > 0 {
			args = append(args,
				"--mcp-config", fmt.Sprintf(`{"mcpServers":{"ask":{"type":"http","url":"http://127.0.0.1:%d/"}}}`, mcpPort),
				"--settings", askUserQuestionHookSettings,
			)
		}

		cmd := exec.CommandContext(ctx, "claude", args...)
		cmd.Env = claudeEnv()
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return claudeInitLoadedMsg{err: err}
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return claudeInitLoadedMsg{err: err}
		}
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			return claudeInitLoadedMsg{err: err}
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
				debugLog("probeClaudeInit got %d slash commands", len(ev.SlashCommands))
				return claudeInitLoadedMsg{slashCmds: enrichSlashCommands(ev.SlashCommands)}
			}
		}
		return claudeInitLoadedMsg{err: fmt.Errorf("claude init event not seen")}
	}
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
