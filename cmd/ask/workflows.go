package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/Cidan/ask/pkg/workflow"
)

// Workflow status constants. Empty string is "no record" — used by
// the kanban card renderer to short-circuit before reaching for a
// glyph. Only `done` and `failed` ever land on disk; `working` is
// process-local because there's nothing to resume across restarts.
const (
	workflowStatusWorking = workflow.StatusWorking
	workflowStatusDone    = workflow.StatusDone
	workflowStatusFailed  = workflow.StatusFailed
)

// workflowStatusChangedMsg is broadcast by the runtime tracker every
// time an issue's workflow status flips. Issues screens listening for
// this message rebuild their card icons immediately so the kanban
// reflects "working" the moment a workflow tab spawns and the
// terminal status the moment the chain finalises. Empty status means
// the entry was cleared (e.g. cancelled before a single step ran).
type workflowStatusChangedMsg struct {
	issueKey string
	status   string
}

// workflowTracker returns the process-wide workflow runtime tracker.
// The tracker itself — its in-memory entry map, disk-session hydration,
// and status transitions — lives in pkg/workflow (workflow.GlobalTracker),
// shared with the agent-facing workflow run path (see workflow_graph.go
// handing the same GlobalTracker to workflow.NewProgress). This is a thin
// accessor so the TUI reads and writes the SAME tracker the runner does,
// instead of a second in-memory copy the UI never saw. Its status
// listener is wired to broadcastWorkflowStatus in init() so every
// transition — whether from the tab lifecycle (tabs.go) or the runner's
// progress adapter — repaints live screens.
func workflowTracker() *workflow.Tracker { return workflow.GlobalTracker() }

func init() {
	workflow.GlobalTracker().SetListener(broadcastWorkflowStatus)
}

// resetWorkflowTrackerForTest wipes the shared tracker's in-memory map.
// Tests use this to start from a clean slate without a process restart;
// production code never calls it.
func resetWorkflowTrackerForTest() {
	workflow.GlobalTracker().ResetForTest()
}

// broadcastWorkflowStatus delivers a workflowStatusChangedMsg to the
// live tea.Program so every tab's Update sees the change. No-op when
// the program isn't registered (early startup, tests without a
// program).
//
// The Send is deferred onto a goroutine because every caller in this
// package fires from inside Update (markWorking from openWorkflowTab,
// markFinal from advanceWorkflowStep / closeTab, …). Calling
// tea.Program.Send synchronously from inside Update can stall the
// main loop if the program's input channel doesn't have headroom —
// the writer (Update) waits on the reader, who is currently busy
// running Update. mcpBridge.broadcast is the only other Send caller
// in this codebase and it always runs on a fresh HTTP-handler
// goroutine, which is implicitly safe; we mirror that here.
func broadcastWorkflowStatus(key, status string) {
	p := teaProgramPtr.Load()
	if p == nil {
		return
	}
	msg := workflowStatusChangedMsg{issueKey: key, status: status}
	go p.Send(msg)
}

// workflowDefByName looks up the named workflow under cwd's project,
// searching global first, then repo, then user (personal-wins
// resolution). Returns false when no match. Shared between the picker
// and the runtime so the two can't drift on naming rules.
func workflowDefByName(cwd, name string) (workflowDef, bool) {
	return findWorkflow(cwd, name, "")
}

// workflowKeyPrefix returns ("<provider>:<owner/repo>#", true) when
// s has a configured issue provider that can resolve the project
// scope. Used by the kanban / detail renderers to avoid calling
// IssueProvider.IssueRef per card. ok=false on unresolved providers
// so the caller silently skips the status-icon path.
func workflowKeyPrefix(s *issuesState) (string, bool) {
	if s == nil || s.provider == nil {
		return "", false
	}
	r, err := s.provider.IssueRef(s.projectCfg, s.cwd, issue{})
	if err != nil {
		return "", false
	}
	return r.Provider + ":" + r.Project + "#", true
}

// workflowStatusForIssue is the row-render-time lookup. Returns the
// glyph string ("" / ▸ / ✓ / ✗) for the issue under cwd, using the
// pre-computed keyPrefix to avoid resolving owner/repo per row.
// hasPrefix=false short-circuits to "" so the renderer doesn't have
// to gate on it at every call site.
func workflowStatusForIssue(s *issuesState, prefix string, hasPrefix bool, n int) string {
	if !hasPrefix {
		return ""
	}
	key := prefix + itoaInt(n)
	e, ok := workflowTracker().Lookup(s.cwd, key)
	if !ok {
		return ""
	}
	return workflowStatusGlyph(e.Status)
}

// itoaInt is a tiny inline strconv.Itoa used by workflowStatusForIssue
// and the workflow picker; both files want to stay light on imports.
// (strconv is already imported elsewhere; this just avoids reaching
// for it from workflows.go where it isn't.)
func itoaInt(n int) string {
	return itoa(n)
}

