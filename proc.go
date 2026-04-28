package main

import (
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

// cancelFallbackTimeout bounds how long cancelTurn will wait for a
// cooperative cancel to take effect before falling back to killProc.
// Codex usually wraps up in well under a second after turn/interrupt;
// this ceiling exists purely to prevent the UI getting stuck if the
// provider's stream never emits turn/completed.
const cancelFallbackTimeout = 10 * time.Second

// sessionArgs bundles the model's current state into the provider-shaped
// arg struct used by Provider.StartSession / ProbeInit.
//
// SessionID and NewSessionID are mutually exclusive: a freshly minted
// id (sessionMinted=true) routes to NewSessionID so claudeCLIArgs picks
// the --session-id branch; otherwise the id is treated as a resume
// target.
func (m model) sessionArgs() ProviderSessionArgs {
	args := ProviderSessionArgs{
		Cwd:                m.cwd,
		MCPPort:            m.mcpPort,
		TabID:              m.id,
		Model:              m.providerModel,
		Effort:             m.providerEffort,
		OllamaHost:         m.ollamaHost,
		OllamaModel:        m.ollamaModel,
		SkipAllPermissions: m.skipAllPermissions,
		Worktree:           m.worktree,
		ResumeCwd:          m.resumeCwd,
		PluginDir:          usagePluginDir,
		AddedDirs:          append([]string(nil), m.addedDirs...),
	}
	if m.sessionMinted {
		args.NewSessionID = m.sessionID
	} else {
		args.SessionID = m.sessionID
	}
	return args
}

// ensureProc lazily starts a provider session on first send in a turn.
// Subsequent calls are no-ops while the process is alive.
//
// Worktree lifecycle is owned entirely by ask (no provider opts out):
// when the user's worktree preference is on and we're inside a repo
// root, a `.claude/worktrees/<name>` sibling is created before fork
// and `args.Cwd` points at it. Git repos use `git worktree`; repos
// with a top-level `.jj` use `jj workspace` instead. Subsequent calls
// (after a provider swap or a proc exit) reuse `m.worktreeName`
// verbatim so the same directory serves every backend in the tab.
// Resume paths populate `m.worktreeName` from `m.resumeCwd` if the
// prior session lived inside a worktree.
func (m *model) ensureProc() error {
	if m.proc != nil {
		return nil
	}
	rootCwd, err := os.Getwd()
	if err != nil {
		return err
	}
	m.preMintNativeSessionIfNeeded()
	args := m.sessionArgs()
	args, m.worktreeName, err = prepareProviderSessionAt(args, m.worktreeName, rootCwd)
	if err != nil {
		return err
	}
	proc, ch, err := m.provider.StartSession(args)
	if err != nil {
		return err
	}
	m.proc = proc
	m.streamCh = ch
	m.sessionMinted = false
	return nil
}

// preMintNativeSessionIfNeeded asks the current provider for a fresh
// caller-chosen session id, sets m.sessionID + m.sessionMinted, and
// records the virtual-session row up front. No-op when m.sessionID is
// already populated (resume or post-first-turn) or when the provider
// returns "" (codex, fakes that don't pre-mint). Caller is responsible
// for clearing m.sessionMinted once the fork has dispatched so a later
// kill+retry takes the --resume branch.
func (m *model) preMintNativeSessionIfNeeded() {
	if m.sessionID != "" || m.provider == nil {
		return
	}
	id := m.provider.PreMintSessionID(m.sessionArgs())
	if id == "" {
		return
	}
	m.sessionID = id
	m.sessionMinted = true
	m.recordVirtualSession(id)
}

func prepareProviderSession(args ProviderSessionArgs, worktreeName string) (ProviderSessionArgs, string, error) {
	rootCwd := args.Cwd
	if rootCwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return args, worktreeName, err
		}
		rootCwd = cwd
	}
	return prepareProviderSessionAt(args, worktreeName, rootCwd)
}

