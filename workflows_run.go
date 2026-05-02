package main

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// workflowTabHandleKey gates input on a workflow tab. The user
// cannot type a turn — there is no chat input on a workflow tab —
// but they can still scroll the chat viewport to read streaming
// output, copy text, cancel the run (Ctrl+C), or close the tab
// (Ctrl+D). Every other key is silently absorbed so a stray press
// can't bleed into the chain.
func (m model) workflowTabHandleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		// Ctrl+C on a running workflow tab cancels the chain — the
		// proc is killed and the tab flips to the failed banner.
		// Once cancelled (or naturally finished), Ctrl+C is a no-op
		// (the user can read the output; close with Ctrl+D).
		if m.workflowRun != nil && !m.workflowRun.done && !m.workflowRun.failed {
			return m.workflowFinalize(false, "cancelled by user")
		}
		return m, nil
	}
	// Allow viewport navigation keys through so the user can scroll
	// the chat history while the agent runs. Everything else is
	// absorbed.
	switch msg.Code {
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown,
		tea.KeyHome, tea.KeyEnd, 'k', 'j', 'g', 'G':
		var cmd tea.Cmd
		m.chat, cmd = m.chat.Update(msg)
		m.lastContentFP = ""
		return m, cmd
	}
	return m, nil
}

// startWorkflowStep dispatches the step at m.workflowRun.StepIdx.
// Swaps m.provider and m.providerModel for the step, builds the
// step's prompt (including the issue reference and any previous-step
// output), then routes through the standard sendToProvider path so
// the existing proc/spawn/wire plumbing runs unchanged. Sets the
// per-tab skipAllPermissions flag every step start so a workflow
// running across providers never strands on a tool-permission modal.
func (m model) startWorkflowStep() (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		return m, nil
	}
	if r.StepIdx < 0 || r.StepIdx >= len(r.Workflow.Steps) {
		return m.workflowFinalize(true, "")
	}
	step := r.Workflow.Steps[r.StepIdx]
	prov := providerByID(step.Provider)
	if prov == nil {
		return m.workflowFinalize(false, "provider not registered: "+step.Provider)
	}
	// providerByID falls back to the first registered provider on
	// an unknown id, so a workflow step authored against a provider
	// that isn't registered in this build would silently run on the
	// wrong agent. Reject mismatches explicitly so the user sees a
	// clear failure instead of getting a quietly-redirected step.
	if step.Provider != "" && prov.ID() != step.Provider {
		return m.workflowFinalize(false, "provider not registered: "+step.Provider)
	}
	// Reset everything that pins the model to a previous step so the
	// next StartSession runs clean. sessionID/sessionMinted/resumeCwd
	// stay empty — workflow steps are one-shots, no resume.
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
	// Force skip-permissions on for workflow steps; the user opted
	// into the workflow, and a permission modal would freeze the
	// chain (the tab can't surface ask/approval modals — see the
	// askToolRequestMsg/approvalRequestMsg auto-cancel branches).
	m.skipAllPermissions = true
	workflowTracker().markStep(r.Issue.Key(), r.StepIdx)
	prompt := buildWorkflowStepPrompt(step, r.Issue, r.stepLog)
	return m.sendToProvider(prompt)
}

// workflowFinalize records the terminal outcome and stops the chain.
// ok=true marks the run done; ok=false marks failed with `reason`.
// Always kills the proc so a runaway agent can't keep streaming
// after the chain is over. Idempotent: a second call after the run
// already finalised is a no-op.
func (m model) workflowFinalize(ok bool, reason string) (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		return m, nil
	}
	m.killProc()
	if ok {
		r.done = true
		workflowTracker().markFinal(m.cwd, r.Issue.Key(), r.Workflow.Name, workflowStatusDone, r.StepIdx)
		m.appendHistory(outputStyle.Render(promptStyle.Render("✓ workflow complete: " + r.Workflow.Name)))
	} else {
		r.failed = true
		r.failedReason = reason
		workflowTracker().markFinal(m.cwd, r.Issue.Key(), r.Workflow.Name, workflowStatusFailed, r.StepIdx)
		out := "✗ workflow failed: " + r.Workflow.Name
		if reason != "" {
			out += " — " + reason
		}
		m.appendHistory(outputStyle.Render(errStyle.Render(out)))
	}
	return m, nil
}

