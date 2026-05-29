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
// output, cancel the run (Ctrl+C), or close the tab (ActionTabClose,
// default Ctrl+D). Every other key is silently absorbed so a stray
// press can't bleed into the chain.
func (m model) workflowTabHandleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if currentKeyMap().Matches(ActionTabClose, msg) {
		return m, closeTabCmd(m.id)
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		// Ctrl+C on a running workflow tab cancels the chain — the
		// proc is killed and the tab flips to the failed banner.
		// Once cancelled (or naturally finished), Ctrl+C is a no-op
		// (the user can read the output; close with Ctrl+D). This is
		// also the manual escape for a loop whose tail step refuses to
		// register a decision (the runner re-prompts it indefinitely).
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

// startWorkflowStep dispatches the agent step the cursor currently
// points at. For the linear portion of the chain that is
// Workflow.Steps[StepIdx]; inside a loop it is the loop's inner step at
// loop.innerIdx. The cursor is advanced by advanceWorkflowStep before
// this fires (via workflowRunStartStepMsg), so this function only reads
// the cursor and dispatches — it never decides what runs next.
//
// Swaps m.provider / m.providerModel for the step, clears session
// state (workflow steps are one-shots, no resume), forces
// skip-permissions on, and routes through the standard sendToProvider
// path so the existing proc/spawn/wire plumbing runs unchanged.
func (m model) startWorkflowStep() (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		return m, nil
	}
	if r.StepIdx < 0 || r.StepIdx >= len(r.Workflow.Steps) {
		return m.workflowFinalize(true, "")
	}
	top := r.Workflow.Steps[r.StepIdx]

	// Enter a loop step the first time the cursor lands on it.
	if top.isLoop() && r.loop == nil {
		if len(top.Steps) == 0 {
			// Defensive: an empty loop is only reachable via the TUI
			// path (which doesn't pre-validate like the MCP path does).
			// Skip it rather than spin on nothing.
			r.StepIdx++
			return m.startWorkflowStep()
		}
		r.loop = &loopRunFrame{innerIdx: 0, iteration: 1}
		m.appendHistory(loopNoteLine(top.Name, "started",
			fmt.Sprintf("max %d iteration(s)", top.effectiveMaxIterations())))
	}

	// Resolve the agent step to dispatch and, inside a loop, the loop
	// context injected into its prompt.
	step := top
	var loopCtx *loopPromptCtx
	if r.loop != nil {
		step = top.Steps[r.loop.innerIdx]
		loopCtx = &loopPromptCtx{
			name:          top.Name,
			iteration:     r.loop.iteration,
			maxIterations: top.effectiveMaxIterations(),
			exitCondition: top.ExitCondition,
			isTail:        r.loop.innerIdx == len(top.Steps)-1,
			remind:        r.loop.retry > 0,
		}
	}

	prov := providerByID(step.Provider)
	if prov == nil {
		return m.workflowFinalize(false, "provider not registered: "+step.Provider)
	}
	// providerByID falls back to the first registered provider on an
	// unknown id, so a step authored against a provider that isn't
	// registered in this build would silently run on the wrong agent.
	// Reject mismatches explicitly so the user sees a clear failure.
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
	// askToolRequestMsg headless reply / approvalRequestMsg auto-deny
	// branches).
	m.skipAllPermissions = true
	workflowTracker().markStep(r.Source.Key(), r.StepIdx)
	// Each turn starts with no registered loop intent; the inner step's
	// workflow_loop call (if any) sets it, advanceWorkflowStep consumes
	// it at turn end.
	r.pendingLoopSignal = nil
	prompt := buildWorkflowStepPrompt(step, r.Source, r.contextForDispatch(), loopCtx)
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
		workflowTracker().markFinal(m.cwd, r.Source.Key(), r.Workflow.Name, workflowStatusDone, r.StepIdx)
		m.appendHistory(outputStyle.Render(promptStyle.Render("✓ workflow complete: " + r.Workflow.Name)))
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

// advanceWorkflowStep is called when the current step has finished
// (turnCompleteMsg arrived, or an error fired). It rolls the captured
// per-step text into the appropriate log, kills the proc so the next
// step starts fresh, mutates the cursor per the loop decision table,
// and either finalises or hands off to startWorkflowStep through a
// deferred workflowRunStartStepMsg (so the next proc spawn happens at a
// clean Update boundary rather than chaining inside this branch).
//
// Loop decision table, evaluated against the just-finished inner step's
// registered intent (pendingLoopSignal — recorded by the workflow_loop
// tool during the turn; nil if the agent registered nothing):
//
//	any inner · break    → exit loop now, skip the rest of the iteration
//	non-tail  · else     → run the next inner step (same iteration)
//	tail      · continue → next iteration, or soft-exit on the cap
//	tail      · none     → re-prompt the tail (keep hammering until it
//	                       registers; Ctrl+C is the manual escape)
func (m model) advanceWorkflowStep(stepErr error) (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		return m, nil
	}
	txt := strings.TrimSpace(r.currentStep.String())
	r.currentStep.Reset()
	if stepErr != nil {
		return m.workflowFinalize(false, stepErr.Error())
	}
	m.killProc()

	// Linear (non-loop) step: record output, advance the top-level
	// cursor, dispatch the next step (which may enter a loop or
	// finalise the run).
	if r.loop == nil {
		if txt != "" {
			r.stepLog = append(r.stepLog, txt)
		}
		r.StepIdx++
		return m.dispatchOrFinalize()
	}

	// Inside a loop: apply the decision table to the inner step that
	// just finished.
	top := r.Workflow.Steps[r.StepIdx]
	isTail := r.loop.innerIdx == len(top.Steps)-1
	sig := r.pendingLoopSignal
	r.pendingLoopSignal = nil

	// Break from any inner step exits the loop immediately, skipping
	// the rest of the iteration.
	if sig != nil && sig.decision == workflowLoopBreak {
		if txt != "" {
			r.loop.iterationLog = append(r.loop.iterationLog, txt)
		}
		m.appendHistory(loopNoteLine(top.Name, "break", sig.reason))
		r.exitLoop()
		return m.dispatchOrFinalize()
	}

	// Non-tail step with no break: proceed to the next inner step in
	// the same iteration. A registered "continue" from a non-tail step
	// is equivalent to silence here — only the tail's continue advances
	// the iteration.
	if !isTail {
		if txt != "" {
			r.loop.iterationLog = append(r.loop.iterationLog, txt)
		}
		r.loop.innerIdx++
		return m.dispatchOrFinalize()
	}

	// Tail with no registered intent: keep hammering. Re-dispatch the
	// tail with a reminder, feeding its own output back so it can decide
	// without redoing the work. No retry cap — Ctrl+C is the escape.
	if sig == nil {
		r.loop.retry++
		r.loop.lastTailText = txt
		m.appendHistory(loopNoteLine(top.Name,
			fmt.Sprintf("re-prompting final step (attempt %d)", r.loop.retry+1),
			"no loop decision registered"))
		return m.dispatchOrFinalize()
	}

	// Tail registered continue: record output, then start the next
	// iteration or soft-exit if the iteration cap is reached.
	if txt != "" {
		r.loop.iterationLog = append(r.loop.iterationLog, txt)
	}
	if r.loop.iteration >= top.effectiveMaxIterations() {
		m.appendHistory(loopNoteLine(top.Name, "hit iteration limit",
			fmt.Sprintf("%d iteration(s)", r.loop.iteration)))
		r.exitLoop()
		return m.dispatchOrFinalize()
	}
	m.appendHistory(loopNoteLine(top.Name,
		fmt.Sprintf("iteration %d → continue", r.loop.iteration), sig.reason))
	r.loop.prevTail = lastOf(r.loop.iterationLog)
	r.loop.iterationLog = nil
	r.loop.iteration++
	r.loop.innerIdx = 0
	r.loop.retry = 0
	r.loop.lastTailText = ""
	return m.dispatchOrFinalize()
}

