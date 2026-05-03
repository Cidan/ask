package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

func (m model) Init() tea.Cmd {
	debugLog("Init provider=%s mcpPort=%d", m.provider.ID(), m.mcpPort)
	// Skip ProbeInit when ask's cwd isn't a valid project root: it
	// would fork claude/codex inside a subdir or worktree to discover
	// slash commands. Startup itself stays silent — the user only
	// sees the chat-facing error when they type their first command.
	if invalid := validateAskCwd(m.cwd); invalid.Msg != "" {
		return cursor.Blink
	}
	cmds := []tea.Cmd{m.provider.ProbeInit(m.sessionArgs()), cursor.Blink}
	// `ask resume <vid>` pre-seeds m.virtualSessionID before the program
	// runs. Once Init fires we kick off the same resume the picker uses;
	// gating on empty m.sessionID keeps later Inits (Ctrl+T new tabs that
	// inherit a fresh state) from re-replaying.
	if m.virtualSessionID != "" && m.sessionID == "" {
		cmds = append(cmds, startupResumeCmd(m.id, m.virtualSessionID))
	}
	return tea.Batch(cmds...)
}

// startupResumeCmd returns a tea.Cmd that emits a startupResumeMsg
// targeted at tabID. Used by Init when the CLI seeded a vsID so the
// resume runs on the bubbletea event loop — model mutations inside
// resumeVirtualSession (history reset, busy flag, etc.) only land
// reliably when applied through Update.
func startupResumeCmd(tabID int, vsID string) tea.Cmd {
	return func() tea.Msg {
		return startupResumeMsg{tabID: tabID, vsID: vsID}
	}
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
	case issueLoadingTickMsg:
		// High-fps loader animation. Drop ticks aimed at a different
		// tab so a background tab's loader can't burn cycles on the
		// foreground. When loading flips false (first chunk arrival
		// or error dismissal) we stop scheduling further ticks; the
		// in-flight one is silently absorbed here.
		if msg.tabID != m.id {
			return m, nil
		}
		if m.issues == nil || !m.issues.loading {
			return m, nil
		}
		m.issues.loadingFrame++
		return m, issueLoadingTickCmd(m.id)

	case issuePageLoadedMsg:
		if msg.tabID != m.id {
			return m, nil
		}
		if m.issues == nil {
			m.issues = newIssuesState()
		}
		// Stale-gen drop: the user changed the query while this fetch
		// was in flight. Silently discard so the new query's data
		// can't be polluted by the old one.
		if msg.gen != m.issues.queryGen {
			return m, nil
		}
		s := m.issues
		// requestedCursor=="" is the first-chunk sentinel; reading
		// it (not msg.page) means error paths still clear loading.
		isFirstChunk := msg.requestedCursor == ""
		if isFirstChunk {
			s.loading = false
		}
		if msg.err != nil {
			// First-chunk error shows the modal; subsequent-chunk
			// errors are best-effort and just clear the fetching
			// marker so a retry can happen on the next scroll trigger
			// (or via Ctrl+R for a clean restart).
			if isFirstChunk {
				s.loadErr = msg.err
			}
			// Kanban tracks per-column fetching flags so a retry
			// can fire on the next scroll trigger.
			if kv, ok := s.view.(*kanbanIssueView); ok {
				if idx := kv.columnByQueryFingerprint(s, s.queryFingerprint(msg.query)); idx >= 0 {
					kv.columns[idx].fetching = false
				}
			}
			return m, nil
		}
		s.loadErr = nil
		issues := append([]issue(nil), msg.page.Issues...)
		s.applySort(issues)
		chunk := issuePageChunk{
			cursor:     msg.requestedCursor,
			nextCursor: msg.page.NextCursor,
			hasMore:    msg.page.HasMore,
			issues:     issues,
		}
		s.appendChunk(msg.query, chunk)
		// Kanban owns col.loaded / col.nextCursor / col.fetching as
		// per-column local state. The cache append above keeps a
		// view rebuild (Ctrl+R, search-box close) able to re-stitch
		// columns from the cache without re-fetching.
		if kv, ok := s.view.(*kanbanIssueView); ok {
			fp := s.queryFingerprint(msg.query)
			idx := kv.columnByQueryFingerprint(s, fp)
			if idx >= 0 {
				col := &kv.columns[idx]
				col.loaded = append(col.loaded, issues...)
				col.nextCursor = msg.page.NextCursor
				col.hasMore = msg.page.HasMore
				col.fetching = false
			}
		}
		return m, nil

	case issueMoveDoneMsg:
		if msg.tabID != m.id || m.issues == nil {
			return m, nil
		}
		if msg.err == nil {
			return m, nil
		}
		s := m.issues
		kv, ok := s.view.(*kanbanIssueView)
		if !ok {
			return m, m.toast.show("issues: move failed: " + msg.err.Error())
		}
		// Defensive rollback: locate the issue by number rather than
		// blindly indexing the target column. A reload between drop
		// and rollback wipes the cache + columns; in that case the
		// issue is no longer at the head of the target slice (or is
		// missing entirely) and we silently drop the rollback rather
		// than corrupting freshly-fetched state.
		if msg.targetColIdx >= 0 && msg.targetColIdx < len(kv.columns) {
			tcol := &kv.columns[msg.targetColIdx]
			for i := range tcol.loaded {
				if tcol.loaded[i].number == msg.issueNumber {
					tcol.loaded = append(tcol.loaded[:i], tcol.loaded[i+1:]...)
					break
				}
			}
			s.removeIssueFromCache(tcol.spec.Query, msg.issueNumber)
		}
		if msg.originColIdx >= 0 && msg.originColIdx < len(kv.columns) {
			ocol := &kv.columns[msg.originColIdx]
			alreadyPresent := false
			for _, it := range ocol.loaded {
				if it.number == msg.issueNumber {
					alreadyPresent = true
					break
				}
			}
			if !alreadyPresent {
				row := msg.originRowIdx
				if row < 0 {
					row = 0
				}
				if row > len(ocol.loaded) {
					row = len(ocol.loaded)
				}
				ocol.loaded = append(ocol.loaded[:row],
					append([]issue{msg.originSnap}, ocol.loaded[row:]...)...)
				s.insertIssueIntoCache(ocol.spec.Query, msg.originSnap, row)
			}
		}
		kv.clampSelection()
		return m, m.toast.show("issues: move failed: " + msg.err.Error())

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(msg.Width - 5)
		// We don't pre-walk m.history clearing rendered fields any
		// more — the chatView re-wraps lazily as entries enter the
		// visible window, so a 5000-entry resize stays fast even
		// before the first scroll. The wrap cache invalidates via
		// wrappedFor != width inside ensureEntryWrapped, which also
		// owns rebuilding m.renderer at the new width.
		m.lastContentFP = ""
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

	case providerCwdMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		name := worktreeNameFromCwd(msg.cwd)
		if name != "" && name != m.worktreeName {
			lockWorktree(name)
		}
		m.worktreeName = name
		m.lastContentFP = ""
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case providerModelMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		m.modelForContext = msg.model
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case usageMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		m.lastUsageTokens = msg.tokens
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case codexUsageMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		m.codexUsage.primary = msg.primary
		m.codexUsage.secondary = msg.secondary
		m.codexUsage.hasRateLimits = true
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case codexContextMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		m.codexUsage.contextTokens = msg.tokens
		m.codexUsage.modelContextWindow = msg.window
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case bgTaskStartedMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		if m.bgTasks == nil {
			m.bgTasks = map[string]string{}
		}
		m.bgTasks[msg.taskID] = msg.toolUseID
		debugLog("bgTaskStartedMsg tab=%d task_id=%s tool_use_id=%s bgTasks=%d",
			m.id, msg.taskID, msg.toolUseID, len(m.bgTasks))
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case bgTaskEndedMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		delete(m.bgTasks, msg.taskID)
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case hookSubagentStartMsg:
		// Observability only: SubagentStart fires for every Task-spawned
		// sub-agent, including foreground ones, so we can't use it as a
		// bgTasks increment without over-counting. The debug log is
		// useful for verifying whether agent_id correlates with the
		// task_id we get from the stream's task_started event.
		debugLog("hookSubagentStartMsg tab=%d agent_id=%s agent_type=%s bgTasks=%d",
			msg.tabID, msg.agentID, msg.agentType, len(m.bgTasks))
		return m, nil

	case hookSubagentStopMsg:
		// Authoritative cleanup. agent_id may be the task_id (direct
		// hit on the bgTasks key) or the spawning Task call's
		// tool_use_id (claude's CLI uses different identifier
		// namespaces depending on context). Try both before giving up.
		// Foreground sub-agents and unknown ids are no-ops.
		reaped := reapBgTaskByAgentID(m.bgTasks, msg.agentID)
		switch reaped {
		case "":
			debugLog("hookSubagentStopMsg tab=%d agent_id=%s agent_type=%s no match in bgTasks=%d",
				msg.tabID, msg.agentID, msg.agentType, len(m.bgTasks))
		default:
			debugLog("hookSubagentStopMsg reaping stuck bgTask tab=%d agent_id=%s task_id=%s bgTasks=%d→%d",
				msg.tabID, msg.agentID, reaped, len(m.bgTasks)+1, len(m.bgTasks))
		}
		return m, nil

	case toolDiffMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		if m.renderDiffs && !m.quietMode {
			m.appendHistory(renderDiffBlock(msg.filePath, msg.hunks))
		}
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case toolCallMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		// Memory capture: accumulate tool name + file path so the
		// turn-end flush can write per-file observations. Always
		// runs (no service-open gate) — memoryWrite is the no-op
		// guard at the bottom of the call.
		(&m).recordToolCall(msg.name, msg.input)
		if m.shouldRenderToolCall(msg) {
			m.appendHistory(renderToolCallBlock(msg.name, msg.input, m.toolOutputMode))
		}
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case toolResultMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		if m.shouldRenderToolResult(msg) {
			m.appendHistory(renderToolResultBlock(msg.output, msg.isError))
		}
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case providerInitLoadedMsg:
		if msg.tabID != m.id {
			return m, nil
		}
		if msg.err != nil {
			debugLog("providerInitLoadedMsg err: %v", msg.err)
			return m, nil
		}
		debugLog("providerInitLoadedMsg slashCmds=%d", len(msg.slashCmds))
		m.providerSlashCmds = msg.slashCmds
		return m, persistSlashCmdsCmd(m.provider, msg.slashCmds)

	case providerStartDoneMsg:
		return m.handleProviderStartDone(msg)

	case cancelWatchdogMsg:
		// Cooperative cancel never completed within the grace window.
		// If the same proc is still running and still busy, kill it
		// as a fallback so the UI doesn't sit in "cancelling…"
		// forever. If the proc has already exited (nil m.proc) or
		// the turn wound down normally (not busy), we silently drop.
		if msg.proc != m.proc || !m.busy || m.proc == nil {
			return m, nil
		}
		debugLog("cancel watchdog fired; force-killing proc")
		m.killProc()
		m.appendHistory(outputStyle.Render(dimStyle.Render(
			"✗ cancelled (force-killed after interrupt timed out)")))
		return m, nil

	case providerExitedMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		var stderrTail string
		if m.proc != nil && m.proc.stderr != nil {
			stderrTail = strings.TrimSpace(m.proc.stderr.String())
		}
		debugLog("providerExitedMsg err=%v stderrLen=%d", msg.err, len(stderrTail))
		m.flushTurnBuffer()
		m.busy = false
		m.status = ""
		m.todos = nil
		m.bgTasks = nil
		m.streamCh = nil
		m.proc = nil
		// Keep m.worktreeName across proc exits — the directory still
		// exists on disk and the next turn (or a provider swap) reuses
		// it. /new, /clear, and the worktree-off toggle clear it
		// explicitly; prune reaps it on shutdown.
		m.dismissCancelTurnConfirmIfIdle()
		if m.mode == modeApproval {
			// Unblock any codex approval responder still waiting on
			// the user so the goroutine can exit cleanly. The channel
			// is buffered, so a non-blocking send is safe.
			if m.approvalReply != nil {
				select {
				case m.approvalReply <- approvalReply{allow: false}:
				default:
				}
			}
			m = m.clearApproval()
		}
		if msg.err != nil || stderrTail != "" {
			out := m.provider.DisplayName() + " exited"
			if msg.err != nil {
				out += ": " + msg.err.Error()
			}
			if stderrTail != "" {
				out += "\n" + stderrTail
			}
			m.appendHistory(outputStyle.Render(errStyle.Render(out)))
		}
		// Workflow tabs: the proc dying on its own (without
		// turnCompleteMsg arriving first) is a fatal step error.
		// turnCompleteMsg's success path already kills the proc
		// itself, so by the time providerExitedMsg lands on a
		// healthy step the runner has already moved on; the guard
		// against `done`/`failed` ensures we don't double-finalise.
		if m.workflowRun != nil && !m.workflowRun.done && !m.workflowRun.failed {
			detail := stderrTail
			if msg.err != nil {
				detail = msg.err.Error()
			}
			return m, workflowAdvanceCmd(m.id, errStepError(detail))
		}
		return m, nil

	case providerDoneMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		debugLog("providerDoneMsg err=%v isError=%v resultLen=%d",
			msg.err, msg.res.IsError, len(msg.res.Result))
		m.dismissCancelTurnConfirmIfIdle()
		// Workflow tabs don't pin a virtual session — every step
		// is a fresh one-shot, so persisting native session ids
		// would just leak orphan VS rows.
		if msg.res.SessionID != "" && m.workflowRun == nil {
			m.sessionID = msg.res.SessionID
			m.recordVirtualSession(msg.res.SessionID)
		}
		// Claude just finished a turn, which means it hit the API and
		// wrote a fresh .usage-cache.json. Re-read so the 5h/wk chip
		// segments reflect the latest utilization. Errors are silent:
		// the previous snapshot (or nil) stays in place.
		if uc, err := readUsageCache(); err == nil {
			m.usageCache = uc
		}
		var workflowErr error
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
			workflowErr = msg.err
		case msg.res.IsError:
			m.appendHistory(outputStyle.Render(errStyle.Render("error: " + msg.res.Result)))
			m.busy = false
			m.status = ""
			m.todos = nil
			workflowErr = errStepError(msg.res.Result)
		}
		var cmd tea.Cmd
		if m.streamCh != nil {
			cmd = nextStreamCmd(m.streamCh)
		}
		m.refreshPathMatches()
		// Workflow error: hand off to the runner so the chain
		// finalises with `failed`. Success paths are routed by
		// turnCompleteMsg instead — providerDoneMsg fires for both
		// paths and we don't want to double-advance.
		if workflowErr != nil && m.workflowRun != nil && !m.workflowRun.done && !m.workflowRun.failed {
			advance := workflowAdvanceCmd(m.id, workflowErr)
			if cmd != nil {
				return m, tea.Batch(cmd, advance)
			}
			return m, advance
		}
		return m, cmd

	case assistantTextMsg:
		if msg.proc != m.proc {
			return m, nil
		}
		// Memory capture: feed the response builder for the per-turn
		// outcome line. No-op when no turn is in flight.
		(&m).recordAssistantText(msg.text)
		// Workflow capture: feed the per-step output buffer so
		// step N+1's prompt can carry "Previous step output:". No-op
		// off a workflow tab.
		(&m).workflowAssistantText(msg.text)
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
		// Memory capture: write the accumulated turn observations to
		// memmy. No-op when memory is disabled (memoryWrite is closed-
		// service-safe). flushMemoryTurn resets internal state, so a
		// subsequent stray turnCompleteMsg won't double-fire.
		(&m).flushMemoryTurn()
		m.busy = false
		m.status = ""
		m.todos = nil
		m.dismissCancelTurnConfirmIfIdle()
		// Workflow tabs: a clean turn completion is the signal to
		// advance to the next step. The runner kills the proc,
		// rolls the captured text into the step log, and either
		// fires the next step or finalises the run. The advance
		// cmd runs as a fresh tea.Cmd so any next-step proc spawn
		// happens at a clean Update boundary.
		var workflowCmd tea.Cmd
		if m.workflowRun != nil && !m.workflowRun.done && !m.workflowRun.failed {
			workflowCmd = workflowAdvanceCmd(m.id, nil)
		}
		var streamCmd tea.Cmd
		if m.streamCh != nil {
			streamCmd = nextStreamCmd(m.streamCh)
		}
		switch {
		case streamCmd != nil && workflowCmd != nil:
			return m, tea.Batch(streamCmd, workflowCmd)
		case streamCmd != nil:
			return m, streamCmd
		case workflowCmd != nil:
			return m, workflowCmd
		}
		return m, nil

	case historyLoadedMsg:
		if msg.tabID != m.id {
			return m, nil
		}
		// Cross-provider translation loads a source provider's native
		// id while m.sessionID is still empty, so VS id is the only
		// reliable pairing; fall back to sessionID for untagged loads.
		if msg.virtualSessionID != "" {
			if msg.virtualSessionID != m.virtualSessionID {
				return m, nil
			}
		} else if msg.sessionID != m.sessionID {
			return m, nil
		}
		if msg.err != nil {
			if msg.silent {
				debugLog("silent history load err: %v", msg.err)
				return m, nil
			}
			m.appendHistory(outputStyle.Render(errStyle.Render(
				"could not load session history: " + msg.err.Error())))
			return m, nil
		}
		if msg.silent {
			m.history = msg.entries
		} else {
			m.history = append(msg.entries, historyEntry{
				kind: histPrerendered,
				text: outputStyle.Render(promptStyle.Render(
					fmt.Sprintf("✓ resumed session %s", short(m.sessionID)))),
			})
		}
		m.lastContentFP = ""
		m.layout()
		m.chat.GotoBottom()
		return m, nil

	case virtualSessionMaterializedMsg:
		if msg.tabID != m.id {
			return m, nil
		}
		if msg.vsID != m.virtualSessionID {
			return m, nil
		}
		m.busy = false
		m.status = ""
		if msg.err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render(
				"translate: " + msg.err.Error())))
			if msg.entries != nil {
				m.history = msg.entries
			}
			return m, nil
		}
		m.sessionID = msg.nativeSessionID
		m.resumeCwd = msg.nativeCwd
		if msg.entries != nil {
			m.history = msg.entries
		}
		return m, nil

	case startupResumeMsg:
		if msg.tabID != m.id {
			return m, nil
		}
		// Picker entries always carry id == virtualSessionID — same here.
		// resumeVirtualSession owns the load-vs / dispatch-history /
		// dispatch-translate decision tree, so a CLI resume goes through
		// exactly the same code path as Enter on the picker.
		return m.resumeVirtualSession(sessionEntry{
			id:               msg.vsID,
			virtualSessionID: msg.vsID,
		})

	case sessionsLoadedMsg:
		if msg.tabID != m.id {
			return m, nil
		}
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
		if m.workflowRun != nil {
			// Workflow tabs have no input affordance — the agent
			// gets `cancelled: true` immediately so it doesn't sit
			// waiting on a user answer that will never come.
			if msg.reply != nil {
				msg.reply <- askReply{cancelled: true}
			}
			return m, nil
		}
		if m.mode == modeAskQuestion {
			msg.reply <- askReply{cancelled: true}
			return m, nil
		}
		m = m.startAsk(msg.questions)
		m.askReply = msg.reply
		return m, nil

	case approvalRequestMsg:
		debugLog("approvalRequestMsg tool=%s id=%s", msg.toolName, msg.toolUseID)
		if m.workflowRun != nil {
			// Workflow tabs run with skip-permissions on, but if a
			// provider still routes an approval request through the
			// MCP bridge, auto-deny it so the chain doesn't stall.
			if msg.reply != nil {
				msg.reply <- approvalReply{allow: false}
			}
			return m, nil
		}
		if m.mode == modeApproval || m.mode == modeAskQuestion {
			msg.reply <- approvalReply{allow: false}
			return m, nil
		}
		m = m.startApproval(msg)
		return m, nil

	case workflowRunStartStepMsg:
		return m.workflowRunHandleStartStep(msg)

	case workflowRunStepDoneMsg:
		return m.workflowRunHandleStepDone(msg)

	case workflowStatusChangedMsg:
		// Status changed somewhere — invalidate cached frame so the
		// kanban (if visible) repaints with the new icon. The
		// kanban renderer reads from the workflow tracker on each
		// render, so no per-tab state mutation is needed.
		m.lastContentFP = ""
		if m.fc != nil {
			m.fc.vpFP = ""
			m.fc.vbFP = ""
		}
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
		if m.mode != modeInput {
			return m, nil
		}
		switch m.screen {
		case screenAsk:
			var cmd tea.Cmd
			m.chat, cmd = m.chat.Update(msg)
			m.lastContentFP = ""
			return m, cmd
		case screenIssues:
			return m.issuesHandleMouse(msg)
		}
		return m, nil

	case tea.MouseClickMsg:
		if m.mode != modeInput {
			return m, nil
		}
		if m.screen == screenIssues {
			return m.issuesHandleMouse(msg)
		}
		if m.screen != screenAsk {
			return m, nil
		}
		vpH := m.chat.Height()
		inViewport := msg.Y >= 0 && msg.Y < vpH
		onScrollbar := inViewport && msg.X == m.width-1 && m.chat.TotalLineCount() > vpH
		switch msg.Button {
		case tea.MouseLeft:
			if onScrollbar {
				m.scrollbarDragging = true
				m.scrollViewportTo(msg.Y)
				return m, nil
			}
			if inViewport && msg.X >= 0 && msg.X < m.width-1 {
				m.clearSelection()
				cell := m.screenToContentCell(msg.X, msg.Y)
				m.selAnchor = cell
				m.selFocus = cell
				m.selDragging = true
				m.lastContentFP = ""
				return m, nil
			}
		case tea.MouseRight:
			if m.selActive {
				return m.copySelectionAndClear()
			}
		}
		return m, nil

	case tea.MouseMotionMsg:
		if m.screen == screenIssues {
			return m.issuesHandleMouse(msg)
		}
		if m.scrollbarDragging {
			m.scrollViewportTo(msg.Y)
			return m, nil
		}
		if m.selDragging {
			y := max(0, min(m.chat.Height()-1, msg.Y))
			x := max(0, min(m.width-2, msg.X))
			m.selFocus = m.screenToContentCell(x, y)
			m.lastContentFP = ""
			return m, nil
		}
		return m, nil

	case tea.MouseReleaseMsg:
		if m.screen == screenIssues {
			return m.issuesHandleMouse(msg)
		}
		if m.scrollbarDragging {
			m.scrollbarDragging = false
		}
		if m.selDragging {
			m.selDragging = false
			if m.selAnchor == m.selFocus {
				m.clearSelection()
			} else {
				m.selActive = true
			}
			m.lastContentFP = ""
		}
		return m, nil

	case toastShowMsg, toastTickMsg:
		if m.toast == nil {
			return m, nil
		}
		next, cmd := m.toast.Update(msg)
		m.toast = next
		return m, cmd

	case tea.PasteMsg:
		if m.mode == modeConfig && m.configMemoryPickerActive && m.configMemoryFieldEditing != "" {
			return m.applyConfigMemoryPaste(msg.Content)
		}
		if m.mode == modeConfig && m.configProjectPickerActive && m.configProjectFieldEditing != "" {
			return m.applyConfigProjectPaste(msg.Content)
		}
		if m.mode != modeInput {
			return m, nil
		}
		// Workflow tabs replace the composer with a status banner; a
		// forwarded paste would land in a hidden textarea with no way
		// to submit. Mirrors workflowTabHandleKey absorbing typed keys.
		if m.workflowRun != nil {
			return m, nil
		}
		// Inline confirms overlay the input area and intercept typed
		// keys so they don't bleed into the textarea — paste must
		// follow the same rule or it becomes a side-channel write.
		if m.cancelTurnConfirming || m.closeTabConfirming {
			return m, nil
		}
		// No !m.busy gate: typed keys, image pastes, and shell-mode
		// keystrokes all reach the textarea while a turn (or shell
		// command) is in flight so the user can stage a follow-up.
		// Bracketed paste is the same input modality and must match.
		return m, m.updateComposer(msg)

	case shellBatchMsg:
		if msg.tabID != m.id {
			return m, nil
		}
		if len(msg.lines) > 0 {
			parts := make([]string, 0, len(msg.lines))
			for _, l := range msg.lines {
				styled := l.text
				if l.err {
					styled = errStyle.Render(l.text)
				}
				parts = append(parts, outputStyle.Render(styled))
			}
			joined := strings.Join(parts, "\n")
			if m.shellOutIdx >= 0 && m.shellOutIdx < len(m.history) {
				e := &m.history[m.shellOutIdx]
				if e.text != "" {
					e.text += "\n"
				}
				e.text += joined
				invalidateEntryRender(e)
			} else {
				m.appendHistory(joined)
				m.shellOutIdx = len(m.history) - 1
			}
			m.lastContentFP = ""
		}
		if msg.done != nil {
			d := *msg.done
			m.shellOutIdx = -1
			m.shellCh = nil
			m.shellProc = nil
			if d.newCwd != "" && d.newCwd != m.cwd {
				if err := os.Chdir(d.newCwd); err == nil {
					m.cwd = d.newCwd
					// Sync the memory-hook tenant tuple to the new
					// project. shell-mode cd is the second of two
					// places ask cwd can move (the other is /cd in
					// commands.go); both must update the bridge.
					m.mcpBridge.setCwd(d.newCwd)
					m.refreshPrompt()
					m.pending = nil
					m.refreshPathMatches()
				}
			}
			if d.err != nil {
				debugLog("shell cmd %q err: %v", d.input, d.err)
				m.appendHistory(outputStyle.Render(errStyle.Render(d.err.Error())))
			}
			return m, nil
		}
		if m.shellCh != nil {
			return m, nextShellStreamCmd(m.shellCh, m.id)
		}
		return m, nil

	case tea.KeyPressMsg:
		// Lock-state modifiers (CapsLock/NumLock/ScrollLock) are reported
		// on every keypress under the Kitty keyboard protocol. Treating
		// them as real modifiers would silently break `Mod == 0` gates on
		// arrow keys, Esc, Enter, etc., so strip them before dispatch.
		msg.Mod &^= tea.ModCapsLock | tea.ModNumLock | tea.ModScrollLock
		// Modal/picker dispatch comes first: anything modal-shaped owns
		// the keyboard until it dismisses, regardless of which screen is
		// underneath. Screen-switching is gated against the same
		// modalOpen() check below so this ordering is consistent.
		switch m.mode {
		case modeSessionPicker:
			return m.updatePicker(msg)
		case modeAskQuestion:
			return m.updateAsk(msg)
		case modeApproval:
			return m.updateApproval(msg)
		case modeConfig:
			return m.updateConfigModal(msg)
		case modeProviderSwitch:
			return m.updateProviderSwitch(msg)
		}
		// Workflow tabs are read-only: the input area is replaced
		// with a status banner and the user has no way to type a
		// turn into the chain. We allow only Ctrl+D (close tab),
		// Ctrl+C (cancel — drops the proc, marks failed, stays open
		// for the user to read), and viewport scroll keys so the
		// user can read the streaming output. Screen-switching is
		// blocked too — leaving a half-finished workflow behind on
		// another screen would be confusing.
		if m.workflowRun != nil {
			return m.workflowTabHandleKey(msg)
		}
		// Screen-switching keys (Ctrl+I → issues, Ctrl+O → ask) are
		// global within a tab but blocked while a modal/confirm overlay
		// is up. They must run before the active screen's updateKey so a
		// screen handler can't shadow them by accident. Background work
		// keeps streaming into the active tab regardless of which screen
		// the user is looking at — message routing is by message type at
		// the model layer, not by screen focus.
		//
		// Ctrl+I has overloaded semantics: from any other screen it
		// enters the issues screen, but once already on issues it
		// cycles through the layer views (list → kanban → … → list).
		// Cycling is a no-op on views that aren't in the cycle (e.g.
		// the detail view returned by Enter), which keeps Ctrl+I from
		// surprising a user mid-read.
		if msg.Mod == tea.ModCtrl && msg.Code == 'i' && !m.modalOpen() {
			if m.screen == screenIssues {
				if m.issues != nil {
					m.issues.cycleView()
					// Landing on kanban kicks off N parallel column
					// loads if columns are empty. The same call is a
					// no-op when columns are already loaded (returning
					// to kanban after a previous fetch shows the
					// cached data immediately).
					if kv, ok := m.issues.view.(*kanbanIssueView); ok {
						return m, kv.initialLoad(m.issues)
					}
				}
				return m, nil
			}
			// Hard-gate the screen-switch on a configured provider:
			// if the project has no provider (or the configured one
			// can't dispatch), refuse the transition entirely and
			// surface the toast. Letting the user land on issues with
			// nothing to look at would be a worse UX than staying put
			// and seeing why.
			cfg, _ := loadConfig()
			provider, pc := activeIssueProvider(cfg, m.cwd)
			if !provider.Configured(pc, m.cwd) {
				return m, m.toast.show(
					"Issues not configured for this project: " + shortCwdOf(projectKey(m.cwd)))
			}
			m = m.switchScreen(screenIssues)
			// Flip the loading flag and pick a fun message before
			// dispatching the provider call. The screen renders a
			// centered animated modal whenever loading is true, so
			// the user sees activity instead of a blank kanban
			// during the network round-trip. Any prior loadErr is
			// cleared so the new attempt starts fresh.
			if m.issues == nil {
				m.issues = newIssuesState()
			}
			// Install the resolved provider so query fingerprinting
			// and kanban column lookups operate against the right
			// backend. Resetting the active sub-view rebuilds it
			// against the freshly-installed provider (which the
			// kanban view needs to construct columns).
			m.issues.provider = provider
			m.issues.projectCfg = pc
			m.issues.cwd = m.cwd
			m.issues.tabID = m.id
			m.issues.view = issueViewLayers[0].builder(m.issues)
			kv, _ := m.issues.view.(*kanbanIssueView)
			// Show the loader modal on first entry — when no kanban
			// column has data yet. Re-entry after a previous load
			// (cache hit) skips the modal so the user sees content
			// immediately.
			anyLoaded := false
			if kv != nil {
				for _, c := range kv.columns {
					if len(c.loaded) > 0 {
						anyLoaded = true
						break
					}
				}
			}
			cmds := []tea.Cmd{}
			if !anyLoaded {
				m.issues.loading = true
				m.issues.loadingMessage = pickLoadingMessage()
				m.issues.loadingFrame = 0
				m.issues.loadErr = nil
				cmds = append(cmds, issueLoadingTickCmd(m.id))
			}
			_ = m.issues.beginLoad()
			if kv != nil {
				if c := kv.initialLoad(m.issues); c != nil {
					cmds = append(cmds, c)
				}
			}
			if len(cmds) == 0 {
				return m, nil
			}
			return m, tea.Batch(cmds...)
		}
		if msg.Mod == tea.ModCtrl && msg.Code == 'w' && !m.modalOpen() {
			// Ctrl+W opens the workflows builder. Going to the
			// builder from the issues screen mirrors Ctrl+I's
			// "drop the cache + cancel in-flight" hygiene so a
			// later return to issues re-fetches cleanly.
			if m.screen == screenIssues && m.issues != nil {
				if kv, ok := m.issues.view.(*kanbanIssueView); ok {
					kv.cancelCarry(m.issues)
				}
				m.issues.discardOnLeave()
			}
			m = m.switchScreen(screenWorkflows)
			if m.workflowsBuilder == nil {
				m.workflowsBuilder = newWorkflowsBuilderState(m.cwd)
			} else {
				m.workflowsBuilder.refreshItems()
			}
			return m, nil
		}
		if msg.Mod == tea.ModCtrl && msg.Code == 'o' && !m.modalOpen() {
			// Leaving issues: drop cache + cancel in-flight so re-entry
			// doesn't stack duplicate chunks onto the chain. Carry
			// state lives on the kanban view, so we re-insert the
			// in-flight card before discardOnLeave wipes the cache —
			// otherwise the row would silently disappear next entry.
			if m.screen == screenIssues && m.issues != nil {
				if kv, ok := m.issues.view.(*kanbanIssueView); ok {
					kv.cancelCarry(m.issues)
				}
				m.issues.discardOnLeave()
			}
			return m.switchScreen(screenAsk), nil
		}
		newM, cmd, _ := m.activeScreen().updateKey(m, msg)
		return newM, cmd

	default:
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
}

