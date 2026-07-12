package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// This file implements the legacy, UI-driven workflow runner methods on model
// exclusively for the test suite in workflows_run_test.go. These methods are
// excluded from the production build.

func (m model) startWorkflowStep() (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		return m, nil
	}
	if r.StepIdx < 0 || r.StepIdx >= len(r.Workflow.Steps) {
		return m.workflowFinalize(true, "")
	}
	top := r.Workflow.Steps[r.StepIdx]

	if top.isLoop() && r.loop == nil {
		if len(top.Steps) == 0 {
			r.StepIdx++
			return m.startWorkflowStep()
		}
		r.loop = &loopRunFrame{innerIdx: 0, iteration: 1}
		m.appendHistory(loopNoteLine(top.Name, "started",
			fmt.Sprintf("max %d iteration(s)", top.effectiveMaxIterations())))
	}

	step := top
	if r.loop != nil {
		step = top.Steps[r.loop.innerIdx]
	}
	isStartStep := r.StepIdx == 0 && r.loop == nil
	isLoopStartStep := r.StepIdx == 0 && r.loop != nil && r.loop.iteration == 1 && r.loop.innerIdx == 0
	var notesDir string
	switch {
	case isStartStep, isLoopStartStep:
		notesDir = startPlanDir(m.cwd, m.worktreeName)
	case r.loop != nil:
		notesDir = stepNotesDir(m.cwd, m.worktreeName, step.Name, top.Name, r.loop.iteration)
	default:
		notesDir = stepNotesDir(m.cwd, m.worktreeName, step.Name, "", 0)
	}
	r.currentNotesDir = notesDir
	pc := &stepPromptCtx{
		remind:              r.remind,
		remindDetail:        r.remindDetail,
		notesDir:            notesDir,
		prevNotesDir:        r.prevNotesDir,
		isStartStep:         isStartStep || isLoopStartStep,
		isWorkflowFinalStep: r.StepIdx == len(r.Workflow.Steps)-1,
	}
	if r.loop != nil {
		pc.loop = &loopPromptCtx{
			name:          top.Name,
			iteration:     r.loop.iteration,
			maxIterations: top.effectiveMaxIterations(),
			exitCondition: top.ExitCondition,
			isTail:        r.loop.innerIdx == len(top.Steps)-1,
		}
	}

	if pc.isStartStep {
		if err := ensureStartPlanExists(m.cwd, m.worktreeName); err != nil {
			r.remind = remindFixPlanDir
			r.remindDetail = err.Error()
			return m.sendToProviderLegacy(buildWorkflowStepPrompt(step, r.Source, r.contextForDispatch(), pc))
		}
	} else if err := ensureStepNotesDir(notesDir); err != nil {
		r.remind = remindFixPlanDir
		r.remindDetail = err.Error()
		return m.sendToProviderLegacy(buildWorkflowStepPrompt(step, r.Source, r.contextForDispatch(), pc))
	}

	prov := providerByID(step.Provider)
	if prov == nil {
		return m.workflowFinalize(false, "provider not registered: "+step.Provider)
	}
	if step.Provider != "" && prov.ID() != step.Provider {
		return m.workflowFinalize(false, "provider not registered: "+step.Provider)
	}
	m.provider = prov
	m.providerModel = step.Model
	settings := prov.LoadSettings()
	if m.providerModel == "" {
		m.providerModel = settings.Model
	}
	m.providerEffort = settings.Effort
	m.providerSlashCmds = settings.SlashCommands
	m.sessionID = ""
	m.sessionMinted = false
	m.resumeCwd = ""
	m.todos = nil
	m.skipAllPermissions = true
	workflowTracker().markStep(r.Source.Key(), r.StepIdx)
	r.pendingEndTurn = nil
	prompt := buildWorkflowStepPrompt(step, r.Source, r.contextForDispatch(), pc)
	return m.sendToProviderLegacy(prompt)
}

func (m model) sendToProviderLegacy(prompt string) (tea.Model, tea.Cmd) {
	m.testBusy = true
	return m.sendToProvider(prompt)
}

func (m model) workflowFinalize(ok bool, reason string) (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		return m, nil
	}
	m.killProc()
	if ok {
		r.done = true
		workflowTracker().markFinal(m.cwd, r.Source.Key(), r.Workflow.Name, workflowStatusDone, r.StepIdx)
		m.appendHistory(outputStyle.Render(promptStyle.Render("✓ workflow complete: " + r.Workflow.Name)))
		if err := removeAllWorkflowPlans(m.cwd, m.worktreeName); err != nil {
			debugLog("workflow cleanup: %v", err)
		}
	} else {
		r.failed = true
		r.failedReason = reason
		workflowTracker().markFinal(m.cwd, r.Source.Key(), r.Workflow.Name, workflowStatusFailed, r.StepIdx)
		out := "✗ workflow failed: " + r.Workflow.Name
		if reason != "" {
			out += " — " + reason
		}
		m.appendHistory(outputStyle.Render(errStyle.Render(out)))
	}
	return m, nil
}