// dispatchOrFinalize is the shared tail of advanceWorkflowStep: when
// the linear cursor has run off the end of the chain the run is done;
// otherwise hand off to startWorkflowStep via a deferred message. While
// inside a loop the cursor is never "past the end" (StepIdx points at
// the loop step), so this always defers in that case.
func (m model) dispatchOrFinalize() (tea.Model, tea.Cmd) {
	r := m.workflowRun
	if r.loop == nil && r.StepIdx >= len(r.Workflow.Steps) {
		return m.workflowFinalize(true, "")
	}
	return m, workflowStartStepCmd(m.id)
}

// workflowStartStepCmd produces a tea.Cmd that re-enters the runner for
// the current cursor on a fresh Update cycle.
func workflowStartStepCmd(tabID int) tea.Cmd {
	return func() tea.Msg { return workflowRunStartStepMsg{tabID: tabID} }
}

// contextForDispatch returns the "Previous step output" slice for the
// step about to run, implementing the bounded-context policy:
//
//   - Linear steps see the full linear log (every completed top-level
//     step's output), matching pre-loop behaviour.
//   - Inside a loop, the linear log is frozen (it only grows on loop
//     exit). The head inner step additionally sees the previous
//     iteration's tail output (so a kick-back reaches the next pass);
//     downstream inner steps see the current iteration's prior outputs.
//   - A tail re-prompt additionally carries the tail's own prior output
//     so it can decide without the work being lost.
func (r *workflowRunState) contextForDispatch() []string {
	if r.loop == nil {
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
	if r.loop.retry > 0 && r.loop.lastTailText != "" {
		ctx = append(ctx, r.loop.lastTailText)
	}
	return ctx
}

// exitLoop commits the loop's final iteration outputs to the linear log
// (so the step after the loop sees the last pass) and clears the frame,
// advancing the top-level cursor past the loop step.
func (r *workflowRunState) exitLoop() {
	if r.loop != nil {
		r.stepLog = append(r.stepLog, r.loop.iterationLog...)
	}
	r.loop = nil
	r.StepIdx++
}

// handleWorkflowLoopSignal records an inner step's break/continue
// intent on the run state and acks the blocking workflow_loop tool. The
// tool only registers intent — the runner acts on it at turn end (see
// advanceWorkflowStep), which is why this never changes the cursor. A
// signal arriving when the tab isn't inside a loop is answered with a
// "no effect" note so the agent knows it did nothing.
func (m model) handleWorkflowLoopSignal(msg workflowLoopSignalMsg) (tea.Model, tea.Cmd) {
	if msg.tabID != m.id {
		return m, nil
	}
	r := m.workflowRun
	if r == nil || r.done || r.failed || r.loop == nil {
		if msg.reply != nil {
			msg.reply <- workflowLoopReply{
				registered: false,
				note:       "not currently inside a workflow loop; workflow_loop has no effect here",
			}
		}
		return m, nil
	}
	r.pendingLoopSignal = &loopSignal{decision: msg.decision, reason: msg.reason}
	if msg.reply != nil {
		note := "loop intent registered: " + msg.decision
		if msg.reason != "" {
			note += " — " + msg.reason
		}
		note += ". Finish your turn normally; the loop acts on it when your turn ends."
		msg.reply <- workflowLoopReply{registered: true, note: note}
	}
	return m, nil
}

// loopPromptCtx carries the per-dispatch loop facts injected into an
// inner step's prompt by buildWorkflowStepPrompt.
type loopPromptCtx struct {
	name          string
	iteration     int
	maxIterations int
	exitCondition string
	isTail        bool
	remind        bool
}

// buildWorkflowStepPrompt assembles the user-message text for one step.
// Format:
//
//	<step.Prompt>
//
//	<source.RefBlock()>          (skipped when RefBlock is empty)
//
//	Previous step output:        (only when prevOutputs is non-empty)
//	<prevOutputs[0]>
//	---
//	<prevOutputs[1]>
//	...
//
//	<loop control instructions>  (only when loop != nil)
//
//	<retry reminder>             (only when loop.remind)
//
// Issue sources produce a single "Reference: <project>#<n>" line; chat
// sources produce a multi-line "Reference (chat transcript):" block.
// loop==nil reproduces the pre-loop prompt byte-for-byte. Whitespace at
// the head and tail is trimmed; the body is left as the user wrote it.
func buildWorkflowStepPrompt(step workflowStep, source workflowSource, prevOutputs []string, loop *loopPromptCtx) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(step.Prompt))
	if ref := source.RefBlock(); ref != "" {
		b.WriteString("\n\n")
		b.WriteString(ref)
	}
	if len(prevOutputs) > 0 {
		b.WriteString("\n\nPrevious step output:\n")
		for i, entry := range prevOutputs {
			if i > 0 {
				b.WriteString("\n---\n")
			}
			b.WriteString(strings.TrimSpace(entry))
		}
	}
	if loop != nil {
		b.WriteString("\n\n")
		b.WriteString(loopInstructionBlock(loop))
		if loop.remind {
			b.WriteString("\n\n")
			b.WriteString(workflowLoopRetryReminder)
		}
	}
	return strings.TrimSpace(b.String())
}