func (m model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	if m.shellMode {
		return m.updateShellInput(msg)
	}
	if msg.Text == "!" && m.input.Value() == "" && len(m.pending) == 0 && !m.busy {
		m.shellMode = true
		m.shellBsArmed = false
		m.resetHistoryNav()
		m.refreshPathMatches()
		return m, nil
	}
	if m.cancelTurnConfirming {
		return m.updateCancelTurnConfirm(msg)
	}
	if m.closeTabConfirming {
		return m.updateCloseTabConfirm(msg)
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
				return m, closeTabCmd(m.id)
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
	if msg.Mod == tea.ModCtrl && msg.Code == 'b' {
		if m.busy {
			// Don't allow swapping mid-turn — the stream reader is
			// tied to the current proc and the session id is about to
			// be wiped.
			return m, nil
		}
		// Provider switch ends up calling ProbeInit on the new
		// provider, which forks claude/codex to discover slash
		// commands. Refuse the same way as a regular send when ask's
		// cwd isn't a valid checkout root.
		if invalid := validateAskCwd(m.cwd); invalid.Msg != "" {
			m.appendHistory(outputStyle.Render(errStyle.Render(invalid.Msg)))
			return m, nil
		}
		return m.openProviderSwitch(), nil
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
		if m.input.Value() == "" {
			m.closeTabConfirming = true
			m.closeTabChoice = 0
			return m, nil
		}
		return m, nil
	}

	items := m.filterSlashCmds()
	menuOpen := !m.busy && m.historyIdx < 0 && len(items) > 0
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
			m.chat.ScrollUp(m.chat.Height() / 2)
			m.lastContentFP = ""
			return m, nil
		case tea.KeyPgDown:
			m.chat.ScrollDown(m.chat.Height() / 2)
			m.lastContentFP = ""
			return m, nil
		case tea.KeyEnter:
			val := m.input.Value()
			line := strings.TrimSpace(val)
			debugLog("Enter line=%q valLen=%d busy=%v pending=%d pathCmd=%q bare=%q",
				line, len(val), m.busy, len(m.pending), m.pathPickerCmd(), bareCommand(line))
			// Slash menu open + the typed value is not yet a registered
			// command exactly: Enter completes the highlighted entry
			// instead of submitting (mirrors Tab). When the value IS an
			// exact match — even with longer commands also matching the
			// prefix (e.g. "/omc" alongside "/omc-ab") — fall through to
			// the regular submit path so the user can run the short one.
			if menuOpen && !slashCmdsContain(items, val) {
				pick := items[m.menuIdx].name
				m.input.SetValue(pick)
				m.menuIdx = 0
				m.resetHistoryNav()
				return m, nil
			}
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
			return m.sendToProvider(val)
		}
	}

	return m, m.updateComposer(msg)
}

