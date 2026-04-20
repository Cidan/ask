package main

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(msg.Width - 5)
		m.renderer = newRenderer(msg.Width)
		for i := range m.history {
			if m.history[i].kind == histResponse {
				m.history[i].rendered = ""
			}
		}
		m.layout()
		return m, nil

	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.layout()
		return m, cmd

	case streamStatusMsg:
		m.status = msg.status
		m.layout()
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case claudeExitedMsg:
		var stderrTail string
		if m.proc != nil && m.proc.stderr != nil {
			stderrTail = strings.TrimSpace(m.proc.stderr.String())
		}
		debugLog("claudeExitedMsg err=%v stderrLen=%d", msg.err, len(stderrTail))
		m.busy = false
		m.status = ""
		m.queue = 0
		m.pendingPrompts = nil
		m.streamCh = nil
		m.proc = nil
		if msg.err != nil || stderrTail != "" {
			out := "claude exited"
			if msg.err != nil {
				out += ": " + msg.err.Error()
			}
			if stderrTail != "" {
				out += "\n" + stderrTail
			}
			m.appendHistory(outputStyle.Render(errStyle.Render(out)))
		}
		return m, nil

	case claudeDoneMsg:
		debugLog("claudeDoneMsg err=%v isError=%v resultLen=%d queue=%d",
			msg.err, msg.res.IsError, len(msg.res.Result), m.queue)
		if m.queue > 0 {
			m.queue--
		}
		stillBusy := m.queue > 0
		m.busy = stillBusy
		if stillBusy {
			m.status = "thinking…"
		} else {
			m.status = ""
		}
		switch {
		case msg.err != nil:
			out := errStyle.Render(fmt.Sprintf("error: %v", msg.err))
			if msg.raw != "" {
				out += "\n" + dimStyle.Render(msg.raw)
			}
			m.appendHistory(outputStyle.Render(out))
		case msg.res.IsError:
			m.appendHistory(outputStyle.Render(errStyle.Render("error: " + msg.res.Result)))
		default:
			if msg.res.SessionID != "" {
				m.sessionID = msg.res.SessionID
			}
			m.appendResponse(msg.res.Result)
		}
		if len(m.pendingPrompts) > 0 {
			next := m.pendingPrompts[0]
			m.pendingPrompts = m.pendingPrompts[1:]
			m.appendUser(next)
		}
		if stillBusy && m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		m.refreshPathMatches()
		m.layout()
		return m, nil

	case historyLoadedMsg:
		if msg.sessionID != m.sessionID {
			return m, nil
		}
		if msg.err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render(
				"could not load session history: " + msg.err.Error())))
			return m, nil
		}
		m.history = append(msg.entries, historyEntry{
			kind: histPrerendered,
			text: outputStyle.Render(promptStyle.Render(
				fmt.Sprintf("✓ resumed session %s", short(m.sessionID)))),
		})
		m.layout()
		m.viewport.GotoBottom()
		return m, nil

	case sessionsLoadedMsg:
		if msg.err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render(fmt.Sprintf("could not load sessions: %v", msg.err))))
			return m, nil
		}
		if len(msg.sessions) == 0 {
			m.appendHistory(outputStyle.Render(dimStyle.Render("no prior sessions for this project")))
			return m, nil
		}
		m.sessions = msg.sessions
		m.pickerIdx = 0
		m.mode = modeSessionPicker
		return m, nil

	case imagePastedMsg:
		debugLog("imagePastedMsg bytes=%d mime=%q pngBytes=%d w=%d h=%d err=%v",
			len(msg.data), msg.mime, len(msg.pngForKitty), msg.width, msg.height, msg.err)
		if msg.err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render("paste: " + msg.err.Error())))
			return m, nil
		}
		m.pendingImage = msg.data
		m.pendingMime = msg.mime
		m.pendingThumbCols = 0
		m.pendingThumbRows = 0
		if isKitty() && msg.pngForKitty != nil {
			if err := kittyTransmitPNG(pendingImageID, msg.pngForKitty); err != nil {
				debugLog("kitty transmit err: %v", err)
			} else {
				m.pendingThumbCols, m.pendingThumbRows = thumbnailGrid(msg.width, msg.height)
			}
		}
		m.layout()
		return m, nil

	case tea.MouseWheelMsg:
		if m.mode == modeInput {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && m.mode == modeInput {
			vpH := m.viewport.Height()
			if msg.X == m.width-1 && msg.Y >= 0 && msg.Y < vpH && m.viewport.TotalLineCount() > vpH {
				m.scrollbarDragging = true
				m.scrollViewportTo(msg.Y)
				return m, nil
			}
		}
		return m, nil

	case tea.MouseMotionMsg:
		if m.scrollbarDragging {
			m.scrollViewportTo(msg.Y)
			return m, nil
		}
		return m, nil

	case tea.MouseReleaseMsg:
		if m.scrollbarDragging {
			m.scrollbarDragging = false
		}
		return m, nil

	case tea.PasteMsg:
		if m.mode == modeInput && !m.busy {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			m.refreshPathMatches()
			m.layout()
			return m, cmd
		}
		return m, nil

	case tea.KeyPressMsg:
		switch m.mode {
		case modeSessionPicker:
			return m.updatePicker(msg)
		default:
			return m.updateInput(msg)
		}
	}
	return m, nil
}