func (m model) advanceWorkflowStep(stepErr error) (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		return m, nil
	}
	txt := strings.TrimSpace(r.currentStep.String())
	r.currentStep.Reset()
	if stepErr != nil {
		m.killProc()
		cfg, _ := loadConfig()
		maxRetries, initialDelay, backoffFactor := agentRetryOptions(cfg)

		if r.stepErrorRetry < maxRetries {
			r.stepErrorRetry++
			wait := time.Duration(float64(initialDelay) * math.Pow(backoffFactor, float64(r.stepErrorRetry-1)))

			stepName, _, _ := currentWorkflowStepMeta(r)
			var note string
			if r.loop != nil {
				note = loopNoteLine(r.Workflow.Steps[r.StepIdx].Name, "failed", fmt.Sprintf("step %q: %v — retrying in %s (attempt %d of %d)", stepName, stepErr, humanDuration(wait), r.stepErrorRetry, maxRetries))
			} else {
				note = workflowNoteLine(fmt.Sprintf("step %q failed: %v", stepName, stepErr), fmt.Sprintf("retrying in %s (attempt %d of %d)", humanDuration(wait), r.stepErrorRetry, maxRetries))
			}
			m.appendHistory(note)

			return m, tea.Tick(wait, func(t time.Time) tea.Msg {
				return workflowRunStartStepMsg{tabID: m.id}
			})
		}
		return m.workflowFinalize(false, stepErr.Error())
	}
	r.stepErrorRetry = 0
	m.killProc()
	sig := r.pendingEndTurn
	r.pendingEndTurn = nil
	r.remind = remindNone
	r.remindDetail = ""

	if r.loop == nil {
		step := r.Workflow.Steps[r.StepIdx]
		if sig == nil {
			r.linearRetry++
			r.linearText = txt
			r.remind = remindNoSummary
			m.appendHistory(workflowNoteLine(
				fmt.Sprintf("re-prompting %q for end_turn (attempt %d)", nonEmpty(step.Name, "step"), r.linearRetry+1), ""))
			return m.dispatchOrFinalize()
		}
		m.appendWorkflowStepDone(step.Name, step.Provider, step.Model, sig.summary)
		if r.StepIdx == len(r.Workflow.Steps)-1 && r.finishData == nil {
			r.linearRetry++
			r.linearText = txt
			r.remind = remindNoFinishTool
			m.appendHistory(workflowNoteLine(
				fmt.Sprintf("re-prompting %q for finish_workflow (attempt %d)", nonEmpty(step.Name, "step"), r.linearRetry+1), ""))
			return m.dispatchOrFinalize()
		}
		if err := revalidateWorkflowNotesDirLegacy(m.cwd, m.worktreeName, r); err != nil {
			r.remind = remindFixPlanDir
			r.remindDetail = err.Error()
			return m.dispatchOrFinalize()
		}
		r.prevNotesDir = r.currentNotesDir
		if txt != "" {
			r.stepLog = append(r.stepLog, txt)
		}
		r.linearRetry = 0
		r.linearText = ""
		r.StepIdx++
		return m.dispatchOrFinalize()
	}

	top := r.Workflow.Steps[r.StepIdx]
	inner := top.Steps[r.loop.innerIdx]
	isTail := r.loop.innerIdx == len(top.Steps)-1

	if sig == nil {
		r.loop.retry++
		r.loop.retryText = txt
		r.remind = remindNoSummary
		m.appendHistory(loopNoteLine(top.Name,
			fmt.Sprintf("re-prompting %q for end_turn (attempt %d)", nonEmpty(inner.Name, "step"), r.loop.retry+1), ""))
		return m.dispatchOrFinalize()
	}

	m.appendWorkflowStepDone(inner.Name, inner.Provider, inner.Model, sig.summary)

	if isTail && sig.decision == workflowLoopBreak {
		if r.StepIdx == len(r.Workflow.Steps)-1 && r.finishData == nil {
			r.loop.retry++
			r.loop.retryText = txt
			r.remind = remindNoFinishTool
			m.appendHistory(loopNoteLine(top.Name,
				fmt.Sprintf("re-prompting %q for finish_workflow (attempt %d)", nonEmpty(inner.Name, "step"), r.loop.retry+1), ""))
			return m.dispatchOrFinalize()
		}
		r.prevNotesDir = r.currentNotesDir
		if txt != "" {
			r.loop.iterationLog = append(r.loop.iterationLog, txt)
		}
		m.appendHistory(loopNoteLine(top.Name, "break", ""))
		r.exitLoop()
		return m.dispatchOrFinalize()
	}
	if !isTail && sig.decision == workflowLoopBreak {
		m.appendHistory(loopNoteLine(top.Name, "break rejected",
			"only the final step of a loop iteration can break the loop"))
	}

	if !isTail {
		if err := revalidateWorkflowNotesDirLegacy(m.cwd, m.worktreeName, r); err != nil {
			r.remind = remindFixPlanDir
			r.remindDetail = err.Error()
			return m.dispatchOrFinalize()
		}
		r.prevNotesDir = r.currentNotesDir
		if txt != "" {
			r.loop.iterationLog = append(r.loop.iterationLog, txt)
		}
		r.loop.retry = 0
		r.loop.retryText = ""
		r.loop.innerIdx++
		return m.dispatchOrFinalize()
	}

	if sig.decision != workflowLoopContinue {
		r.loop.retry++
		r.loop.retryText = txt
		r.remind = remindNoDecision
		m.appendHistory(loopNoteLine(top.Name,
			fmt.Sprintf("re-prompting final step for a decision (attempt %d)", r.loop.retry+1), ""))
		return m.dispatchOrFinalize()
	}

	if err := revalidateWorkflowNotesDirLegacy(m.cwd, m.worktreeName, r); err != nil {
		r.remind = remindFixPlanDir
		r.remindDetail = err.Error()
		return m.dispatchOrFinalize()
	}
	r.prevNotesDir = r.currentNotesDir
	if txt != "" {
		r.loop.iterationLog = append(r.loop.iterationLog, txt)
	}
	if r.loop.iteration >= top.effectiveMaxIterations() {
		if r.StepIdx == len(r.Workflow.Steps)-1 && r.finishData == nil {
			r.loop.retry++
			r.loop.retryText = txt
			r.remind = remindNoFinishTool
			m.appendHistory(loopNoteLine(top.Name,
				fmt.Sprintf("re-prompting %q for finish_workflow (attempt %d)", nonEmpty(inner.Name, "step"), r.loop.retry+1), ""))
			return m.dispatchOrFinalize()
		}
		m.appendHistory(loopNoteLine(top.Name, "hit iteration limit",
			fmt.Sprintf("%d iteration(s)", r.loop.iteration)))
		r.exitLoop()
		return m.dispatchOrFinalize()
	}
	m.appendHistory(loopNoteLine(top.Name,
		fmt.Sprintf("iteration %d complete → continue", r.loop.iteration), ""))
	r.loop.prevTail = lastOf(r.loop.iterationLog)
	r.loop.iterationLog = nil
	r.loop.iteration++
	r.loop.innerIdx = 0
	r.loop.retry = 0
	r.loop.retryText = ""
	return m.dispatchOrFinalize()
}