func prepareProviderSessionAt(args ProviderSessionArgs, worktreeName, rootCwd string) (ProviderSessionArgs, string, error) {
	if rootCwd == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return args, worktreeName, err
		}
		rootCwd = cwd
	}
	if args.Cwd == "" {
		args.Cwd = rootCwd
	}
	// Resume paths derive the worktree name from where the prior
	// session lived. Do this first so the step below can treat a
	// resumed worktree name the same as any other pre-set value.
	if args.SessionID != "" && args.ResumeCwd != "" && worktreeName == "" {
		worktreeName = worktreeNameFromCwd(args.ResumeCwd)
	}

	// Fresh session + worktree preference on: create a new worktree/workspace.
	if worktreeName == "" && args.SessionID == "" && args.Worktree &&
		worktreeBackendAt(rootCwd) != workspaceBackendNone {
		path, name, err := createWorktreeAt(rootCwd)
		if err != nil {
			return args, worktreeName, err
		}
		args.Cwd = path
		worktreeName = name
	}

	// With a worktree name in hand (freshly created, reused on swap,
	// or resume-derived), point the provider at its directory and
	// recreate it if prune wiped it between sessions.
	// This runs even when worktree is currently off so a resumed
	// session lands in the same isolated workspace it was created in.
	if worktreeName != "" {
		args.Cwd = worktreePath(rootCwd, worktreeName)
		if err := ensureResumeWorktree(args.Cwd); err != nil {
			return args, worktreeName, err
		}
	}
	// Last-line guard: if worktree mode is on, args.Cwd must live
	// inside .claude/worktrees/ — never the project root. Catches
	// future regressions where a refactor lets worktree-mode sessions
	// reach this point with the bare project root cwd.
	if err := validateExecutorCwd(args, rootCwd); err != nil {
		return args, worktreeName, err
	}
	return args, worktreeName, nil
}

