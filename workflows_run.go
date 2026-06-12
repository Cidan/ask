package main

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
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
	// Enter on a finished supplanted run hands the tab back to the
	// conversation it took over (sidebar tab mode). Dedicated
	// workflow tabs have nothing underneath — Enter stays absorbed.
	if msg.Mod == 0 && msg.Code == tea.KeyEnter {
		if r := m.workflowRun; r != nil && (r.done || r.failed) && r.supplanted != nil {
			return m.restoreSupplantedTab()
		}
		return m, nil
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

// restoreSupplantedTab hands a tab back to the conversation a
// workflow supplanted (sidebar tab mode): the pre-run provider /
// session snapshot is reinstated, the run state is dropped, and the
// input area returns. The step summaries stay in the transcript as a
// permanent record of the run. The proc is already dead — finalize
// killed it — so the next user turn relaunches with --resume on the
// restored session id.
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
	m.busy = false
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

	// Resolve the agent step to dispatch and assemble its prompt context.
	// Every step carries the end_turn contract; inside a loop the context
	// also carries the loop framing injected into the prompt. r.remind is
	// set by advanceWorkflowStep when the prior turn skipped end_turn (or
	// a loop tail omitted its decision) so the re-prompt explains itself.
	step := top
	pc := &stepPromptCtx{remind: r.remind}
	if r.loop != nil {
		step = top.Steps[r.loop.innerIdx]
		pc.loop = &loopPromptCtx{
			name:          top.Name,
			iteration:     r.loop.iteration,
			maxIterations: top.effectiveMaxIterations(),
			exitCondition: top.ExitCondition,
			isTail:        r.loop.innerIdx == len(top.Steps)-1,
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
	// Each turn starts with no registered end_turn report; the step's
	// end_turn call sets it, advanceWorkflowStep consumes it at turn end.
	r.pendingEndTurn = nil
	prompt := buildWorkflowStepPrompt(step, r.Source, r.contextForDispatch(), pc)
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
// (turnCompleteMsg arrived, or an error fired). It reads the step's
// end_turn report (pendingEndTurn — recorded by the end_turn tool during
// the turn; nil if the step never called it), appends the step's summary
// to the visible log, rolls the captured text into the appropriate
// context log, kills the proc, mutates the cursor, and either finalises
// or hands off to startWorkflowStep through a deferred
// workflowRunStartStepMsg (so the next proc spawn happens at a clean
// Update boundary rather than chaining inside this branch).
//
// Every step must call end_turn. A step that ends its turn without it is
// re-prompted ("hammered" until it registers; Ctrl+C is the manual
// escape). Decision table, evaluated against the just-finished step:
//
//	any step      · no end_turn → re-prompt the same step (need a summary)
//	linear        · summary     → record, advance the top-level cursor
//	loop any      · break       → exit loop now, skip rest of the iteration
//	loop non-tail · else        → run the next inner step (same iteration)
//	loop tail     · no decision → re-prompt the tail (need continue/break)
//	loop tail     · continue    → next iteration, or soft-exit on the cap
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
	sig := r.pendingEndTurn
	r.pendingEndTurn = nil
	r.remind = remindNone

	// Linear (non-loop) step.
	if r.loop == nil {
		step := r.Workflow.Steps[r.StepIdx]
		// No end_turn report: re-prompt the same step, feeding its own
		// output back so it doesn't redo the work.
		if sig == nil {
			r.linearRetry++
			r.linearText = txt
			r.remind = remindNoSummary
			m.appendHistory(workflowNoteLine(
				fmt.Sprintf("re-prompting %q for end_turn (attempt %d)", nonEmpty(step.Name, "step"), r.linearRetry+1), ""))
			return m.dispatchOrFinalize()
		}
		// Got the summary: render the step's line, record output, advance.
		m.appendHistory(stepSummaryLine(step.Name, step.Provider, step.Model, sig.summary, m.width))
		if txt != "" {
			r.stepLog = append(r.stepLog, txt)
		}
		r.linearRetry = 0
		r.linearText = ""
		r.StepIdx++
		return m.dispatchOrFinalize()
	}

	// Inside a loop: apply the decision table to the inner step that just
	// finished.
	top := r.Workflow.Steps[r.StepIdx]
	inner := top.Steps[r.loop.innerIdx]
	isTail := r.loop.innerIdx == len(top.Steps)-1

	// No end_turn report from this inner step: re-prompt it in place,
	// feeding its own output back so it doesn't redo the work.
	if sig == nil {
		r.loop.retry++
		r.loop.retryText = txt
		r.remind = remindNoSummary
		m.appendHistory(loopNoteLine(top.Name,
			fmt.Sprintf("re-prompting %q for end_turn (attempt %d)", nonEmpty(inner.Name, "step"), r.loop.retry+1), ""))
		return m.dispatchOrFinalize()
	}

	// We have a summary: render the inner step's line.
	m.appendHistory(stepSummaryLine(inner.Name, inner.Provider, inner.Model, sig.summary, m.width))

	// Break from any inner step exits the loop immediately, skipping the
	// rest of the iteration.
	if sig.decision == workflowLoopBreak {
		if txt != "" {
			r.loop.iterationLog = append(r.loop.iterationLog, txt)
		}
		m.appendHistory(loopNoteLine(top.Name, "break", ""))
		r.exitLoop()
		return m.dispatchOrFinalize()
	}

	// Non-tail step with no break: proceed to the next inner step in the
	// same iteration. (A "continue" and an omitted decision are equivalent
	// here — only the tail's continue advances the iteration.)
	if !isTail {
		if txt != "" {
			r.loop.iterationLog = append(r.loop.iterationLog, txt)
		}
		r.loop.retry = 0
		r.loop.retryText = ""
		r.loop.innerIdx++
		return m.dispatchOrFinalize()
	}

	// Tail summarised but registered no (valid) decision: re-prompt for
	// the decision. No retry cap — Ctrl+C is the escape.
	if sig.decision != workflowLoopContinue {
		r.loop.retry++
		r.loop.retryText = txt
		r.remind = remindNoDecision
		m.appendHistory(loopNoteLine(top.Name,
			fmt.Sprintf("re-prompting final step for a decision (attempt %d)", r.loop.retry+1), ""))
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
		fmt.Sprintf("iteration %d complete → continue", r.loop.iteration), ""))
	r.loop.prevTail = lastOf(r.loop.iterationLog)
	r.loop.iterationLog = nil
	r.loop.iteration++
	r.loop.innerIdx = 0
	r.loop.retry = 0
	r.loop.retryText = ""
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
//     step's output), matching pre-loop behaviour. A re-prompted linear
//     step also sees its own prior output so it doesn't redo the work.
//   - Inside a loop, the linear log is frozen (it only grows on loop
//     exit). The head inner step additionally sees the previous
//     iteration's tail output (so a kick-back reaches the next pass);
//     downstream inner steps see the current iteration's prior outputs.
//   - A re-prompted inner step additionally carries its own prior output
//     so it can register without the work being lost.
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

// handleEndTurnSignal records the current step's end_turn report (the
// summary plus, in a loop, the optional decision) on the run state and
// acks the blocking end_turn tool. The tool only records — the runner
// acts on it at turn end (see advanceWorkflowStep), which is why this
// never changes the cursor. A signal arriving when no workflow step is
// live is answered with a "no effect" note so the agent knows it did
// nothing.
func (m model) handleEndTurnSignal(msg endTurnSignalMsg) (tea.Model, tea.Cmd) {
	if msg.tabID != m.id {
		return m, nil
	}
	r := m.workflowRun
	if r == nil || r.done || r.failed {
		if msg.reply != nil {
			msg.reply <- endTurnReply{
				registered: false,
				note:       "no active workflow step; end_turn has no effect here",
			}
		}
		return m, nil
	}
	r.pendingEndTurn = &endTurnSignal{decision: msg.decision, summary: msg.summary}
	if msg.reply != nil {
		note := "end_turn recorded"
		if msg.decision != "" {
			note += " (decision: " + msg.decision + ")"
		}
		note += ". Finish your turn normally; the workflow acts on it when your turn ends."
		msg.reply <- endTurnReply{registered: true, note: note}
	}
	return m, nil
}

// remindKind says why the current dispatch is a re-prompt, selecting the
// reminder buildWorkflowStepPrompt appends. remindNone is a normal first
// dispatch.
type remindKind int

const (
	remindNone       remindKind = iota
	remindNoSummary             // the prior turn ended without calling end_turn
	remindNoDecision            // a loop tail called end_turn but omitted its decision
)

// stepPromptCtx carries the per-dispatch facts buildWorkflowStepPrompt
// needs beyond the step itself: the loop framing (nil outside a loop) and
// why this dispatch is a re-prompt (remindNone on a normal first dispatch).
type stepPromptCtx struct {
	loop   *loopPromptCtx
	remind remindKind
}

// loopPromptCtx carries the per-dispatch loop facts injected into an
// inner step's prompt by buildWorkflowStepPrompt.
type loopPromptCtx struct {
	name          string
	iteration     int
	maxIterations int
	exitCondition string
	isTail        bool
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
//	<end_turn contract>          (always — every step must call end_turn)
//
//	<re-prompt reminder>         (only when pc.remind != remindNone)
//
// Issue sources produce a single "Reference: <project>#<n>" line; chat
// sources produce a multi-line "Reference (chat transcript):" block. The
// end_turn contract is position-aware (a loop tail must also register a
// decision). Whitespace at the head and tail is trimmed; the body is left
// as the user wrote it.
func buildWorkflowStepPrompt(step workflowStep, source workflowSource, prevOutputs []string, pc *stepPromptCtx) string {
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
	var loop *loopPromptCtx
	remind := remindNone
	if pc != nil {
		loop = pc.loop
		remind = pc.remind
	}
	b.WriteString("\n\n")
	b.WriteString(endTurnInstructionBlock(loop))
	if remind != remindNone {
		b.WriteString("\n\n")
		b.WriteString(endTurnReminder(remind))
	}
	return strings.TrimSpace(b.String())
}

// endTurnInstructionBlock renders the auto-injected end_turn contract for
// a step. Every step is told to call end_turn with a summary; a loop step
// additionally gets the loop framing, and its tail the "you MUST include a
// decision" clause (a non-tail step is told to break only in the
// exceptional case the exit goal is already met).
func endTurnInstructionBlock(loop *loopPromptCtx) string {
	var b strings.Builder
	if loop != nil {
		fmt.Fprintf(&b, "[Workflow loop %q · iteration %d of up to %d]", loop.name, loop.iteration, loop.maxIterations)
		if cond := strings.TrimSpace(loop.exitCondition); cond != "" {
			b.WriteString("\nLoop exit goal: ")
			b.WriteString(cond)
		}
		b.WriteString("\n")
	}
	b.WriteString("When you have finished this step, you MUST call the end_turn tool as your final action, with " +
		"a `summary` of 1-3 sentences describing what you did and the outcome. This records your progress in the " +
		"workflow log; it does not cut your turn short.")
	if loop != nil {
		if loop.isTail {
			b.WriteString(" You are the final step of this loop iteration, so you MUST also pass a `decision`: " +
				"\"continue\" to run another iteration, or \"break\" to end the loop. Use \"break\" only when the " +
				"loop's exit goal is met — breaking should be exceptional.")
		} else {
			b.WriteString(" You are inside a loop but not its final step, so omit `decision` and let the loop " +
				"proceed — unless the exit goal is already met and the remaining steps should be skipped, in which " +
				"case pass `decision=\"break\"` (this should be exceptional).")
		}
	}
	return b.String()
}

// endTurnReminder is appended to a re-prompted step's prompt, explaining
// why it's being asked again without making it redo the work shown above.
func endTurnReminder(k remindKind) string {
	switch k {
	case remindNoDecision:
		return "REMINDER: you called end_turn without a `decision`, which is required for the final step of a " +
			"loop iteration. You have already done the work shown above — do NOT repeat it. Call end_turn again now " +
			"with decision=\"continue\" or decision=\"break\"."
	default: // remindNoSummary
		return "REMINDER: your previous turn ended without calling end_turn. You have already done the work shown " +
			"above — do NOT repeat it. Call the end_turn tool now (see the instructions above for what to include)."
	}
}

// workflowNoteLine renders a dim single-line status note in the workflow
// log (re-prompts, loop transitions). detail is appended after a colon
// when non-empty.
func workflowNoteLine(msg, detail string) string {
	if strings.TrimSpace(detail) != "" {
		msg += ": " + strings.TrimSpace(detail)
	}
	return outputStyle.Render(dimStyle.Render(msg))
}

// loopNoteLine renders a loop-transition note (start / iteration / break /
// limit) so the user watching the workflow tab can follow the loop's
// progress inline.
func loopNoteLine(loopName, action, detail string) string {
	return workflowNoteLine(fmt.Sprintf("⟳ loop %q %s", loopName, action), detail)
}

// stepSummaryLine renders a completed step's entry in the workflow log: a
// styled "▸ name (provider/model)" header with the agent's end_turn
// summary wrapped beneath it. This per-step content is what replaces the
// raw transcript on a workflow tab. width is the tab width; the summary
// wraps to fit with a small indent.
func stepSummaryLine(name, provider, model, summary string, width int) string {
	header := promptStyle.Render("▸ " + nonEmpty(name, "step"))
	if meta := providerMeta(provider, model); meta != "" {
		header += dimStyle.Render(" (" + meta + ")")
	}
	lines := []string{outputStyle.Render(header)}
	if s := strings.TrimSpace(summary); s != "" {
		wrapWidth := width - 4
		if wrapWidth < 20 {
			wrapWidth = 20
		}
		wrapped := lipgloss.NewStyle().Width(wrapWidth).Render(s)
		for _, ln := range strings.Split(wrapped, "\n") {
			lines = append(lines, outputStyle.Render("  "+ln))
		}
	}
	return strings.Join(lines, "\n")
}

// providerMeta renders the "provider/model" suffix shown next to a step
// name, collapsing to just one side when the other is empty.
func providerMeta(provider, model string) string {
	switch {
	case provider == "":
		return model
	case model == "":
		return provider
	default:
		return provider + "/" + model
	}
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
