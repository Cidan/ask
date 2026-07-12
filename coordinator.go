package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
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
		// Start a fresh session
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
			}
		} else if proc != nil {
			// For testing / fake providers that don't return an in-process agentSession payload:
			// forward the turn to the provider directly!
			return p.Send(proc, text, attachments)
		}
	} else {
		c.mu.Unlock()
	}

	if session != nil {
		// Enqueue the turn to the session's background runner
		var files []fantasy.FilePart
		if len(attachments) > 0 {
			files = attachmentFileParts(attachments)
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

// Clear all active sessions (e.g. on application exit)
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

// RunWorkflow Sync executes a workflow synchronously step by step in the background.
// Emits event-bus messages directly to update the TUI live.
func (c *Coordinator) RunWorkflow(ctx context.Context, tabID int, def workflowDef, src workflowSource) (finalizedPlanReply, error) {
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

	agentSendToProgram(WorkflowStartedMsg{
		TabID:    tabID,
		Workflow: def,
		Source:   src,
	})

	runState := &workflowRunState{
		Workflow:  def,
		Source:    src,
		startedAt: time.Now().UTC(),
		StepIdx:   0,
	}

	rootCwd := ""
	if parentSession != nil {
		rootCwd = projectRoot(parentSession.args.Cwd)
	} else {
		rootCwd = projectRoot("")
	}
	workflowTracker().markWorking(rootCwd, src.Key(), def.Name, tabID)

	var stepLog []string
	var loopFrame *loopRunFrame
	var prevNotesDir string
	var currentNotesDir string
	var remind remindKind
	var remindDetail string
	var linearRetry int
	var linearText string
	var stepErrorRetry int

	for {
		select {
		case <-ctx.Done():
			agentSendToProgram(WorkflowFailedMsg{TabID: tabID, Reason: "cancelled by user"})
			return finalizedPlanReply{}, ctx.Err()
		default:
		}

		if loopFrame == nil && runState.StepIdx >= len(def.Steps) {
			break
		}

		top := def.Steps[runState.StepIdx]
		if top.isLoop() && loopFrame == nil {
			if len(top.Steps) == 0 {
				runState.StepIdx++
				continue
			}
			loopFrame = &loopRunFrame{innerIdx: 0, iteration: 1}
			agentSendToProgram(AppendHistoryMsg{
				TabID: tabID,
				Text:  loopNoteLine(top.Name, "started", fmt.Sprintf("max %d iteration(s)", top.effectiveMaxIterations())),
			})
		}

		step := top
		if loopFrame != nil {
			step = top.Steps[loopFrame.innerIdx]
		}

		agentSendToProgram(WorkflowStepStartedMsg{
			TabID:    tabID,
			StepIdx:  runState.StepIdx,
			StepName: step.Name,
			Provider: step.Provider,
			Model:    step.Model,
		})

		isStartStep := runState.StepIdx == 0 && loopFrame == nil
		isLoopStartStep := runState.StepIdx == 0 && loopFrame != nil && loopFrame.iteration == 1 && loopFrame.innerIdx == 0

		worktreeName := ""
		cfg, _ := loadConfig()
		if cfg.UI.Worktree != nil && *cfg.UI.Worktree && worktreeBackendAt(rootCwd) != workspaceBackendNone {
			worktreeName = newWorktreeName(rootCwd)
		}

		var notesDir string
		switch {
		case isStartStep, isLoopStartStep:
			notesDir = startPlanDir(rootCwd, worktreeName)
		case loopFrame != nil:
			notesDir = stepNotesDir(rootCwd, worktreeName, step.Name, top.Name, loopFrame.iteration)
		default:
			notesDir = stepNotesDir(rootCwd, worktreeName, step.Name, "", 0)
		}
		currentNotesDir = notesDir

		var prevOutputs []string
		if loopFrame == nil {
			if linearRetry > 0 && linearText != "" {
				prevOutputs = append(append([]string(nil), stepLog...), linearText)
			} else {
				prevOutputs = stepLog
			}
		} else {
			prevOutputs = append([]string(nil), stepLog...)
			if loopFrame.innerIdx == 0 {
				if loopFrame.prevTail != "" {
					prevOutputs = append(prevOutputs, loopFrame.prevTail)
				}
			} else {
				prevOutputs = append(prevOutputs, loopFrame.iterationLog...)
			}
			if loopFrame.retry > 0 && loopFrame.retryText != "" {
				prevOutputs = append(prevOutputs, loopFrame.retryText)
			}
		}

		pc := &stepPromptCtx{
			remind:              remind,
			remindDetail:        remindDetail,
			notesDir:            notesDir,
			prevNotesDir:        prevNotesDir,
			isStartStep:         isStartStep || isLoopStartStep,
			isWorkflowFinalStep: runState.StepIdx == len(def.Steps)-1,
		}
		if loopFrame != nil {
			pc.loop = &loopPromptCtx{
				name:          top.Name,
				iteration:     loopFrame.iteration,
				maxIterations: top.effectiveMaxIterations(),
				exitCondition: top.ExitCondition,
				isTail:        loopFrame.innerIdx == len(top.Steps)-1,
			}
		}

		var dirErr error
		if pc.isStartStep {
			dirErr = ensureStartPlanExists(rootCwd, worktreeName)
		} else {
			dirErr = ensureStepNotesDir(notesDir)
		}
		if dirErr != nil {
			remind = remindFixPlanDir
			remindDetail = dirErr.Error()
			continue
		}

		prompt := buildWorkflowStepPrompt(step, src, prevOutputs, pc)

		prov := providerByID(step.Provider)
		if prov == nil {
			err := fmt.Errorf("provider not registered: %s", step.Provider)
			agentSendToProgram(WorkflowFailedMsg{TabID: tabID, Reason: err.Error()})
			return finalizedPlanReply{}, err
		}

		args := ProviderSessionArgs{
			Cwd:                 rootCwd,
			TabID:               tabID,
			Model:               step.Model,
			Effort:              "medium",
			SkipAllPermissions:  true,
			InWorkflow:          true,
			IsWorkflowFinalStep: runState.StepIdx == len(def.Steps)-1,
		}

		proc, ch, err := prov.StartSession(args)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				agentSendToProgram(WorkflowFailedMsg{TabID: tabID, Reason: "cancelled by user"})
				return finalizedPlanReply{}, ctx.Err()
			}
			maxRetries, initialDelay, backoffFactor := agentRetryOptions(cfg)
			if stepErrorRetry < maxRetries {
				stepErrorRetry++
				wait := time.Duration(float64(initialDelay) * math.Pow(backoffFactor, float64(stepErrorRetry-1)))
				msg := fmt.Sprintf("step %q failed: %v — retrying in %s (attempt %d of %d)", step.Name, err, humanDuration(wait), stepErrorRetry, maxRetries)
				agentSendToProgram(AppendHistoryMsg{TabID: tabID, Text: msg})
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return finalizedPlanReply{}, ctx.Err()
				}
				continue
			}
			agentSendToProgram(WorkflowFailedMsg{TabID: tabID, Reason: err.Error()})
			return finalizedPlanReply{}, err
		}

		session := proc.payload.(*agentSession)
		c.SetSession(tabID, session)

		err = session.queueTurn(prompt)
		if err != nil {
			session.shutdown()
			c.RemoveSession(tabID)
			agentSendToProgram(WorkflowFailedMsg{TabID: tabID, Reason: err.Error()})
			return finalizedPlanReply{}, err
		}

		var stepResult string
		var stepErr error
		for msg := range ch {
			switch m := msg.(type) {
			case assistantTextMsg:
				stepResult += m.text
			case providerDoneMsg:
				if m.err != nil {
					stepErr = m.err
				} else if m.res.IsError {
					stepErr = fmt.Errorf("step failed: %s", m.res.Result)
				} else {
					stepResult = m.res.Result
				}
			}
		}

		session.shutdown()
		c.RemoveSession(tabID)

		if stepErr != nil {
			if errors.Is(stepErr, context.Canceled) || ctx.Err() != nil {
				agentSendToProgram(WorkflowFailedMsg{TabID: tabID, Reason: "cancelled by user"})
				return finalizedPlanReply{}, ctx.Err()
			}
			maxRetries, initialDelay, backoffFactor := agentRetryOptions(cfg)
			if stepErrorRetry < maxRetries {
				stepErrorRetry++
				wait := time.Duration(float64(initialDelay) * math.Pow(backoffFactor, float64(stepErrorRetry-1)))
				msg := fmt.Sprintf("step %q failed: %v — retrying in %s (attempt %d of %d)", step.Name, stepErr, humanDuration(wait), stepErrorRetry, maxRetries)
				agentSendToProgram(AppendHistoryMsg{TabID: tabID, Text: msg})
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return finalizedPlanReply{}, ctx.Err()
				}
				continue
			}
			agentSendToProgram(WorkflowFailedMsg{TabID: tabID, Reason: stepErr.Error()})
			return finalizedPlanReply{}, stepErr
		}

		stepErrorRetry = 0

		sig := session.env.pendingEndTurn
		finishData := session.env.pendingFinishData

		remind = remindNone
		remindDetail = ""

		if loopFrame == nil {
			if sig == nil {
				linearRetry++
				linearText = stepResult
				remind = remindNoSummary
				agentSendToProgram(AppendHistoryMsg{
					TabID: tabID,
					Text:  workflowMarginStyle.Render("|") + " Re-prompting " + step.Name + " for end_turn",
				})
				continue
			}

			agentSendToProgram(WorkflowStepDoneMsg{
				TabID:   tabID,
				StepIdx: runState.StepIdx,
				Summary: sig.summary,
			})

			if runState.StepIdx == len(def.Steps)-1 && finishData == nil {
				linearRetry++
				linearText = stepResult
				remind = remindNoFinishTool
				agentSendToProgram(AppendHistoryMsg{
					TabID: tabID,
					Text:  workflowMarginStyle.Render("|") + " Re-prompting " + step.Name + " for finish_workflow",
				})
				continue
			}

			if runState.StepIdx == len(def.Steps)-1 && finishData != nil {
				runState.finishData = finishData
			}

			prevNotesDir = currentNotesDir
			if stepResult != "" {
				stepLog = append(stepLog, stepResult)
			}
			linearRetry = 0
			linearText = ""
			runState.StepIdx++
			continue
		}

		isTail := loopFrame.innerIdx == len(top.Steps)-1
		if sig == nil {
			loopFrame.retry++
			loopFrame.retryText = stepResult
			remind = remindNoSummary
			agentSendToProgram(AppendHistoryMsg{
				TabID: tabID,
				Text:  workflowMarginStyle.Render("|") + " Re-prompting " + step.Name + " for end_turn",
			})
			continue
		}

		agentSendToProgram(WorkflowStepDoneMsg{
			TabID:   tabID,
			StepIdx: runState.StepIdx,
			Summary: sig.summary,
		})

		if isTail && sig.decision == workflowLoopBreak {
			if runState.StepIdx == len(def.Steps)-1 && finishData == nil {
				loopFrame.retry++
				loopFrame.retryText = stepResult
				remind = remindNoFinishTool
				agentSendToProgram(AppendHistoryMsg{
					TabID: tabID,
					Text:  workflowMarginStyle.Render("|") + " Re-prompting final step for finish_workflow",
				})
				continue
			}

			if runState.StepIdx == len(def.Steps)-1 && finishData != nil {
				runState.finishData = finishData
			}

			prevNotesDir = currentNotesDir
			if stepResult != "" {
				loopFrame.iterationLog = append(loopFrame.iterationLog, stepResult)
			}
			agentSendToProgram(AppendHistoryMsg{
				TabID: tabID,
				Text:  loopNoteLine(top.Name, "break", ""),
			})

			stepLog = append(stepLog, loopFrame.iterationLog...)
			loopFrame = nil
			runState.StepIdx++
			continue
		}

		if !isTail {
			prevNotesDir = currentNotesDir
			if stepResult != "" {
				loopFrame.iterationLog = append(loopFrame.iterationLog, stepResult)
			}
			loopFrame.retry = 0
			loopFrame.retryText = ""
			loopFrame.innerIdx++
			continue
		}

		if sig.decision != workflowLoopContinue {
			loopFrame.retry++
			loopFrame.retryText = stepResult
			remind = remindNoDecision
			agentSendToProgram(AppendHistoryMsg{
				TabID: tabID,
				Text:  workflowMarginStyle.Render("|") + " Re-prompting final step for a decision",
			})
			continue
		}

		prevNotesDir = currentNotesDir
		if stepResult != "" {
			loopFrame.iterationLog = append(loopFrame.iterationLog, stepResult)
		}

		if loopFrame.iteration >= top.effectiveMaxIterations() {
			if runState.StepIdx == len(def.Steps)-1 && finishData == nil {
				loopFrame.retry++
				loopFrame.retryText = stepResult
				remind = remindNoFinishTool
				agentSendToProgram(AppendHistoryMsg{
					TabID: tabID,
					Text:  workflowMarginStyle.Render("|") + " Re-prompting final step for finish_workflow",
				})
				continue
			}

			if runState.StepIdx == len(def.Steps)-1 && finishData != nil {
				runState.finishData = finishData
			}

			agentSendToProgram(AppendHistoryMsg{
				TabID: tabID,
				Text:  loopNoteLine(top.Name, "hit iteration limit", fmt.Sprintf("%d iteration(s)", loopFrame.iteration)),
			})

			stepLog = append(stepLog, loopFrame.iterationLog...)
			loopFrame = nil
			runState.StepIdx++
			continue
		}

		agentSendToProgram(AppendHistoryMsg{
			TabID: tabID,
			Text:  loopNoteLine(top.Name, fmt.Sprintf("iteration %d complete → continue", loopFrame.iteration), ""),
		})

		loopFrame.prevTail = lastOf(loopFrame.iterationLog)
		loopFrame.iterationLog = nil
		loopFrame.iteration++
		loopFrame.innerIdx = 0
		loopFrame.retry = 0
		loopFrame.retryText = ""
	}

	if err := removeAllWorkflowPlans(rootCwd, ""); err != nil {
		debugLog("workflow cleanup err: %v", err)
	}

	desc := ""
	var arts []string
	if runState.finishData != nil {
		desc = runState.finishData.Description
		arts = runState.finishData.Artifacts
	}

	agentSendToProgram(WorkflowDoneMsg{
		TabID:       tabID,
		Description: desc,
		Artifacts:   arts,
	})

	workflowTracker().markFinal(rootCwd, src.Key(), def.Name, workflowStatusDone, runState.StepIdx)

	return finalizedPlanReply{
		workflowName: def.Name,
		workflowDone: true,
		outcome:      desc,
	}, nil
}
