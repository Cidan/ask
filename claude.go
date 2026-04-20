package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
)

func userContent(line string, image []byte, mime string) any {
	if image == nil {
		return line
	}
	blocks := []map[string]any{}
	if line != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": line})
	}
	blocks = append(blocks, map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mime,
			"data":       base64.StdEncoding.EncodeToString(image),
		},
	})
	return blocks
}

func userBarText(line string, hasImage bool) string {
	switch {
	case hasImage && line == "":
		return "[image attached]"
	case hasImage:
		return line + "  [image attached]"
	default:
		return line
	}
}

func (m model) sendToClaude(line string) (tea.Model, tea.Cmd) {
	hasImg := m.pendingImage != nil
	debugLog("sendToClaude line=%q hasImage=%v imageBytes=%d mime=%q procNil=%v sessionID=%q",
		line, hasImg, len(m.pendingImage), m.pendingMime, m.proc == nil, m.sessionID)
	m.appendUser(userBarText(line, hasImg))
	if err := m.ensureProc(); err != nil {
		debugLog("ensureProc err: %v", err)
		m.appendHistory(outputStyle.Render(errStyle.Render("could not start claude: " + err.Error())))
		return m, nil
	}
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": userContent(line, m.pendingImage, m.pendingMime),
		},
	}
	m.pendingImage = nil
	m.pendingMime = ""
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
			"content": userContent(line, m.pendingImage, m.pendingMime),
		},
	}
	barText := userBarText(line, m.pendingImage != nil)
	m.pendingImage = nil
	m.pendingMime = ""
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
	m.proc = &claudeProc{cmd: cmd, stdin: stdin, stderr: stderr}
	m.streamCh = ch
	go readClaudeStream(stdout, cmd, ch)
	return nil
}

func (m *model) killProc() {
	if m.proc == nil {
		return
	}
	_ = m.proc.stdin.Close()
	_ = m.proc.cmd.Process.Kill()
	_ = m.proc.cmd.Wait()
	m.proc = nil
	m.streamCh = nil
	m.queue = 0
	m.pendingPrompts = nil
	m.busy = false
	m.status = ""
}

func readClaudeStream(stdout io.Reader, cmd *exec.Cmd, ch chan tea.Msg) {
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
				ch <- streamStatusMsg{status: status}
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
			ch <- claudeDoneMsg{res: pending}
		}
	}
	err := cmd.Wait()
	ch <- claudeExitedMsg{err: err}
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

func nextStreamCmd(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}
