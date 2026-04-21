package main

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

func (m model) Init() tea.Cmd {
	debugLog("Init mcpPort=%d", m.mcpPort)
	return tea.Batch(probeClaudeInitCmd(m.mcpPort), cursor.Blink)
}

func (m model) Update(msg tea.Msg) (newModel tea.Model, cmd tea.Cmd) {
	if debugOn {
		defer debugTrace(fmt.Sprintf("Update[%T]", msg), time.Now())
	}
	defer func() {
		if mm, ok := newModel.(model); ok {
			(&mm).layout()
			newModel = mm
		}
	}()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(msg.Width - 5)
		m.renderer = newRenderer(msg.Width)
		for i := range m.history {
			switch m.history[i].kind {
			case histResponse, histUser:
				m.history[i].rendered = ""
			}
		}
		return m, nil

	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case streamStatusMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		wasIdle := !m.busy
		m.busy = true
		m.status = msg.status
		var cmds []tea.Cmd
		if m.streamCh != nil {
			cmds = append(cmds, nextStreamCmd(m.streamCh))
		}
		if wasIdle {
			cmds = append(cmds, m.spinner.Tick)
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)

	case todoUpdatedMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		m.todos = msg.todos
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case toolDiffMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		if m.renderDiffs {
			m.appendHistory(m.renderDiffBlock(msg.filePath, msg.hunks))
		}
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case claudeInitLoadedMsg:
		if msg.err != nil {
			debugLog("claudeInitLoadedMsg err: %v", msg.err)
			return m, nil
		}
		debugLog("claudeInitLoadedMsg slashCmds=%d", len(msg.slashCmds))
		m.claudeSlashCmds = msg.slashCmds
		return m, persistSlashCmdsCmd(msg.slashCmds)

	case claudeExitedMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		var stderrTail string
		if m.proc != nil && m.proc.stderr != nil {
			stderrTail = strings.TrimSpace(m.proc.stderr.String())
		}
		debugLog("claudeExitedMsg err=%v stderrLen=%d", msg.err, len(stderrTail))
		m.flushTurnBuffer()
		m.busy = false
		m.status = ""
		m.todos = nil
		m.streamCh = nil
		m.proc = nil
		m.dismissCancelTurnConfirmIfIdle()
		if m.mode == modeApproval {
			m = m.clearApproval()
		}
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
		if msg.proc != m.proc {
			return m, nil
		}
		debugLog("claudeDoneMsg err=%v isError=%v resultLen=%d",
			msg.err, msg.res.IsError, len(msg.res.Result))
		m.dismissCancelTurnConfirmIfIdle()
		if msg.res.SessionID != "" {
			m.sessionID = msg.res.SessionID
		}
		switch {
		case msg.err != nil:
			out := errStyle.Render(fmt.Sprintf("error: %v", msg.err))
			if msg.raw != "" {
				out += "\n" + dimStyle.Render(msg.raw)
			}
			m.appendHistory(outputStyle.Render(out))
			m.busy = false
			m.status = ""
			m.todos = nil
		case msg.res.IsError:
			m.appendHistory(outputStyle.Render(errStyle.Render("error: " + msg.res.Result)))
			m.busy = false
			m.status = ""
			m.todos = nil
		}
		var cmd tea.Cmd
		if m.streamCh != nil {
			cmd = nextStreamCmd(m.streamCh)
		}
		m.refreshPathMatches()
		return m, cmd

	case assistantTextMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		wasIdle := !m.busy
		m.busy = true
		if m.quietMode {
			m.turnBuffer = append(m.turnBuffer, msg.text)
		} else {
			m.appendResponse(msg.text)
		}
		var cmds []tea.Cmd
		if m.streamCh != nil {
			cmds = append(cmds, nextStreamCmd(m.streamCh))
		}
		if wasIdle {
			cmds = append(cmds, m.spinner.Tick)
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)

	case turnCompleteMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		m.flushTurnBuffer()
		m.busy = false
		m.status = ""
		m.todos = nil
		m.dismissCancelTurnConfirmIfIdle()
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
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

	case askToolRequestMsg:
		debugLog("askToolRequestMsg questions=%d", len(msg.questions))
		if m.mode == modeAskQuestion {
			msg.reply <- askReply{cancelled: true}
			return m, nil
		}
		m = m.startAsk(msg.questions)
		m.askReply = msg.reply
		return m, nil

	case approvalRequestMsg:
		debugLog("approvalRequestMsg tool=%s id=%s", msg.toolName, msg.toolUseID)
		if m.mode == modeApproval || m.mode == modeAskQuestion {
			msg.reply <- approvalReply{allow: false}
			return m, nil
		}
		m = m.startApproval(msg)
		return m, nil

	case imagePastedMsg:
		debugLog("imagePastedMsg bytes=%d mime=%q pngBytes=%d w=%d h=%d err=%v",
			len(msg.data), msg.mime, len(msg.pngForKitty), msg.width, msg.height, msg.err)
		if msg.err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render("paste: " + msg.err.Error())))
			return m, nil
		}
		att := pendingAttachment{data: msg.data, mime: msg.mime}
		if isKitty() && msg.pngForKitty != nil {
			m.nextImageID++
			if m.nextImageID == 0 {
				m.nextImageID = 1
			}
			if err := kittyTransmitPNG(m.nextImageID, msg.pngForKitty); err != nil {
				debugLog("kitty transmit err: %v", err)
			} else {
				att.imageID = m.nextImageID
				att.thumbCols, att.thumbRows = thumbnailGrid(msg.width, msg.height)
			}
		}
		m.pending = append(m.pending, att)
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
			return m, cmd
		}
		return m, nil

	case tea.KeyPressMsg:
		switch m.mode {
		case modeSessionPicker:
			return m.updatePicker(msg)
		case modeAskQuestion:
			return m.updateAsk(msg)
		case modeApproval:
			return m.updateApproval(msg)
		case modeConfig:
			return m.updateConfigModal(msg)
		default:
			return m.updateInput(msg)
		}

	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
}