func (m *model) updateComposer(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.historyIdx >= 0 && m.historyIdx < len(m.inputHistory) &&
		m.input.Value() != m.inputHistory[m.historyIdx] {
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
	return cmd
}

func (m model) updateShellInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	running := m.shellCh != nil
	isCtrlC := msg.Mod == tea.ModCtrl && msg.Code == 'c'
	isEsc := msg.Mod == 0 && msg.Code == tea.KeyEsc

	if isCtrlC {
		if running {
			m.killShellProc()
			return m, nil
		}
		m = m.exitShellMode()
		return m, nil
	}
	if isEsc {
		if running {
			return m, nil
		}
		m = m.exitShellMode()
		return m, nil
	}
	if running {
		if msg.Mod == 0 && msg.Code == tea.KeyEnter {
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	isBackspace := msg.Mod == 0 && msg.Code == tea.KeyBackspace
	if isBackspace && m.input.Value() == "" {
		if m.shellBsArmed {
			m = m.exitShellMode()
			return m, nil
		}
		m.shellBsArmed = true
		return m, nil
	}
	m.shellBsArmed = false

	if msg.Mod == 0 {
		switch msg.Code {
		case tea.KeyUp:
			if m.shellHistoryIdx >= 0 || m.input.Line() == 0 {
				if m.shellHistoryPrev() {
					return m, nil
				}
			}
		case tea.KeyDown:
			if m.shellHistoryIdx >= 0 {
				m.shellHistoryNext()
				return m, nil
			}
		case tea.KeyEnter:
			val := m.input.Value()
			if strings.TrimSpace(val) == "" {
				return m, nil
			}
			m.recordShellHistory(val)
			m.input.Reset()
			m.appendUser(val)
			cmd := m.startShellCmd(val)
			return m, cmd
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.shellHistoryIdx >= 0 && m.input.Value() != m.shellHistory[m.shellHistoryIdx] {
		m.resetShellHistoryNav()
	}
	return m, cmd
}

func (m model) exitShellMode() model {
	m.shellMode = false
	m.shellBsArmed = false
	m.input.Reset()
	m.resetShellHistoryNav()
	m.refreshPathMatches()
	return m
}

func (m *model) recordShellHistory(val string) {
	m.resetShellHistoryNav()
	if val == "" {
		return
	}
	if n := len(m.shellHistory); n > 0 && m.shellHistory[n-1] == val {
		return
	}
	m.shellHistory = append(m.shellHistory, val)
}

func (m *model) resetShellHistoryNav() {
	m.shellHistoryIdx = -1
	m.shellHistoryDraft = ""
}

func (m *model) shellHistoryPrev() bool {
	if len(m.shellHistory) == 0 {
		return false
	}
	if m.shellHistoryIdx == -1 {
		m.shellHistoryDraft = m.input.Value()
		m.shellHistoryIdx = len(m.shellHistory) - 1
	} else if m.shellHistoryIdx > 0 {
		m.shellHistoryIdx--
	} else {
		return true
	}
	m.input.SetValue(m.shellHistory[m.shellHistoryIdx])
	m.input.CursorEnd()
	return true
}

func (m *model) shellHistoryNext() {
	if m.shellHistoryIdx == -1 {
		return
	}
	m.shellHistoryIdx++
	if m.shellHistoryIdx >= len(m.shellHistory) {
		draft := m.shellHistoryDraft
		m.shellHistoryIdx = -1
		m.shellHistoryDraft = ""
		if draft != "" {
			m.input.SetValue(draft)
			m.input.CursorEnd()
		} else {
			m.input.Reset()
		}
	} else {
		m.input.SetValue(m.shellHistory[m.shellHistoryIdx])
		m.input.CursorEnd()
	}
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
	mm, cmd := m.cancelTurn()
	return mm, cmd
}

func (m *model) dismissCancelTurnConfirmIfIdle() {
	if !m.busy {
		m.cancelTurnConfirming = false
		m.cancelTurnChoice = 0
	}
}

func (m model) updateCloseTabConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'd':
		m.closeTabConfirming = false
		return m, closeTabCmd(m.id)
	case msg.Mod == tea.ModCtrl && msg.Code == 'c':
		m.closeTabConfirming = false
		m.closeTabChoice = 0
		return m, nil
	case msg.Code == tea.KeyEsc, msg.Code == 'n' && msg.Mod == 0:
		m.closeTabConfirming = false
		m.closeTabChoice = 0
		return m, nil
	case msg.Code == 'y' && msg.Mod == 0:
		m.closeTabConfirming = false
		return m, closeTabCmd(m.id)
	case msg.Code == tea.KeyLeft, msg.Code == 'h' && msg.Mod == 0:
		m.closeTabChoice = 0
		return m, nil
	case msg.Code == tea.KeyRight, msg.Code == 'l' && msg.Mod == 0:
		m.closeTabChoice = 1
		return m, nil
	case msg.Code == tea.KeyTab:
		m.closeTabChoice = 1 - m.closeTabChoice
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.closeTabChoice == 1 {
			m.closeTabConfirming = false
			return m, closeTabCmd(m.id)
		}
		m.closeTabConfirming = false
		m.closeTabChoice = 0
		return m, nil
	}
	return m, nil
}

func (m model) updatePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
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
			return m.resumeVirtualSession(m.sessions[m.pickerIdx])
		}
	}
	return m, nil
}