// advanceWorkflowStep is called when the current step has finished
// (turnCompleteMsg arrived, or an error fired). Rolls the captured
// per-step text into the log, kills the proc so the next step
// starts fresh, increments StepIdx, and either finalises or emits a
// workflowRunStartStepMsg to fire the next step.
func (m model) advanceWorkflowStep(stepErr error) (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		return m, nil
	}
	if txt := strings.TrimSpace(r.currentStep.String()); txt != "" {
		r.stepLog = append(r.stepLog, txt)
	}
	r.currentStep.Reset()
	if stepErr != nil {
		return m.workflowFinalize(false, stepErr.Error())
	}
	m.killProc()
	r.StepIdx++
	if r.StepIdx >= len(r.Workflow.Steps) {
		return m.workflowFinalize(true, "")
	}
	tabID := m.id
	return m, func() tea.Msg { return workflowRunStartStepMsg{tabID: tabID} }
}

// buildWorkflowStepPrompt assembles the user-message text for step N.
// Format:
//
//	<step.Prompt>
//
//	Reference: <owner/repo#N>
//
//	Previous step output:        (only when log is non-empty)
//	<log[0]>
//	---
//	<log[1]>
//	...
//
// Whitespace at the head and tail is trimmed; the body is left as
// the user wrote it.
func buildWorkflowStepPrompt(step workflowStep, issue issueRef, log []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(step.Prompt))
	b.WriteString("\n\n")
	b.WriteString("Reference: ")
	b.WriteString(issue.Display())
	if len(log) > 0 {
		b.WriteString("\n\nPrevious step output:\n")
		for i, entry := range log {
			if i > 0 {
				b.WriteString("\n---\n")
			}
			b.WriteString(strings.TrimSpace(entry))
		}
	}
	return strings.TrimSpace(b.String())
}

// workflowRunHandleStartStep is the tea.Msg dispatcher for
// workflowRunStartStepMsg. The app layer routes the message to the
// right tab via dispatchByTabID; the model handler validates the
// tab id and runs startWorkflowStep.
func (m model) workflowRunHandleStartStep(msg workflowRunStartStepMsg) (tea.Model, tea.Cmd) {
	if msg.tabID != m.id {
		return m, nil
	}
	return m.startWorkflowStep()
}

// workflowRunHandleStepDone is the tea.Msg dispatcher for
// workflowRunStepDoneMsg. Same routing pattern as the start
// dispatcher.
func (m model) workflowRunHandleStepDone(msg workflowRunStepDoneMsg) (tea.Model, tea.Cmd) {
	if msg.tabID != m.id {
		return m, nil
	}
	return m.advanceWorkflowStep(msg.err)
}

// workflowAdvanceCmd produces a tea.Cmd that emits a
// workflowRunStepDoneMsg targeting the matching tab. Used by the
// turnCompleteMsg / providerDoneMsg / providerExitedMsg handlers to
// hand off into the workflow runner without baking the runner into
// every error path.
func workflowAdvanceCmd(tabID int, err error) tea.Cmd {
	return func() tea.Msg { return workflowRunStepDoneMsg{tabID: tabID, err: err} }
}

// workflowAssistantText accumulates assistant text into the
// in-flight step's buffer. Called from the assistantTextMsg handler
// when m.workflowRun != nil so step N+1's prompt can carry the
// "Previous step output:" block.
func (m *model) workflowAssistantText(text string) {
	if m.workflowRun == nil {
		return
	}
	if m.workflowRun.currentStep.Len() > 0 {
		m.workflowRun.currentStep.WriteString("\n")
	}
	m.workflowRun.currentStep.WriteString(text)
}

// errStepError is the canonical error wrapping a step's reported
// failure (msg.res.Result on a providerDoneMsg with IsError set, or
// the underlying error). Pulled into a helper so the wording is
// consistent across the three error paths that converge on
// advanceWorkflowStep.
func errStepError(detail string) error {
	if detail == "" {
		return errors.New("step error")
	}
	return fmt.Errorf("step error: %s", detail)
}
