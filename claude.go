package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
)

const askUserQuestionHookSettings = `{"hooks":{"PreToolUse":[{"matcher":"AskUserQuestion","hooks":[{"type":"command","command":"echo 'BLOCKED: the built-in AskUserQuestion tool is disabled here. Use the mcp__ask__ask_user_question MCP tool instead. It supports pick_one, pick_many, and pick_diagram question kinds and lets you bundle multiple questions in a single call; the user sees them as tabs and submits all answers together.' >&2; exit 2"}]}]}}`

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
	debugLog("sendToClaude line=%q attachments=%d procNil=%v sessionID=%q",
		line, nAtt, m.proc == nil, m.sessionID)
	m.appendUser(userBarText(line, nAtt))
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
	debugLog("sendToClaude writing payload bytes=%d", len(b))
	if _, err := m.proc.stdin.Write(append(b, '\n')); err != nil {
		debugLog("stdin write err: %v", err)
		m.appendHistory(outputStyle.Render(errStyle.Render("write to claude failed: " + err.Error())))
		m.killProc()
		return m, nil
	}
	debugLog("sendToClaude wrote ok, busy=true")
	m.busy = true
	m.status = "thinking…"
	m.queue = 1
	return m, tea.Batch(
		m.spinner.Tick,
		nextStreamCmd(m.streamCh),
	)
}

func (m model) queueToClaude(line string) (tea.Model, tea.Cmd) {
	if m.proc == nil {
		return m, nil
	}
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": userContent(line, m.pending),
		},
	}
	barText := userBarText(line, len(m.pending))
	m.pending = nil
	b, _ := json.Marshal(payload)
	if _, err := m.proc.stdin.Write(append(b, '\n')); err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("queue write failed: " + err.Error())))
		return m, nil
	}
	m.queue++
	m.pendingPrompts = append(m.pendingPrompts, barText)
	m.layout()
	return m, nil
}

func (m *model) ensureProc() error {
	if m.proc != nil {
		return nil
	}
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if m.mcpPort > 0 {
		args = append(args, "--mcp-config", fmt.Sprintf(`{"mcpServers":{"ask":{"type":"http","url":"http://127.0.0.1:%d/"}}}`, m.mcpPort))
		args = append(args, "--settings", askUserQuestionHookSettings)
	}
	if m.sessionID != "" {
		args = append(args, "--resume", m.sessionID)
	}
	cmd := exec.Command("claude", args...)
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
	m.killProc()
	m.appendHistory(outputStyle.Render(dimStyle.Render("✗ cancelled")))
	return m
}

func (m *model) killProc() {
	if m.proc == nil {
		return
	}
	_ = m.proc.stdin.Close()
	_ = m.proc.cmd.Process.Kill()
	m.proc = nil
	m.streamCh = nil
	m.queue = 0
	m.pendingPrompts = nil
	m.busy = false
	m.status = ""
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
		}
	}
	err := proc.cmd.Wait()
	ch <- claudeExitedMsg{err: err, proc: proc}
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