// resumeVirtualSession wires the picker's selection into the tab.
// Every entry is a VirtualSession id; the current tab's provider
// decides which native session to load:
//
//   - the VS already has a mapping for this provider → resume that
//     native id directly (m.sessionID set, provider.LoadHistory runs)
//   - the VS has no mapping for this provider → render history via
//     a source provider that does have a mapping (so the user sees
//     the conversation), but leave m.sessionID empty so the next
//     user turn starts a fresh native session on the current
//     provider. providerDoneMsg's upsert then records the fresh
//     native id under the same VS id.
func (m model) resumeVirtualSession(entry sessionEntry) (tea.Model, tea.Cmd) {
	m.killProc()
	store, err := loadVirtualSessions()
	if err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render(
			"could not load sessions.json: " + err.Error())))
		m.mode = modeInput
		return m, nil
	}
	vs := store.findByID(entry.virtualSessionID)
	if vs == nil {
		m.appendHistory(outputStyle.Render(errStyle.Render(
			"virtual session " + short(entry.virtualSessionID) + " not found")))
		m.mode = modeInput
		return m, nil
	}
	m.virtualSessionID = vs.ID
	m.mode = modeInput
	m.history = nil
	m.addedDirs = append([]string(nil), vs.AddedDirs...)

	providerID := m.provider.ID()
	opts := HistoryOpts{
		RenderDiffs: m.renderDiffs,
		ToolOutput:  m.toolOutputMode,
		QuietMode:   m.quietMode,
	}
	// Reuse the cached native id only when the current provider was
	// also the last writer (or no last-writer recorded, treated as
	// benign). A stale mapping — e.g. claude mapping cached before
	// codex took subsequent turns — would strand the newer turns on
	// the other provider's file; translating from VS.LastProvider is
	// the only way to pick up the canonical state.
	if ref, ok := vs.ProviderSessions[providerID]; ok && ref.SessionID != "" &&
		(vs.LastProvider == "" || vs.LastProvider == providerID) {
		m.sessionID = ref.SessionID
		m.sessionMinted = false
		m.resumeCwd = ref.Cwd
		m.appendHistory(outputStyle.Render(dimStyle.Render(
			fmt.Sprintf("loading session %s…", short(vs.ID)))))
		return m, loadHistoryCmd(m.id, m.provider, ref.SessionID, vs.ID, opts, false)
	}

	// Translate from VS.LastProvider (or a registry-order fallback
	// when LastProvider isn't registered): materialize a fresh
	// native session on the current provider with the last-writer's
	// turns, then resume that. The mapping for the current provider
	// is overwritten so the next swap picks up from here.
	m.sessionID = ""
	m.sessionMinted = false
	m.resumeCwd = ""
	sourceProv, sourceRef, ok := pickSourceProvider(vs)
	if !ok {
		m.appendHistory(outputStyle.Render(dimStyle.Render(
			fmt.Sprintf("resumed %s — no prior provider history to replay", short(vs.ID)))))
		return m, nil
	}
	m.busy = true
	m.status = "translating session…"
	m.appendHistory(outputStyle.Render(dimStyle.Render(
		fmt.Sprintf("translating %s from %s → %s…",
			short(vs.ID), sourceProv.DisplayName(), m.provider.DisplayName()))))
	return m, translateVSCmd(translateVSReq{
		tabID:           m.id,
		target:          m.provider,
		vsID:            vs.ID,
		workspace:       m.cwd,
		nativeCwd:       nativeCwdForUpsert(m.cwd, m.worktreeName),
		source:          sourceProv,
		sourceSessionID: sourceRef.SessionID,
		opts:            opts,
	})
}