func (m model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, tea.Quit
	}
	if m.cancelTurnConfirming {
		return m.updateCancelTurnConfirm(msg)
	}
	isCtrlC := msg.Mod == tea.ModCtrl && msg.Code == 'c'
	if !isCtrlC {
		m.exitArmed = false
	}
	if isCtrlC {
		if m.busy {
			m.cancelTurnConfirming = true
			m.cancelTurnChoice = 0
			return m, nil
		}
		if m.input.Value() == "" && len(m.pending) == 0 {
			if m.exitArmed {
				return m, tea.Quit
			}
			m.exitArmed = true
			m.appendHistory(outputStyle.Render(dimStyle.Render("Press ctrl+c again to exit")))
			return m, nil
		}
		m.input.Reset()
		m.pending = nil
		m.resetHistoryNav()
		m.refreshPathMatches()
		return m, nil
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'v' {
		return m, pasteImageCmd()
	}
	if msg.Mod == 0 && msg.Code == tea.KeyEsc {
		if m.busy {
			m.cancelTurnConfirming = true
			m.cancelTurnChoice = 0
			return m, nil
		}
		if len(m.pending) > 0 {
			m.pending = nil
			return m, nil
		}
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
			if m.historyIdx >= 0 || m.input.Line() == 0 {
				if m.historyPrev() {
					return m, nil
				}
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
			if m.historyIdx >= 0 {
				m.historyNext()
				return m, nil
			}
		case tea.KeyTab:
			if pickOpen {
				pick := m.pathMatches[m.pathIdx]
				m.input.SetValue(m.pathPickerCmd() + " " + pick + "/")
				m.resetHistoryNav()
				m.refreshPathMatches()
				return m, nil
			}
			if menuOpen {
				pick := items[m.menuIdx].name
				m.input.SetValue(pick)
				m.menuIdx = 0
				m.resetHistoryNav()
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
			debugLog("Enter line=%q valLen=%d busy=%v pending=%d pathCmd=%q bare=%q",
				line, len(val), m.busy, len(m.pending), m.pathPickerCmd(), bareCommand(line))
			if line == "" && len(m.pending) == 0 {
				return m, nil
			}
			if m.busy && (strings.HasPrefix(line, "/") || m.pathPickerCmd() != "" || bareCommand(line) != "") {
				return m, nil
			}
			m.recordInputHistory(val)
			if cmd := m.pathPickerCmd(); cmd != "" {
				target := strings.TrimSpace(m.pathQuery())
				if len(m.pathMatches) > 0 {
					target = m.pathMatches[m.pathIdx]
				}
				m.input.Reset()
				m.refreshPathMatches()
				return m.runPathCommand(cmd, target)
			}
			if cmd := bareCommand(line); cmd != "" {
				m.input.Reset()
				return m.runPathCommand(cmd, "")
			}
			m.input.Reset()
			m.menuIdx = 0
			if strings.HasPrefix(line, "/") {
				return m.handleCommand(line)
			}
			return m.sendToClaude(val)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.historyIdx >= 0 && m.input.Value() != m.inputHistory[m.historyIdx] {
		m.resetHistoryNav()
	}
	if items := m.filterSlashCmds(); m.menuIdx >= len(items) {
		m.menuIdx = 0
	}
	if !m.busy {
		m.refreshPathMatches()
	} else {
		m.pathMatches = nil
		m.pathIdx = 0
	}
	return m, cmd
}

func (m model) updateCancelTurnConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c':
		return m.applyCancelTurnConfirm()
	case msg.Code == tea.KeyEsc, msg.Code == 'n' && msg.Mod == 0:
		m.cancelTurnConfirming = false
		m.cancelTurnChoice = 0
		return m, nil
	case msg.Code == 'y' && msg.Mod == 0:
		return m.applyCancelTurnConfirm()
	case msg.Code == tea.KeyLeft, msg.Code == 'h' && msg.Mod == 0:
		m.cancelTurnChoice = 0
		return m, nil
	case msg.Code == tea.KeyRight, msg.Code == 'l' && msg.Mod == 0:
		m.cancelTurnChoice = 1
		return m, nil
	case msg.Code == tea.KeyTab:
		m.cancelTurnChoice = 1 - m.cancelTurnChoice
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.cancelTurnChoice == 1 {
			return m.applyCancelTurnConfirm()
		}
		m.cancelTurnConfirming = false
		m.cancelTurnChoice = 0
		return m, nil
	}
	return m, nil
}

func (m model) applyCancelTurnConfirm() (tea.Model, tea.Cmd) {
	m.cancelTurnConfirming = false
	m.cancelTurnChoice = 0
	return m.cancelTurn(), nil
}

func (m *model) dismissCancelTurnConfirmIfIdle() {
	if !m.busy {
		m.cancelTurnConfirming = false
		m.cancelTurnChoice = 0
	}
}

func (m model) updatePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, tea.Quit
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		m.mode = modeInput
		return m, nil
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
	case "/model":
		m = m.startModelPicker()
		return m, nil
	case "/config":
		m = m.startConfigModal()
		return m, nil
	}
	bare := strings.TrimPrefix(cmd, "/")
	for _, e := range m.claudeSlashCmds {
		if e.Name == bare {
			return m.sendToClaude(line)
		}
	}
	m.appendHistory(outputStyle.Render(errStyle.Render("unknown command: " + cmd)))
	return m, nil
}