// workflowLoopRetryReminder is appended to a tail step's re-prompt after
// it finished without registering a loop decision.
const workflowLoopRetryReminder = "REMINDER: your previous turn ended without registering a loop decision. " +
	"You have already done the work shown above — do NOT repeat it. " +
	"Call the workflow_loop tool now with decision=\"break\" or decision=\"continue\" and a short reason."

// loopInstructionBlock renders the auto-injected loop-control guidance
// for an inner step. The tail step gets an extra "you must decide"
// clause so the agent knows silence will cost it a re-prompt.
func loopInstructionBlock(c *loopPromptCtx) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Workflow loop %q · iteration %d of up to %d]", c.name, c.iteration, c.maxIterations)
	if cond := strings.TrimSpace(c.exitCondition); cond != "" {
		b.WriteString("\nLoop exit goal: ")
		b.WriteString(cond)
	}
	b.WriteString("\nYou are running inside this loop. Control it with the workflow_loop tool: " +
		"call decision=\"break\" when the exit goal is met (end the loop), or decision=\"continue\" to run " +
		"another iteration. Always include a short reason. Registering a decision does NOT end your turn — " +
		"finish your work normally; the loop acts on your most recent decision when your turn completes.")
	if c.isTail {
		b.WriteString("\nYou are the final step of this iteration: you MUST register a decision with " +
			"workflow_loop before finishing, or you will be asked again.")
	}
	return b.String()
}

// loopNoteLine renders a dim, single-line history note marking a loop
// transition (start / iteration / break / limit) so the user watching
// the workflow tab can follow the loop's progress inline.
func loopNoteLine(loopName, action, detail string) string {
	msg := fmt.Sprintf("⟳ loop %q %s", loopName, action)
	if strings.TrimSpace(detail) != "" {
		msg += ": " + strings.TrimSpace(detail)
	}
	return outputStyle.Render(dimStyle.Render(msg))
}

// lastOf returns the final element of s, or "" when empty.
func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
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
// when m.workflowRun != nil so the next step's prompt can carry the
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