// pickSourceProvider returns a registered provider that has a native
// session recorded on the VS, preferring LastProvider and falling
// back to registry order. ok=false means no registered provider has
// a mapping on this VS (empty or stranded-by-provider-unregister).
func pickSourceProvider(vs *VirtualSession) (Provider, ProviderSessionRef, bool) {
	if vs == nil {
		return nil, ProviderSessionRef{}, false
	}
	tryID := func(id string) (Provider, ProviderSessionRef, bool) {
		ref, ok := vs.ProviderSessions[id]
		if !ok || ref.SessionID == "" {
			return nil, ProviderSessionRef{}, false
		}
		for _, p := range providerRegistry {
			if p.ID() == id {
				return p, ref, true
			}
		}
		return nil, ProviderSessionRef{}, false
	}
	if p, ref, ok := tryID(vs.LastProvider); ok {
		return p, ref, true
	}
	for _, p := range providerRegistry {
		if p, ref, ok := tryID(p.ID()); ok {
			return p, ref, true
		}
	}
	return nil, ProviderSessionRef{}, false
}

func (m model) handleCommand(line string) (tea.Model, tea.Cmd) {
	cmd, _, _ := strings.Cut(line, " ")
	if invalid := validateAskCwd(m.cwd); invalid.Msg != "" {
		switch cmd {
		case "/resume", "/new", "/clear", "/model", "/effort", "/config", "/workflows":
			// Pure UI commands are still safe to run when ask's cwd
			// is invalid — they don't fork a provider. Blocking them
			// would also strand the user without a way to fix things
			// (e.g. /config to flip worktree off). /resume is the
			// only exception: it pulls a session list scoped to cwd
			// and the user could pick something that triggers a
			// resume-and-send sequence we'd then have to refuse
			// asymmetrically. Easier to just block /resume here.
			if cmd == "/resume" {
				m.appendHistory(outputStyle.Render(errStyle.Render(invalid.Msg)))
				return m, nil
			}
		default:
			// Provider slash commands (or unknown ones) end up
			// dispatched via sendToProvider → fork a provider, which
			// we must refuse the same way as a plain user message.
			// sendToProvider has its own gate, but inserting the
			// error here keeps the message ordering clean (no leaked
			// "user typed X" entry for slash dispatches that would
			// be immediately followed by the error).
			m.appendHistory(outputStyle.Render(errStyle.Render(invalid.Msg)))
			return m, nil
		}
	}
	switch cmd {
	case "/resume":
		return m, loadSessionsCmd(m.id, m.cwd)
	case "/new", "/clear":
		m.killProc()
		m.sessionID = ""
		m.sessionMinted = false
		m.resumeCwd = ""
		m.worktreeName = ""
		m.virtualSessionID = ""
		m.history = nil
		m.addedDirs = nil
		(&m).clearSelection()
		m.appendHistory(outputStyle.Render(promptStyle.Render("✓ new session")))
		return m, nil
	case "/add-dir":
		// Bare /add-dir from the slash menu (no path). Arg-supplied
		// runs go through the path picker, which dispatches to doAddDir
		// before handleCommand sees the line.
		m.appendHistory(outputStyle.Render(errStyle.Render(
			"/add-dir: missing directory argument")))
		return m, nil
	case "/model":
		picker := m.provider.ModelPicker()
		if len(picker.Options) == 0 && !picker.AllowCustom {
			m.appendHistory(outputStyle.Render(errStyle.Render(
				"/model: " + m.provider.DisplayName() + " has no model picker yet")))
			return m, nil
		}
		m = m.startModelPicker()
		return m, nil
	case "/effort":
		m = m.startEffortPicker()
		return m, nil
	case "/config":
		m = m.startConfigModal()
		return m, nil
	case "/workflows":
		// /workflows opens the builder. Same flow as Ctrl+W: drop
		// any in-flight issues query so re-entry to the issues
		// screen later doesn't stack chunks.
		if m.screen == screenIssues && m.issues != nil {
			if kv, ok := m.issues.view.(*kanbanIssueView); ok {
				kv.cancelCarry(m.issues)
			}
			m.issues.discardOnLeave()
		}
		m = m.switchScreen(screenWorkflows)
		if m.workflowsBuilder == nil {
			m.workflowsBuilder = newWorkflowsBuilderState(m.cwd)
		} else {
			m.workflowsBuilder.refreshItems()
		}
		return m, nil
	}
	bare := strings.TrimPrefix(cmd, "/")
	for _, e := range m.providerSlashCmds {
		if e.Name == bare {
			return m.sendToProvider(line)
		}
	}
	m.appendHistory(outputStyle.Render(errStyle.Render("unknown command: " + cmd)))
	return m, nil
}

