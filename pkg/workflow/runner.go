package workflow

import (
	"fmt"
	"strings"
	"time"
)

// StepPromptCtx carries contextual details injected into a step's
// instruction.
type StepPromptCtx struct {
	Loop                *LoopPromptCtx
	IsStartStep         bool
	IsWorkflowFinalStep bool
}

// LoopPromptCtx carries loop metadata for instruction assembly.
type LoopPromptCtx struct {
	Name          string
	MaxIterations int
	ExitCondition string
	IsTail        bool
}

// FinishData captures completion metadata reported at workflow termination.
type FinishData struct {
	Description string   `json:"description"`
	Artifacts   []string `json:"artifacts"`
}

// RunState represents the state of a workflow run.
type RunState struct {
	Workflow     Def
	Source       Source
	StartedAt    time.Time
	StepIdx      int
	Done         bool
	Failed       bool
	FailedReason string
	FinishData   *FinishData
}

// RunnerListener receives progress notifications during workflow execution.
type RunnerListener interface {
	OnWorkflowStarted(tabID int, def Def, src Source)
	OnWorkflowStepStarted(tabID int, stepIdx int, stepName, provider, model string)
	OnWorkflowStepDone(tabID int, stepIdx int, summary string)
	OnWorkflowDone(tabID int, description string, artifacts []string)
	OnWorkflowFailed(tabID int, reason string)
	OnNote(tabID int, text string)
}

// NoopRunnerListener provides a default empty implementation of RunnerListener.
type NoopRunnerListener struct{}

func (NoopRunnerListener) OnWorkflowStarted(int, Def, Source)                     {}
func (NoopRunnerListener) OnWorkflowStepStarted(int, int, string, string, string) {}
func (NoopRunnerListener) OnWorkflowStepDone(int, int, string)                    {}
func (NoopRunnerListener) OnWorkflowDone(int, string, []string)                   {}
func (NoopRunnerListener) OnWorkflowFailed(int, string)                           {}
func (NoopRunnerListener) OnNote(int, string)                                     {}

// BuildStepInstruction assembles the system instruction for one workflow
// step: the author's prompt, the run's reference block, and the end_turn
// contract (plus loop framing when the step sits inside a loop).
//
// It does NOT thread previous step output. The graph does that: a node's
// output arrives as the next node's input, and every step agent runs with
// IncludeContentsNone so it sees that input and its own work rather than
// the full transcript of everything before it.
func BuildStepInstruction(step Step, source Source, pc *StepPromptCtx) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(step.Prompt))
	if ref := source.RefBlock(); ref != "" {
		b.WriteString("\n\n")
		b.WriteString(ref)
	}
	var loop *LoopPromptCtx
	isFinal := false
	if pc != nil {
		loop = pc.Loop
		isFinal = pc.IsWorkflowFinalStep
	}
	b.WriteString("\n\nTo hand structured output to a later step — a plan, a diff, notes — call save_artifact " +
		"with a name, and the later step loads it with load_artifacts. Prefer this over restating large output in your summary." +
		"\n\nAt the start of this step, proactively call the todos tool to lay out your plan for the step. Keep the task list " +
		"updated as you complete tasks. This provides live progress visibility.")
	if isFinal {
		b.WriteString(" You are the FINAL step of this workflow: before you finish, call finish_workflow with a " +
			"description of the outcome and the list of artifacts it produced — every PR, issue, or link the user " +
			"needs. This is how the user learns what the run created.")
	}
	b.WriteString("\n\n")
	b.WriteString(EndTurnInstructionBlock(loop))
	return strings.TrimSpace(b.String())
}

// EndTurnInstructionBlock renders the end_turn contract for a step, and
// inside a loop the iteration framing plus how to break out.
//
// Breaking a loop is ADK's exit_loop tool, not an end_turn argument: the
// tool sets Actions.Escalate, which is what a loopagent watches for.
func EndTurnInstructionBlock(loop *LoopPromptCtx) string {
	var b strings.Builder
	if loop != nil {
		fmt.Fprintf(&b, "[Workflow loop %q · up to %d iterations]", loop.Name, loop.MaxIterations)
		if cond := strings.TrimSpace(loop.ExitCondition); cond != "" {
			b.WriteString("\nLoop exit goal: ")
			b.WriteString(cond)
		}
		b.WriteString("\n")
	}
	b.WriteString("When you have finished this step, you MUST call the end_turn tool as your final action, with " +
		"a `summary` of 1-3 sentences describing what you did and the outcome. This records your progress in the " +
		"workflow log; it does not cut your turn short.")
	if loop != nil {
		if loop.IsTail {
			// Only the tail step is given the exit_loop tool, so only the
			// tail step is told how to break.
			b.WriteString(" You are the last step of this loop iteration: when the loop's exit goal above is met, " +
				"call the exit_loop tool to end the loop. If it is not met, do not call exit_loop — the loop advances " +
				"to its next iteration on its own, and stops by itself after the iteration limit.")
		} else {
			b.WriteString(" You are running inside a loop but you are NOT its last step, so you cannot end the loop — " +
				"only the final step of the iteration decides whether to continue or stop. Do your part and end your " +
				"turn.")
		}
	}
	return b.String()
}

// WorkflowNoteLine formats a single-line status note.
func WorkflowNoteLine(msg, detail string) string {
	res := "     " + msg
	if strings.TrimSpace(detail) != "" {
		res += ": " + strings.TrimSpace(detail)
	}
	return res
}

// LoopNoteLine formats a loop transition note.
func LoopNoteLine(loopName, action, detail string) string {
	return WorkflowNoteLine(fmt.Sprintf("⟳ loop %q %s", loopName, action), detail)
}

// StepSummaryLine formats a step completion summary.
func StepSummaryLine(name, provider, model, summary string) string {
	header := "▸ " + name
	if meta := ProviderMeta(provider, model); meta != "" {
		header += " (" + meta + ")"
	}
	if s := strings.TrimSpace(summary); s != "" {
		header += "\n  " + s
	}
	return header
}

// ProviderMeta formats provider and model strings into a metadata label.
func ProviderMeta(provider, model string) string {
	switch {
	case provider == "":
		return model
	case model == "":
		return provider
	default:
		return provider + "/" + model
	}
}
