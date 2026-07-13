package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func currentWorkflowStepMeta(r *workflowRunState) (name, provider, model string) {
	if r == nil || r.StepIdx < 0 || r.StepIdx >= len(r.Workflow.Steps) {
		return "", "", ""
	}
	top := r.Workflow.Steps[r.StepIdx]
	if r.loop != nil && top.isLoop() && r.loop.innerIdx < len(top.Steps) {
		inner := top.Steps[r.loop.innerIdx]
		return inner.Name, inner.Provider, inner.Model
	}
	return top.Name, top.Provider, top.Model
}

// workflowTabHandleKey gates input on a workflow tab. The user
// cannot type a turn — there is no chat input on a workflow tab —
// but they can still scroll the chat viewport to read streaming
// output, cancel the run (Ctrl+C), or close the tab (ActionTabClose,
// default Ctrl+D). Every other key is silently absorbed so a stray
// press can't bleed into the chain.
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
	// Enter on a finished supplanted run hands the tab back to the
	// conversation it took over. Dedicated workflow tabs have nothing
	// underneath — Enter closes them.
	if msg.Mod == 0 && msg.Code == tea.KeyEnter {
		if r := m.workflowRun; r != nil && (r.done || r.failed) {
			if r.supplanted != nil {
				return m.restoreSupplantedTab()
			}
			return m, closeTabCmd(m.id)
		}
		return m, nil
	}
	if msg.Mod == 0 && msg.Code == 'r' {
		if r := m.workflowRun; r != nil && r.failed {
			r.failed = false
			r.failedReason = ""
			workflowTracker().markWorking(m.cwd, r.Source.Key(), r.Workflow.Name, m.id)
			// Trigger rerun via Coordinator
			runWF := func() tea.Msg {
				go func() {
					_, _ = globalCoordinator.RunWorkflow(context.Background(), m.id, r.Workflow, r.Source)
				}()
				return nil
			}
			return m, runWF
		}
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		if m.workflowRun != nil && !m.workflowRun.done && !m.workflowRun.failed {
			globalCoordinator.CancelWorkflow(m.id)
			return m, nil
		}
		return m, nil
	}
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
// workflow supplanted: the pre-run provider /
// session snapshot is reinstated, the run state is dropped, and the
// input area returns. The step summaries stay in the transcript as a
// permanent record of the run.
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

func (m *model) appendWorkflowStepDone(name, provider, mdl, summary string) {
	m.history = append(m.history, historyEntry{
		kind:           histWorkflowDone,
		text:           summary,
		workflowHeader: "",
	})
}

// stepPromptCtx carries the per-dispatch facts buildWorkflowStepPrompt
// needs beyond the step itself: the loop framing (nil outside a loop),
// why this dispatch is a re-prompt (remindNone on a normal first dispatch),
// and the current/previous notes directories.
type stepPromptCtx struct {
	loop                *loopPromptCtx
	remind              remindKind
	remindDetail        string
	notesDir            string
	prevNotesDir        string
	isStartStep         bool
	isWorkflowFinalStep bool
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
	if pc != nil && pc.notesDir != "" {
		b.WriteString("\n\n")
		b.WriteString("Workflow notes directories:\n")
		b.WriteString("- Your notes directory: " + pc.notesDir)
		if pc.prevNotesDir != "" {
			b.WriteString("\n- Previous step's notes directory: " + pc.prevNotesDir)
		}
		if pc.isStartStep {
			b.WriteString("\n\nThis is the first step. Your notes directory (")
			b.WriteString(pc.notesDir)
			b.WriteString(") MUST be a directory, not a file. Create it if it does not exist, then write one or more files inside it (for example ")
			b.WriteString(filepath.Join(pc.notesDir, "plan.md"))
			b.WriteString("). Do NOT write a single file named \"start\". The workflow runner verifies the directory exists and contains files before step 1; if it is missing, empty, or a file, this step will be re-prompted to fix the directory before any work is done.")
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
		b.WriteString(endTurnReminder(remind, pc.remindDetail))
	}
	return strings.TrimSpace(b.String())
}

// endTurnInstructionBlock renders the auto-injected end_turn contract for
// a step. Every step is told to call end_turn with a summary; a loop step
// additionally gets the loop framing, and its tail the "you MUST include a
// decision" clause. A non-tail step is told it MUST OMIT `decision`
// entirely — only the final step of a loop iteration can pass
// `decision="break"`, and a non-final step's break is silently ignored.
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
			b.WriteString(" You are inside a loop but not its final step of this loop iteration, so you MUST OMIT `decision` " +
				"entirely — only the final step of a loop iteration can pass `decision='break'`. If the loop's " +
				"exit goal appears met, the final step of this iteration will register `break` on its turn.")
		}
	}
	return b.String()
}

// endTurnReminder is appended to a re-prompted step's prompt, explaining
// why it's being asked again without making it redo the work shown above.
func endTurnReminder(k remindKind, detail string) string {
	switch k {
	case remindNoDecision:
		return "REMINDER: you called end_turn without a `decision`, which is required for the final step of a " +
			"loop iteration. You have already done the work shown above — do NOT repeat it. Call end_turn again now " +
			"with decision=\"continue\" or decision=\"break\"."
	case remindFixPlanDir:
		msg := "REMINDER: the workflow notes directory is not usable"
		if detail != "" {
			msg += ": " + detail
		}
		msg += ". You must make it a directory containing files, then call end_turn."
		return msg
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
// summary beneath it. This per-step content is what replaces the raw
// transcript on a workflow tab. The summary is not pre-wrapped; the
// viewport soft-wraps it at render time.
func stepSummaryLine(name, provider, model, summary string) string {
	header := promptStyle.Render("▸ " + nonEmpty(name, "step"))
	if meta := providerMeta(provider, model); meta != "" {
		header += dimStyle.Render(" (" + meta + ")")
	}
	if s := strings.TrimSpace(summary); s != "" {
		header += "\n" + outputStyle.Render("  " + s)
	}
	return header
}