func (m model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && (msg.Code == 'c' || msg.Code == 'd') {
		return m, tea.Quit
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'v' {
		return m, pasteImageCmd()
	}
	if msg.Mod == 0 && msg.Code == tea.KeyEsc && m.pendingImage != nil {
		m.pendingImage = nil
		m.pendingMime = ""
		m.pendingThumbCols = 0
		m.pendingThumbRows = 0
		m.layout()
		return m, nil
	}

	items := m.filterSlashCmds()
	menuOpen := !m.busy && len(items) > 0
	pickOpen := !m.busy && m.pathPickerActive() && len(m.pathMatches) > 0

	if msg.Mod == 0 {
		switch msg.Code {
		case tea.KeyUp:
			if pickOpen {
				if m.pathIdx > 0 {
					m.pathIdx--
				}
				return m, nil
			}
			if menuOpen {
				if m.menuIdx > 0 {
					m.menuIdx--
				}
				return m, nil
			}
		case tea.KeyDown:
			if pickOpen {
				if m.pathIdx < len(m.pathMatches)-1 {
					m.pathIdx++
				}
				return m, nil
			}
			if menuOpen {
				if m.menuIdx < len(items)-1 {
					m.menuIdx++
				}
				return m, nil
			}
		case tea.KeyTab:
			if pickOpen {
				pick := m.pathMatches[m.pathIdx]
				m.input.SetValue(m.pathPickerCmd() + " " + pick + "/")
				m.refreshPathMatches()
				m.layout()
				return m, nil
			}
			if menuOpen {
				pick := items[m.menuIdx].name
				m.input.SetValue(pick)
				m.layout()
				return m, nil
			}
		case tea.KeyPgUp:
			m.viewport.ScrollUp(m.viewport.Height() / 2)
			return m, nil
		case tea.KeyPgDown:
			m.viewport.ScrollDown(m.viewport.Height() / 2)
			return m, nil
		case tea.KeyEnter:
			val := m.input.Value()
			line := strings.TrimSpace(val)
			debugLog("Enter line=%q valLen=%d busy=%v pendingImg=%v pathCmd=%q bare=%q",
				line, len(val), m.busy, m.pendingImage != nil, m.pathPickerCmd(), bareCommand(line))
			if line == "" && m.pendingImage == nil {
				return m, nil
			}
			if m.busy {
				if strings.HasPrefix(line, "/") || m.pathPickerCmd() != "" || bareCommand(line) != "" {
					return m, nil
				}
				m.input.Reset()
				m.layout()
				return m.queueToClaude(val)
			}
			if cmd := m.pathPickerCmd(); cmd != "" {
				target := strings.TrimSpace(m.pathQuery())
				if len(m.pathMatches) > 0 {
					target = m.pathMatches[m.pathIdx]
				}
				m.input.Reset()
				m.refreshPathMatches()
				m.layout()
				return m.runPathCommand(cmd, target)
			}
			if cmd := bareCommand(line); cmd != "" {
				m.input.Reset()
				m.layout()
				return m.runPathCommand(cmd, "")
			}
			m.input.Reset()
			m.menuIdx = 0
			if strings.HasPrefix(line, "/") {
				m.layout()
				return m.handleCommand(line)
			}
			return m.sendToClaude(val)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if items := m.filterSlashCmds(); m.menuIdx >= len(items) {
		m.menuIdx = 0
	}
	if !m.busy {
		m.refreshPathMatches()
	} else {
		m.pathMatches = nil
		m.pathIdx = 0
	}
	m.layout()
	return m, cmd
}

func (m model) updatePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		return m, tea.Quit
	}
	switch msg.Code {
	case tea.KeyEsc:
		m.mode = modeInput
		return m, nil
	case tea.KeyUp:
		if m.pickerIdx > 0 {
			m.pickerIdx--
		}
	case tea.KeyDown:
		if m.pickerIdx < len(m.sessions)-1 {
			m.pickerIdx++
		}
	case tea.KeyEnter:
		if len(m.sessions) > 0 {
			m.killProc()
			sid := m.sessions[m.pickerIdx].id
			m.sessionID = sid
			m.mode = modeInput
			m.history = nil
			m.appendHistory(outputStyle.Render(dimStyle.Render(
				fmt.Sprintf("loading session %s…", short(sid)))))
			return m, loadHistoryCmd(sid)
		}
	}
	return m, nil
}

func (m model) handleCommand(line string) (tea.Model, tea.Cmd) {
	cmd, _, _ := strings.Cut(line, " ")
	switch cmd {
	case "/resume":
		return m, loadSessionsCmd()
	case "/new", "/clear":
		m.killProc()
		m.sessionID = ""
		m.history = nil
		m.appendHistory(outputStyle.Render(promptStyle.Render("✓ new session")))
		return m, nil
	default:
		m.appendHistory(outputStyle.Render(errStyle.Render("unknown command: " + cmd)))
		return m, nil
	}
}

func (m model) filterSlashCmds() []slashCmd {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") {
		return nil
	}
	var out []slashCmd
	for _, c := range slashCmds {
		if strings.HasPrefix(c.name, val) {
			out = append(out, c)
		}
	}
	return out
}