// sendToProvider delivers a user turn (text + pending attachments) to
// the active provider session, starting a fresh subprocess if needed.
// Returns the right tea.Cmd composition for spinner ticks and stream
// readers.
func (m model) sendToProvider(line string) (tea.Model, tea.Cmd) {
	nAtt := len(m.pending)
	debugLog("sendToProvider provider=%s line=%q attachments=%d procNil=%v busy=%v sessionID=%q",
		m.provider.ID(), line, nAtt, m.proc == nil, m.busy, m.sessionID)
	(&m).clearSelection()
	m.appendUser(userBarText(line, nAtt))
	if invalid := validateAskCwd(m.cwd); invalid.Msg != "" {
		m.pending = nil
		m.appendHistory(outputStyle.Render(errStyle.Render(invalid.Msg)))
		return m, nil
	}
	turn := providerQueuedTurn{
		text:        line,
		attachments: append([]pendingAttachment(nil), m.pending...),
	}
	wasIdle := !m.busy

	if m.procStarting && m.proc == nil {
		m.queuedTurns = append(m.queuedTurns, turn)
		m.pending = nil
		m.busy = true
		if m.status == "" {
			m.status = "starting " + m.provider.DisplayName() + "..."
		}
		return m, nil
	}

	if m.proc == nil {
		m.procStartSeq++
		seq := m.procStartSeq
		m.procStarting = true
		m.pending = nil
		m.busy = true
		m.status = "starting " + m.provider.DisplayName() + "..."
		(&m).preMintNativeSessionIfNeeded()
		cmd := startAndSendProviderCmd(m.provider, m.sessionArgs(), m.worktreeName, turn, seq)
		// The dispatched closure has captured args (with NewSessionID
		// when minted); flip the flag back so any later fork in this
		// conversation — kill+retry, provider-swap-back, etc. — runs
		// down the --resume branch since claude has already written
		// the jsonl under that id.
		m.sessionMinted = false
		if wasIdle {
			return m, tea.Batch(cmd, m.spinner.Tick)
		}
		return m, cmd
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
	if wasIdle {
		cmds = append(cmds, m.spinner.Tick)
	}
	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}

func startAndSendProviderCmd(p Provider, args ProviderSessionArgs, worktreeName string, turn providerQueuedTurn, seq uint64) tea.Cmd {
	tabID := args.TabID
	providerID := p.ID()
	displayName := p.DisplayName()
	return func() tea.Msg {
		args, worktreeName, err := prepareProviderSession(args, worktreeName)
		if err != nil {
			return providerStartDoneMsg{
				tabID:      tabID,
				seq:        seq,
				providerID: providerID,
				err:        fmt.Errorf("could not start %s: %w", displayName, err),
				turn:       turn,
			}
		}
		proc, ch, err := p.StartSession(args)
		if err != nil {
			return providerStartDoneMsg{
				tabID:      tabID,
				seq:        seq,
				providerID: providerID,
				err:        fmt.Errorf("could not start %s: %w", displayName, err),
				turn:       turn,
			}
		}
		if err := p.Send(proc, turn.text, turn.attachments); err != nil {
			proc.kill()
			return providerStartDoneMsg{
				tabID:      tabID,
				seq:        seq,
				providerID: providerID,
				err:        fmt.Errorf("write to %s failed: %w", displayName, err),
				turn:       turn,
			}
		}
		return providerStartDoneMsg{
			tabID:        tabID,
			seq:          seq,
			providerID:   providerID,
			proc:         proc,
			streamCh:     ch,
			worktreeName: worktreeName,
			turn:         turn,
		}
	}
}

func (m model) handleProviderStartDone(msg providerStartDoneMsg) (tea.Model, tea.Cmd) {
	if msg.tabID != m.id {
		return m, nil
	}
	if !m.procStarting || msg.seq != m.procStartSeq ||
		m.provider == nil || msg.providerID != m.provider.ID() {
		if msg.proc != nil {
			msg.proc.kill()
		}
		return m, nil
	}

	m.procStarting = false
	if msg.err != nil {
		debugLog("provider start/send err: %v", msg.err)
		m.busy = false
		m.status = ""
		m.todos = nil
		m.queuedTurns = nil
		if len(msg.turn.attachments) > 0 && len(m.pending) == 0 {
			m.pending = append([]pendingAttachment(nil), msg.turn.attachments...)
		}
		m.appendHistory(outputStyle.Render(errStyle.Render(msg.err.Error())))
		return m, nil
	}

	m.proc = msg.proc
	m.streamCh = msg.streamCh
	m.worktreeName = msg.worktreeName
	m.busy = true
	m.status = "thinking…"

	queued := m.queuedTurns
	m.queuedTurns = nil
	for _, turn := range queued {
		if err := m.provider.Send(m.proc, turn.text, turn.attachments); err != nil {
			debugLog("provider queued send err: %v", err)
			m.appendHistory(outputStyle.Render(errStyle.Render("write to " + m.provider.DisplayName() + " failed: " + err.Error())))
			m.killProc()
			return m, nil
		}
	}
	if m.streamCh != nil {
		return m, nextStreamCmd(m.streamCh)
	}
	return m, nil
}

// cancelTurn asks the provider to cancel cooperatively (turn/interrupt
// for codex). Providers that don't support that — claude — return
// handled=false and we fall back to killing the subprocess, matching
// the old behavior. On a cooperative cancel we keep the proc alive so
// the thread stays resumable; the UI goes idle when the provider
// emits its own turn-completed notification.
//
// Returns a tea.Cmd because the cooperative path arms a fallback
// timer — if the turn hasn't wound down by then, cancelWatchdogMsg
// triggers a killProc so the UI never sticks in "cancelling…".
func (m model) cancelTurn() (model, tea.Cmd) {
	if !m.busy && m.proc == nil {
		return m, nil
	}
	m.flushTurnBuffer()
	if m.provider != nil && m.proc != nil {
		handled, err := m.provider.Interrupt(m.proc)
		if err != nil {
			debugLog("provider.Interrupt err: %v", err)
		}
		if handled && err == nil {
			m.status = "cancelling…"
			m.appendHistory(outputStyle.Render(dimStyle.Render("✗ cancelling…")))
			return m, cancelWatchdogCmd(m.proc)
		}
	}
	m.killProc()
	m.appendHistory(outputStyle.Render(dimStyle.Render("✗ cancelled")))
	return m, nil
}

// cancelWatchdogCmd fires a cancelWatchdogMsg after cancelFallbackTimeout
// tagged with the proc that was live at cancel time. The Update
// handler drops the message if the proc is gone (already reaped) or
// no longer busy (the cooperative cancel landed normally).
func cancelWatchdogCmd(p *providerProc) tea.Cmd {
	return tea.Tick(cancelFallbackTimeout, func(time.Time) tea.Msg {
		return cancelWatchdogMsg{proc: p}
	})
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
	if m.procStarting {
		m.procStarting = false
		m.procStartSeq++
		m.queuedTurns = nil
	}
	// Once we kill, any minted-and-not-yet-claimed id is either already
	// claimed on disk (claude wrote turn-zero metadata) or was never
	// claimed (spawn failed). Either way, future forks should take the
	// --resume branch — claude's --session-id refuses an id that maps
	// to an existing file.
	m.sessionMinted = false
	if m.proc == nil {
		m.busy = false
		m.status = ""
		m.todos = nil
		m.bgTasks = nil
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