func revalidateWorkflowNotesDirLegacy(cwd, worktreeName string, r *workflowRunState) error {
	if r.currentNotesDir == "" {
		return nil
	}
	if r.currentNotesDir == startPlanDir(cwd, worktreeName) {
		return ensureStartPlanExists(cwd, worktreeName)
	}
	return ensureStepNotesDir(r.currentNotesDir)
}

func (m model) dispatchOrFinalize() (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r.loop == nil && r.StepIdx >= len(r.Workflow.Steps) {
		return m.workflowFinalize(true, "")
	}
	return m, func() tea.Msg { return workflowRunStartStepMsg{tabID: m.id} }
}

func (r *workflowRunState) contextForDispatch() []string {
	if r.loop == nil {
		if r.linearRetry > 0 && r.linearText != "" {
			return append(append([]string(nil), r.stepLog...), r.linearText)
		}
		return r.stepLog
	}
	ctx := append([]string(nil), r.stepLog...)
	if r.loop.innerIdx == 0 {
		if r.loop.prevTail != "" {
			ctx = append(ctx, r.loop.prevTail)
		}
	} else {
		ctx = append(ctx, r.loop.iterationLog...)
	}
	if r.loop.retry > 0 && r.loop.retryText != "" {
		ctx = append(ctx, r.loop.retryText)
	}
	return ctx
}

func (r *workflowRunState) exitLoop() {
	if r.loop != nil {
		r.stepLog = append(r.stepLog, r.loop.iterationLog...)
	}
	r.loop = nil
	r.StepIdx++
}
