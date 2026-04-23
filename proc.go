package main

import (
	"os"

	tea "charm.land/bubbletea/v2"
)

// sessionArgs bundles the model's current state into the provider-shaped
// arg struct used by Provider.StartSession / ProbeInit.
func (m model) sessionArgs() ProviderSessionArgs {
	return ProviderSessionArgs{
		Cwd:                m.cwd,
		MCPPort:            m.mcpPort,
		Model:              m.providerModel,
		Effort:             m.providerEffort,
		OllamaHost:         m.ollamaHost,
		OllamaModel:        m.ollamaModel,
		SkipAllPermissions: m.skipAllPermissions,
		Worktree:           m.worktree,
		SessionID:          m.sessionID,
		ResumeCwd:          m.resumeCwd,
	}
}

// ensureProc lazily starts a provider session on first send in a turn.
// Subsequent calls are no-ops while the process is alive.
//
// Worktree lifecycle is owned entirely by ask (no provider opts out):
// when the user's worktree preference is on and we're inside a git
// checkout, a `.claude/worktrees/ask-<provider>-<id>` sibling is
// created before fork and `args.Cwd` points at it. Subsequent calls
// (after a provider swap or a proc exit) reuse `m.worktreeName`
// verbatim so the same directory serves every backend in the tab —
// the provider tag in the name is informational only, not a filter.
// Resume paths populate `m.worktreeName` from `m.resumeCwd` if the
// prior session lived inside a worktree.
func (m *model) ensureProc() error {
	if m.proc != nil {
		return nil
	}
	args := m.sessionArgs()
	// Resume paths derive the worktree name from where the prior
	// session lived. Do this first so the step below can treat a
	// resumed worktree name the same as any other pre-set value.
	if m.sessionID != "" && m.resumeCwd != "" && m.worktreeName == "" {
		m.worktreeName = worktreeNameFromCwd(m.resumeCwd)
	}

	// Fresh session + worktree preference on: create a new worktree.
	if m.worktreeName == "" && m.sessionID == "" && m.worktree && inGitCheckout() {
		path, name, err := createWorktree()
		if err != nil {
			return err
		}
		args.Cwd = path
		m.worktreeName = name
	}

	// With a worktree name in hand (freshly created, reused on swap,
	// or resume-derived), point the provider at its directory and
	// recreate it from its branch if prune wiped it between sessions.
	// This runs even when m.worktree is currently off so a resumed
	// session lands in the same branch it was created on.
	if m.worktreeName != "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		args.Cwd = worktreePath(cwd, m.worktreeName)
		if err := ensureResumeWorktree(args.Cwd); err != nil {
			return err
		}
	}
	proc, ch, err := m.provider.StartSession(args)
	if err != nil {
		return err
	}
	m.proc = proc
	m.streamCh = ch
	return nil
}

// sendToProvider delivers a user turn (text + pending attachments) to
// the active provider session, starting a fresh subprocess if needed.
// Returns the right tea.Cmd composition for spinner ticks and stream
// readers.
func (m model) sendToProvider(line string) (tea.Model, tea.Cmd) {
	nAtt := len(m.pending)
	debugLog("sendToProvider provider=%s line=%q attachments=%d procNil=%v busy=%v sessionID=%q",
		m.provider.ID(), line, nAtt, m.proc == nil, m.busy, m.sessionID)
	m.appendUser(userBarText(line, nAtt))
	newProc := m.proc == nil
	wasIdle := !m.busy
	if err := m.ensureProc(); err != nil {
		debugLog("ensureProc err: %v", err)
		m.appendHistory(outputStyle.Render(errStyle.Render("could not start " + m.provider.DisplayName() + ": " + err.Error())))
		return m, nil
	}
	if err := m.provider.Send(m.proc, line, m.pending); err != nil {
		debugLog("provider send err: %v", err)
		m.appendHistory(outputStyle.Render(errStyle.Render("write to " + m.provider.DisplayName() + " failed: " + err.Error())))
		m.killProc()
		return m, nil
	}
	m.pending = nil
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

// cancelTurn kills the active provider process and reports to the user.
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
	last := m.turnBuffer[len(m.turnBuffer)-1]
	m.turnBuffer = nil
	m.appendResponse(last)
}

// killProc tears down the active provider subprocess (if any) and
// resets all per-turn UI state. Safe to call on an idle model.
func (m *model) killProc() {
	if m.proc == nil {
		return
	}
	m.proc.kill()
	m.proc = nil
	m.streamCh = nil
	m.busy = false
	m.status = ""
	m.todos = nil
	m.bgTasks = nil
}

// drainPendingReplies unblocks any MCP tool call that was waiting on
// this tab (ask/approval modal). Called when a tab is closed with a
// modal still open so the provider's request side doesn't hang on a
// dangling channel.
func (m *model) drainPendingReplies() {
	if m.askReply != nil {
		select {
		case m.askReply <- askReply{cancelled: true}:
		default:
		}
		m.askReply = nil
	}
	if m.approvalReply != nil {
		select {
		case m.approvalReply <- approvalReply{allow: false}:
		default:
		}
		m.approvalReply = nil
	}
}
