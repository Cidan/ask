package main

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/workflow"
)

// Coordinator manages the background execution of all in-process agent sessions
// and decouples them from the Bubble Tea TUI model.
type Coordinator struct {
	mu              sync.RWMutex
	sessions        map[int]*agentSession
	workflowCancels map[int]context.CancelFunc
}

var globalCoordinator = &Coordinator{
	sessions:        make(map[int]*agentSession),
	workflowCancels: make(map[int]context.CancelFunc),
}

// GetSession retrieves the active session for a tab.
func (c *Coordinator) GetSession(tabID int) *agentSession {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[tabID]
}

// HasSession reports whether there is an active session for a tab.
func (c *Coordinator) HasSession(tabID int) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[tabID] != nil
}

// IsBusy reports whether the session for a tab is currently busy.
func (c *Coordinator) IsBusy(tabID int) bool {
	c.mu.RLock()
	session := c.sessions[tabID]
	c.mu.RUnlock()
	if session == nil {
		return false
	}
	return session.isBusy()
}

// IsWorkflowRunning reports whether a workflow is actively running on the tab.
func (c *Coordinator) IsWorkflowRunning(tabID int) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.workflowCancels[tabID]
	return ok
}

// SetSession sets the active session for a tab.
func (c *Coordinator) SetSession(tabID int, s *agentSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[tabID] = s
}

// RemoveSession removes the active session for a tab.
func (c *Coordinator) RemoveSession(tabID int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, tabID)
}

// Dispatch sends a user turn to the tab's active session, starting it if needed.
func (c *Coordinator) Dispatch(tabID int, p Provider, args ProviderSessionArgs, text string, attachments []pendingAttachment) error {
	c.mu.Lock()
	session := c.sessions[tabID]
	if session == nil {
		c.mu.Unlock()
		// Create (or reuse) the worktree and repoint args.Cwd into it before
		// the session starts, so the session's tools, prompt, and session
		// store all run in the worktree. A no-op when worktree mode is off or
		// the cwd is not a repo. This is provider-agnostic — it belongs here,
		// not in a provider's StartSession.
		prepared, worktreeName, err := prepareProviderSession(args, args.WorktreeName)
		if err != nil {
			return err
		}
		args = prepared
		proc, _, err := p.StartSession(args)
		if err != nil {
			return err
		}
		if proc.payload != nil {
			if s, ok := proc.payload.(*agentSession); ok {
				session = s
				c.mu.Lock()
				c.sessions[tabID] = session
				c.mu.Unlock()
				// Surface the worktree cwd so the tab sets m.worktreeName and
				// shows the [🌳 name] chip. Only when a worktree is in play.
				if worktreeName != "" {
					session.emit(providerCwdMsg{cwd: args.Cwd})
				}
			}
		} else if proc != nil {
			return p.Send(proc, text, attachments)
		}
	} else {
		c.mu.Unlock()
	}

	if session != nil {
		var files []engine.FilePart
		if len(attachments) > 0 {
			files = attachmentFileParts(attachments)
		}
		if session.isBusy() {
			session.queueMidTurn(text)
			return nil
		}
		return session.queueTurn(text, files)
	}
	return nil
}

// Cancel interrupts the in-flight turn for a tab's session.
func (c *Coordinator) Cancel(tabID int) bool {
	session := c.GetSession(tabID)
	if session == nil {
		return false
	}
	return session.interruptTurn()
}

// CancelWorkflow cancels any running workflow for a tab.
func (c *Coordinator) CancelWorkflow(tabID int) {
	c.mu.Lock()
	cancel, ok := c.workflowCancels[tabID]
	c.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
	c.Cancel(tabID)
}

// Kill shuts down the session for a tab and removes it.
func (c *Coordinator) Kill(tabID int) {
	c.mu.Lock()
	session := c.sessions[tabID]
	delete(c.sessions, tabID)
	c.mu.Unlock()

	if session != nil {
		session.shutdown()
	}
}

// Clear all active sessions
func (c *Coordinator) Clear() {
	c.mu.Lock()
	targets := make([]*agentSession, 0, len(c.sessions))
	for _, s := range c.sessions {
		targets = append(targets, s)
	}
	c.sessions = make(map[int]*agentSession)
	c.mu.Unlock()

	for _, s := range targets {
		s.shutdown()
	}
}

// injectTabID sets the tabID field on message structs that support it.
func injectTabID(msg tea.Msg, tabID int) tea.Msg {
	switch m := msg.(type) {
	case streamStatusMsg:
		m.tabID = tabID
		return m
	case statusRevertMsg:
		m.tabID = tabID
		return m
	case assistantTextMsg:
		m.tabID = tabID
		return m
	case turnCompleteMsg:
		m.tabID = tabID
		return m
	case usageMsg:
		m.tabID = tabID
		return m
	case costMsg:
		m.tabID = tabID
		return m
	case providerModelMsg:
		m.tabID = tabID
		return m
	case todoUpdatedMsg:
		m.tabID = tabID
		return m
	case bgTaskStartedMsg:
		m.tabID = tabID
		return m
	case bgTaskEndedMsg:
		m.tabID = tabID
		return m
	case providerCwdMsg:
		m.tabID = tabID
		return m
	case toolCallMsg:
		m.tabID = tabID
		return m
	case toolResultMsg:
		m.tabID = tabID
		return m
	case toolDiffMsg:
		m.tabID = tabID
		return m
	case providerDoneMsg:
		m.tabID = tabID
		return m
	case providerExitedMsg:
		m.tabID = tabID
		return m
	case queuedMessageDrainedMsg:
		m.tabID = tabID
		return m
	}
	return msg
}