// formatIssueCard renders one card row: optional workflow status
// glyph, "#<number>", title, truncated to width. Pulled out of the
// kanban render so the same shape can serve future per-issue
// surfaces (per-assignee swimlanes, milestone grids).
func formatIssueCard(it issue, keyPrefix string, hasKeyPrefix bool, s *issuesState, width int) string {
	glyph := workflowStatusForIssue(s, keyPrefix, hasKeyPrefix, it.number)
	var card string
	if glyph != "" {
		card = fmt.Sprintf("%s #%d  %s", glyph, it.number, it.title)
	} else {
		card = fmt.Sprintf("#%d  %s", it.number, it.title)
	}
	return xansi.Truncate(card, width, "…")
}

// workflowStatusGlyph returns the single-cell status glyph the
// kanban card / detail view prepends to the issue number for a
// given workflow status. Empty status returns "" so the card
// renders flush against the issue number for issues with no
// workflow history. Styling is applied at the call site so the
// glyph can pick up the surrounding row's foreground when needed.
func workflowStatusGlyph(status string) string {
	switch status {
	case workflowStatusWorking:
		return promptStyle.Render("▸")
	case workflowStatusDone:
		return promptStyle.Render("✓")
	case workflowStatusFailed:
		return errStyle.Render("✗")
	}
	return ""
}

// projectWorkflows returns every workflow visible under cwd — repo
// scope (committed .ask/workflows files) first, then user scope (the
// ask.json list, in disk order). The picker / `f` dispatcher / builder
// consume this; an empty slice means no pipelines are configured in
// either scope.
func projectWorkflows(cwd string) []workflowDef {
	return listAllWorkflows(cwd)
}

func (m *model) workflowAssistantText(text string) {
	if m.workflowRun == nil {
		return
	}
	if m.workflowRun.currentStep.Len() > 0 {
		m.workflowRun.currentStep.WriteString("\n")
	}
	m.workflowRun.currentStep.WriteString(text)
}

func (m *model) appendWorkflowStepDone(name, provider, mdl, summary string, indent int) {
	if strings.TrimSpace(summary) == "" {
		return
	}
	m.pushTranscript(transcriptItem{
		kind:           trWorkflowDone,
		text:           summary,
		workflowIndent: indent,
	})
}

func (m *model) appendWorkflowDone(header, body string, indent int) {
	m.responseActive = false
	m.pushTranscript(transcriptItem{
		kind:           trWorkflowDone,
		text:           body,
		workflowHeader: header,
		workflowIndent: indent,
	})
}

func (m model) workflowTabHandleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if currentKeyMap().Matches(ActionTabClose, msg) {
		if r := m.workflowRun; r != nil && (r.done || r.failed) {
			if r.supplanted != nil {
				return m.restoreSupplantedTab()
			}
			return m, closeTabCmd(m.id)
		}
		return m, closeTabCmd(m.id)
	}
	if currentKeyMap().Matches(ActionTaskListToggle, msg) {
		if len(m.todos) > 0 || len(m.activeSubagents) > 0 {
			m.taskListExpanded = !m.taskListExpanded
			return m, nil
		}
	}
	if msg.Mod == 0 && msg.Code == tea.KeyEnter {
		if r := m.workflowRun; r != nil && (r.done || r.failed) {
			if r.supplanted != nil {
				return m.restoreSupplantedTab()
			}
			m.workflowRun = nil
			return m, closeTabCmd(m.id)
		}
		// If workflow is running, let Enter fall through to updateInput so the
		// user can submit mid-turn queue messages.
	}
	switch msg.Code {
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown,
		tea.KeyHome, tea.KeyEnd, 'k', 'j', 'g', 'G':
		var cmd tea.Cmd
		m.chat, cmd = m.chat.Update(msg)
		m.lastContentFP = ""
		return m, cmd
	}

	// Forward everything else to updateInput so users can type into the input box
	// to steer the active workflow step mid-run.
	return m.updateInput(msg)
}

func (m model) restoreSupplantedTab() (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.supplanted == nil || (!r.done && !r.failed) {
		return m, nil
	}
	snap := r.supplanted
	m.workflowRun = nil
	m.provider = snap.provider
	m.providerModel = snap.providerModel
	m.providerEffort = snap.providerEffort
	m.providerSlashCmds = snap.providerSlashCmds
	m.sessionID = snap.sessionID
	m.sessionMinted = snap.sessionMinted
	m.virtualSessionID = snap.virtualSessionID
	m.resumeCwd = snap.resumeCwd
	m.worktreeName = snap.worktreeName
	m.skipAllPermissions = snap.skipAllPermissions
	m.screen = snap.screen
	m.testBusy = false
	m.status = ""
	m.todos = nil
	m.appendHistory(outputStyle.Render(dimStyle.Render(
		"returned to chat — workflow log preserved above")))
	m.lastContentFP = ""
	if m.fc != nil {
		m.fc.vpFP = ""
		m.fc.vbFP = ""
	}
	return m, nil
}