func persistSlashCmdsCmd(p Provider, cmds []providerSlashEntry) tea.Cmd {
	return func() tea.Msg {
		settings := p.LoadSettings()
		settings.SlashCommands = cmds
		if err := p.SaveSettings(settings); err != nil {
			debugLog("SaveSettings err: %v", err)
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
	m.historyDraft = ""
}

func (m *model) historyPrev() bool {
	if len(m.inputHistory) == 0 {
		return false
	}
	if m.historyIdx == -1 {
		m.historyDraft = m.input.Value()
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
		draft := m.historyDraft
		m.historyIdx = -1
		m.historyDraft = ""
		if draft != "" {
			m.input.SetValue(draft)
			m.input.CursorEnd()
		} else {
			m.input.Reset()
		}
	} else {
		m.input.SetValue(m.inputHistory[m.historyIdx])
		m.input.CursorEnd()
	}
}

// slashCmdsContain reports whether typed exactly matches one of items'
// command names. Enter uses it to decide between autocomplete (no
// exact match) and submit (exact match) when the typed value is also
// a strict prefix of longer registered commands. typed must be the
// raw input.Value() so the comparand stays aligned with how
// filterSlashCmds populated items in the first place.
func slashCmdsContain(items []slashCmd, typed string) bool {
	for _, c := range items {
		if c.name == typed {
			return true
		}
	}
	return false
}

func (m model) filterSlashCmds() []slashCmd {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") {
		return nil
	}
	builtins := append(m.provider.BaseSlashCommands(), appBuiltinSlashCmds...)
	seen := make(map[string]bool, len(builtins))
	var out []slashCmd
	for _, c := range builtins {
		if seen[c.name] {
			continue
		}
		seen[c.name] = true
		if strings.HasPrefix(c.name, val) {
			out = append(out, c)
		}
	}
	for _, e := range m.providerSlashCmds {
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

// reapBgTaskByAgentID drops the bgTasks entry whose key (task_id) or
// stored value (the spawning Task call's tool_use_id) equals agentID.
// Returns the removed task_id, or "" when nothing matched. Tolerates a
// nil map — late hook deliveries after killProc/providerExitedMsg
// reset bgTasks to nil routinely.
func reapBgTaskByAgentID(bgTasks map[string]string, agentID string) string {
	if agentID == "" || len(bgTasks) == 0 {
		return ""
	}
	if _, ok := bgTasks[agentID]; ok {
		delete(bgTasks, agentID)
		return agentID
	}
	for taskID, toolUseID := range bgTasks {
		if toolUseID != "" && toolUseID == agentID {
			delete(bgTasks, taskID)
			return taskID
		}
	}
	return ""
}