func persistSlashCmdsCmd(cmds []claudeSlashEntry) tea.Cmd {
	return func() tea.Msg {
		cfg, _ := loadConfig()
		cfg.Claude.SlashCommands = cmds
		if err := saveConfig(cfg); err != nil {
			debugLog("saveConfig err: %v", err)
		}
		return nil
	}
}

func (m *model) recordInputHistory(val string) {
	m.resetHistoryNav()
	if val == "" {
		return
	}
	if n := len(m.inputHistory); n > 0 && m.inputHistory[n-1] == val {
		return
	}
	m.inputHistory = append(m.inputHistory, val)
}

func (m *model) resetHistoryNav() {
	m.historyIdx = -1
}

func (m *model) historyPrev() bool {
	if len(m.inputHistory) == 0 {
		return false
	}
	if m.historyIdx == -1 {
		m.historyIdx = len(m.inputHistory) - 1
	} else if m.historyIdx > 0 {
		m.historyIdx--
	} else {
		return true
	}
	m.input.SetValue(m.inputHistory[m.historyIdx])
	m.input.CursorEnd()
	return true
}

func (m *model) historyNext() {
	if m.historyIdx == -1 {
		return
	}
	m.historyIdx++
	if m.historyIdx >= len(m.inputHistory) {
		m.resetHistoryNav()
		m.input.Reset()
	} else {
		m.input.SetValue(m.inputHistory[m.historyIdx])
		m.input.CursorEnd()
	}
}

func (m model) filterSlashCmds() []slashCmd {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") {
		return nil
	}
	seen := make(map[string]bool, len(builtinSlashCmds))
	var out []slashCmd
	for _, c := range builtinSlashCmds {
		seen[c.name] = true
		if strings.HasPrefix(c.name, val) {
			out = append(out, c)
		}
	}
	for _, e := range m.claudeSlashCmds {
		full := "/" + e.Name
		if seen[full] {
			continue
		}
		if strings.HasPrefix(full, val) {
			out = append(out, slashCmd{name: full, desc: e.Description})
		}
	}
	return out
}