func (m model) matchesTabID(msgTabID int, msgProc *providerProc) bool {
	if m.proc != nil {
		return msgProc == m.proc
	}
	sess := globalCoordinator.GetSession(m.id)
	if sess == nil {
		if msgProc != nil {
			return false
		}
		if msgTabID == 0 {
			return true
		}
		return msgTabID == m.id
	}
	if msgProc != nil && msgProc != sess.proc {
		return false
	}
	if msgTabID == 0 {
		return true
	}
	return msgTabID == m.id
}

type tuiWorkflowListener struct {
	tabID int
}

func (l tuiWorkflowListener) OnWorkflowStarted(tabID int, def workflow.Def, src workflow.Source) {
	agentSendToProgram(WorkflowStartedMsg{
		TabID:    tabID,
		Workflow: fromPkgWorkflowDef(def),
		Source:   src,
	})
}

func (l tuiWorkflowListener) OnWorkflowStepStarted(tabID int, stepIdx int, stepName, provider, model string) {
	agentSendToProgram(WorkflowStepStartedMsg{
		TabID:    tabID,
		StepIdx:  stepIdx,
		StepName: stepName,
		Provider: provider,
		Model:    model,
	})
}

func (l tuiWorkflowListener) OnWorkflowStepDone(tabID int, stepIdx int, summary string) {
	agentSendToProgram(WorkflowStepDoneMsg{
		TabID:   tabID,
		StepIdx: stepIdx,
		Summary: summary,
	})
}

func (l tuiWorkflowListener) OnWorkflowDone(tabID int, description string, artifacts []string) {
	agentSendToProgram(WorkflowDoneMsg{
		TabID:       tabID,
		Description: description,
		Artifacts:   artifacts,
	})
}

func (l tuiWorkflowListener) OnWorkflowFailed(tabID int, reason string) {
	agentSendToProgram(WorkflowFailedMsg{
		TabID:  tabID,
		Reason: reason,
	})
}

func (l tuiWorkflowListener) OnNote(tabID int, text string) {
	agentSendToProgram(AppendHistoryMsg{
		TabID: tabID,
		Text:  text,
	})
}

// workflowWorktree carries the origin tab's worktree context into a
// workflow run. A workflow supplants its tab (or runs inside the live
// chat session), so it must land in the same isolated worktree the tab
// was using — never the project root. name reuses an existing worktree;
// an empty name with on=true and a repo backend creates a fresh one, the
// same rule prepareProviderSession applies to a chat turn.
type workflowWorktree struct {
	root string // project root (never a worktree path)
	name string // worktree to reuse; "" means create when on
	on   bool   // worktree mode enabled for the tab
}

// RunWorkflow compiles the definition to an ADK workflow graph and drives
// it to completion in the background.
func (c *Coordinator) RunWorkflow(ctx context.Context, tabID int, wt workflowWorktree, def workflowDef, src workflowSource) (finalizedPlanReply, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.mu.Lock()
	if c.workflowCancels == nil {
		c.workflowCancels = make(map[int]context.CancelFunc)
	}
	c.workflowCancels[tabID] = cancel
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.workflowCancels, tabID)
		c.mu.Unlock()
	}()

	parentSession := c.GetSession(tabID)
	defer func() {
		if parentSession != nil {
			c.SetSession(tabID, parentSession)
		} else {
			c.RemoveSession(tabID)
		}
	}()

	listener := tuiWorkflowListener{tabID: tabID}

	// Reuse the tab's worktree — or create one when worktree mode is on but
	// the tab has none yet — exactly as Dispatch does for a chat turn.
	// Without this the run writes into the project root, defeating worktree
	// isolation for the most mutation-heavy path there is.
	runArgs, name, err := prepareProviderSessionAt(
		ProviderSessionArgs{Cwd: wt.root, Worktree: wt.on, WorktreeName: wt.name},
		wt.name, wt.root)
	if err != nil {
		listener.OnWorkflowFailed(tabID, err.Error())
		return finalizedPlanReply{}, err
	}
	// A worktree freshly created for this run (the tab had none) must reach
	// the tab so the restored chat session adopts it and the [🌳] chip
	// shows; a reused worktree is already the tab's.
	if name != "" && name != wt.name {
		agentSendToProgram(injectTabID(providerCwdMsg{cwd: runArgs.Cwd}, tabID))
	}

	runState, err := c.runWorkflowGraph(ctx, runArgs.Cwd, tabID, toPkgWorkflowDef(def), src, listener)
	if err != nil {
		return finalizedPlanReply{}, err
	}

	var reply finalizedPlanReply
	reply.workflowName = def.Name
	reply.workflowDone = true
	if runState != nil && runState.FinishData != nil {
		reply.artifacts = runState.FinishData.Artifacts
		reply.outcome = runState.FinishData.Description
	}
	return reply, nil
}
