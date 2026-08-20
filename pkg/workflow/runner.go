package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/exitlooptool"
)

// Loop decision constants.
const (
	LoopContinue = "continue"
	LoopBreak    = "break"
)

// RemindKind identifies the reason a step is being re-prompted.
type RemindKind int

const (
	RemindNone RemindKind = iota
	RemindNoSummary
	RemindNoDecision
	RemindFixPlanDir
	RemindNoFinishTool
)

// StepPromptCtx carries contextual details injected into a step prompt.
type StepPromptCtx struct {
	Loop                *LoopPromptCtx
	Remind              RemindKind
	RemindDetail        string
	NotesDir            string
	PrevNotesDir        string
	IsStartStep         bool
	IsWorkflowFinalStep bool
}

// LoopPromptCtx carries loop iteration metadata for prompt assembly.
type LoopPromptCtx struct {
	Name          string
	Iteration     int
	MaxIterations int
	ExitCondition string
	IsTail        bool
}

// LoopRunFrame tracks execution progress within a loop step.
type LoopRunFrame struct {
	InnerIdx     int
	Iteration    int
	IterationLog []string
	PrevTail     string
	Retry        int
	RetryText    string
}

// FinishData captures completion metadata reported at workflow termination.
type FinishData struct {
	Description string   `json:"description"`
	Artifacts   []string `json:"artifacts"`
}

// RunState represents the state of a running workflow.
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

// StepResult represents the outcome of executing a single workflow step.
type StepResult struct {
	Output     string
	Summary    string
	Decision   string
	FinishData *FinishData
	Error      error
}

// StepExecutor executes a single step turn against an underlying agent engine/provider.
type StepExecutor interface {
	ExecuteStep(ctx context.Context, cwd string, tabID int, step Step, prompt string, isFinal bool) (StepResult, error)
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

// WorkflowAgentConfig configures the construction of an ADK workflow agent hierarchy.
type WorkflowAgentConfig struct {
	Def                Def
	Source             Source
	Cwd                string
	TabID              int
	ModelBuilder       func(ctx context.Context, step Step) (model.LLM, error)
	ToolsBuilder       func(ctx context.Context, step Step, isLoop bool) ([]tool.Tool, error)
	ToolsetsBuilder    func(ctx context.Context, step Step, isLoop bool) ([]tool.Toolset, error)
	InstructionBuilder func(step Step, isStart bool, isFinal bool, loopCtx *LoopPromptCtx, notesDir, prevNotesDir string) string
}

// BuildWorkflowAgent constructs an ADK agent hierarchy conforming to the workflow definition.
// Top-level linear steps are chained using sequentialagent, while kind: "loop" steps are
// encapsulated in loopagent containers with exitlooptool attached to their sub-agents.
func BuildWorkflowAgent(ctx context.Context, cfg WorkflowAgentConfig) (agent.Agent, error) {
	if err := cfg.Def.Validate(); err != nil {
		return nil, err
	}
	if cfg.ModelBuilder == nil {
		return nil, errors.New("model builder is required")
	}

	var topAgents []agent.Agent
	var prevNotesDir string

	for i, top := range cfg.Def.Steps {
		isFinalStep := i == len(cfg.Def.Steps)-1

		if top.Kind == "loop" {
			if len(top.Steps) == 0 {
				continue
			}
			var innerAgents []agent.Agent
			for innerIdx, innerStep := range top.Steps {
				isLoopStart := i == 0 && innerIdx == 0
				var notesDir string
				if isLoopStart {
					notesDir = StartPlanDir(cfg.Cwd)
				} else {
					notesDir = StepNotesDir(cfg.Cwd, innerStep.Name, top.Name, 1)
				}

				llm, err := cfg.ModelBuilder(ctx, innerStep)
				if err != nil {
					return nil, fmt.Errorf("failed to build model for step %q: %w", innerStep.Name, err)
				}

				var tools []tool.Tool
				if cfg.ToolsBuilder != nil {
					builtTools, err := cfg.ToolsBuilder(ctx, innerStep, true)
					if err != nil {
						return nil, fmt.Errorf("failed to build tools for step %q: %w", innerStep.Name, err)
					}
					tools = append(tools, builtTools...)
				}

				// Attach ADK's native exitlooptool for clean early break out of loop containers
				exitTool, err := exitlooptool.New()
				if err != nil {
					return nil, fmt.Errorf("failed to create exitloop tool: %w", err)
				}
				hasExitTool := false
				for _, t := range tools {
					if t != nil && t.Name() == exitTool.Name() {
						hasExitTool = true
						break
					}
				}
				if !hasExitTool {
					tools = append(tools, exitTool)
				}

				var toolsets []tool.Toolset
				if cfg.ToolsetsBuilder != nil {
					ts, err := cfg.ToolsetsBuilder(ctx, innerStep, true)
					if err != nil {
						return nil, fmt.Errorf("failed to build toolsets for step %q: %w", innerStep.Name, err)
					}
					toolsets = ts
				}

				instruction := innerStep.Prompt
				if cfg.InstructionBuilder != nil {
					loopCtx := &LoopPromptCtx{
						Name:          top.Name,
						Iteration:     1,
						MaxIterations: cfg.Def.EffectiveMaxIterations(top),
						ExitCondition: top.ExitCondition,
						IsTail:        innerIdx == len(top.Steps)-1,
					}
					instruction = cfg.InstructionBuilder(innerStep, isLoopStart, isFinalStep, loopCtx, notesDir, prevNotesDir)
				}

				innerAg, err := llmagent.New(llmagent.Config{
					Name:        innerStep.Name,
					Description: innerStep.Prompt,
					Model:       llm,
					Instruction: instruction,
					Tools:       tools,
					Toolsets:    toolsets,
				})
				if err != nil {
					return nil, fmt.Errorf("failed to create inner step agent %q: %w", innerStep.Name, err)
				}
				innerAgents = append(innerAgents, innerAg)
				prevNotesDir = notesDir
			}

			loopAg, err := loopagent.New(loopagent.Config{
				AgentConfig: agent.Config{
					Name:        top.Name,
					Description: top.ExitCondition,
					SubAgents:   innerAgents,
				},
				MaxIterations: uint(cfg.Def.EffectiveMaxIterations(top)),
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create loop agent %q: %w", top.Name, err)
			}
			topAgents = append(topAgents, loopAg)
			continue
		}

		// Linear step
		isStart := i == 0
		var notesDir string
		if isStart {
			notesDir = StartPlanDir(cfg.Cwd)
		} else {
			notesDir = StepNotesDir(cfg.Cwd, top.Name, "", 0)
		}

		llm, err := cfg.ModelBuilder(ctx, top)
		if err != nil {
			return nil, fmt.Errorf("failed to build model for step %q: %w", top.Name, err)
		}

		var tools []tool.Tool
		if cfg.ToolsBuilder != nil {
			builtTools, err := cfg.ToolsBuilder(ctx, top, false)
			if err != nil {
				return nil, fmt.Errorf("failed to build tools for step %q: %w", top.Name, err)
			}
			tools = append(tools, builtTools...)
		}

		var toolsets []tool.Toolset
		if cfg.ToolsetsBuilder != nil {
			ts, err := cfg.ToolsetsBuilder(ctx, top, false)
			if err != nil {
				return nil, fmt.Errorf("failed to build toolsets for step %q: %w", top.Name, err)
			}
			toolsets = ts
		}

		instruction := top.Prompt
		if cfg.InstructionBuilder != nil {
			instruction = cfg.InstructionBuilder(top, isStart, isFinalStep, nil, notesDir, prevNotesDir)
		}

		stepAg, err := llmagent.New(llmagent.Config{
			Name:        top.Name,
			Description: top.Prompt,
			Model:       llm,
			Instruction: instruction,
			Tools:       tools,
			Toolsets:    toolsets,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create step agent %q: %w", top.Name, err)
		}
		topAgents = append(topAgents, stepAg)
		prevNotesDir = notesDir
	}

	return sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:        cfg.Def.Name,
			Description: cfg.Def.Description,
			SubAgents:   topAgents,
		},
	})
}

// Runner executes multi-step workflow pipelines.
type Runner struct {
	tracker  *Tracker
	executor StepExecutor
	listener RunnerListener
}

// NewRunner creates a new workflow Runner.
func NewRunner(tracker *Tracker, executor StepExecutor, listener RunnerListener) *Runner {
	if tracker == nil {
		tracker = GlobalTracker()
	}
	if listener == nil {
		listener = NoopRunnerListener{}
	}
	return &Runner{
		tracker:  tracker,
		executor: executor,
		listener: listener,
	}
}

// Run executes the workflow def synchronously to completion or until context cancellation.
func (r *Runner) Run(ctx context.Context, cwd string, tabID int, def Def, src Source) (*RunState, error) {
	if err := def.Validate(); err != nil {
		r.listener.OnWorkflowFailed(tabID, err.Error())
		return nil, err
	}

	r.listener.OnWorkflowStarted(tabID, def, src)
	r.tracker.MarkWorking(cwd, src.Key(), def.Name, tabID)

	runState := &RunState{
		Workflow:  def,
		Source:    src,
		StartedAt: time.Now().UTC(),
		StepIdx:   0,
	}

	var stepLog []string
	var loopFrame *LoopRunFrame
	var prevNotesDir string
	var currentNotesDir string
	var remind RemindKind
	var remindDetail string
	var linearRetry int
	var linearText string
	var stepErrorRetry int

	for {
		select {
		case <-ctx.Done():
			r.listener.OnWorkflowFailed(tabID, "cancelled by user")
			r.tracker.MarkFinal(cwd, src.Key(), def.Name, StatusFailed, runState.StepIdx)
			return runState, ctx.Err()
		default:
		}

		if loopFrame == nil && runState.StepIdx >= len(def.Steps) {
			break
		}

		top := def.Steps[runState.StepIdx]
		if top.Kind == "loop" && loopFrame == nil {
			if len(top.Steps) == 0 {
				runState.StepIdx++
				continue
			}
			loopFrame = &LoopRunFrame{InnerIdx: 0, Iteration: 1}
			r.listener.OnNote(tabID, LoopNoteLine(top.Name, "started", fmt.Sprintf("max %d iteration(s)", def.EffectiveMaxIterations(top))))
		}

		step := top
		if loopFrame != nil {
			step = top.Steps[loopFrame.InnerIdx]
		}

		r.listener.OnWorkflowStepStarted(tabID, runState.StepIdx, step.Name, step.Provider, step.Model)

		isStartStep := runState.StepIdx == 0 && loopFrame == nil
		isLoopStartStep := runState.StepIdx == 0 && loopFrame != nil && loopFrame.Iteration == 1 && loopFrame.InnerIdx == 0

		var notesDir string
		switch {
		case isStartStep, isLoopStartStep:
			notesDir = StartPlanDir(cwd)
		case loopFrame != nil:
			notesDir = StepNotesDir(cwd, step.Name, top.Name, loopFrame.Iteration)
		default:
			notesDir = StepNotesDir(cwd, step.Name, "", 0)
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
			if loopFrame.InnerIdx == 0 {
				if loopFrame.PrevTail != "" {
					prevOutputs = append(prevOutputs, loopFrame.PrevTail)
				}
			} else {
				prevOutputs = append(prevOutputs, loopFrame.IterationLog...)
			}
			if loopFrame.Retry > 0 && loopFrame.RetryText != "" {
				prevOutputs = append(prevOutputs, loopFrame.RetryText)
			}
		}

		pc := &StepPromptCtx{
			Remind:              remind,
			RemindDetail:        remindDetail,
			NotesDir:            notesDir,
			PrevNotesDir:        prevNotesDir,
			IsStartStep:         isStartStep || isLoopStartStep,
			IsWorkflowFinalStep: runState.StepIdx == len(def.Steps)-1,
		}
		if loopFrame != nil {
			pc.Loop = &LoopPromptCtx{
				Name:          top.Name,
				Iteration:     loopFrame.Iteration,
				MaxIterations: def.EffectiveMaxIterations(top),
				ExitCondition: top.ExitCondition,
				IsTail:        loopFrame.InnerIdx == len(top.Steps)-1,
			}
		}

		var dirErr error
		if pc.IsStartStep {
			dirErr = EnsureStartPlanExists(cwd)
		} else {
			dirErr = EnsureStepNotesDir(notesDir)
		}
		if dirErr != nil {
			remind = RemindFixPlanDir
			remindDetail = dirErr.Error()
			pc.Remind = remind
			pc.RemindDetail = remindDetail
		}

		prompt := BuildStepPrompt(step, src, prevOutputs, pc)
		isFinalStep := runState.StepIdx == len(def.Steps)-1

		if r.executor == nil {
			err := errors.New("no step executor provided")
			r.listener.OnWorkflowFailed(tabID, err.Error())
			r.tracker.MarkFinal(cwd, src.Key(), def.Name, StatusFailed, runState.StepIdx)
			return runState, err
		}

		res, err := r.executor.ExecuteStep(ctx, cwd, tabID, step, prompt, isFinalStep)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				r.listener.OnWorkflowFailed(tabID, "cancelled by user")
				r.tracker.MarkFinal(cwd, src.Key(), def.Name, StatusFailed, runState.StepIdx)
				return runState, ctx.Err()
			}
			if stepErrorRetry < 3 {
				stepErrorRetry++
				wait := time.Duration(stepErrorRetry) * time.Second
				r.listener.OnNote(tabID, WorkflowNoteLine(fmt.Sprintf("step %q failed: %v", step.Name, err), fmt.Sprintf("retrying (attempt %d of 3)", stepErrorRetry)))
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return runState, ctx.Err()
				}
				continue
			}
			r.listener.OnWorkflowFailed(tabID, err.Error())
			r.tracker.MarkFinal(cwd, src.Key(), def.Name, StatusFailed, runState.StepIdx)
			return runState, err
		}

		stepErrorRetry = 0
		remind = RemindNone
		remindDetail = ""

		if loopFrame == nil {
			if res.Summary == "" {
				linearRetry++
				linearText = res.Output
				remind = RemindNoSummary
				r.listener.OnNote(tabID, "   | Re-prompting "+step.Name+" for end_turn")
				continue
			}

			r.listener.OnWorkflowStepDone(tabID, runState.StepIdx, res.Summary)

			if isFinalStep && res.FinishData != nil {
				runState.FinishData = res.FinishData
			}

			prevNotesDir = currentNotesDir
			if res.Output != "" {
				stepLog = append(stepLog, res.Output)
			}
			linearRetry = 0
			linearText = ""
			runState.StepIdx++
			continue
		}

		isTail := loopFrame.InnerIdx == len(top.Steps)-1
		if res.Summary == "" {
			loopFrame.Retry++
			loopFrame.RetryText = res.Output
			remind = RemindNoSummary
			r.listener.OnNote(tabID, "   | Re-prompting "+step.Name+" for end_turn")
			continue
		}

		r.listener.OnWorkflowStepDone(tabID, runState.StepIdx, res.Summary)

		// Loop termination: either explicitly via decision="break", or native exit_loop tool invocation
		if res.Decision == LoopBreak {
			if isFinalStep && res.FinishData != nil {
				runState.FinishData = res.FinishData
			}

			prevNotesDir = currentNotesDir
			if res.Output != "" {
				loopFrame.IterationLog = append(loopFrame.IterationLog, res.Output)
			}
			r.listener.OnNote(tabID, LoopNoteLine(top.Name, "break", ""))

			stepLog = append(stepLog, loopFrame.IterationLog...)
			loopFrame = nil
			runState.StepIdx++
			continue
		}

		if !isTail {
			prevNotesDir = currentNotesDir
			if res.Output != "" {
				loopFrame.IterationLog = append(loopFrame.IterationLog, res.Output)
			}
			loopFrame.Retry = 0
			loopFrame.RetryText = ""
			loopFrame.InnerIdx++
			continue
		}

		if res.Decision != LoopContinue {
			loopFrame.Retry++
			loopFrame.RetryText = res.Output
			remind = RemindNoDecision
			r.listener.OnNote(tabID, "   | Re-prompting final step for a decision")
			continue
		}

		prevNotesDir = currentNotesDir
		if res.Output != "" {
			loopFrame.IterationLog = append(loopFrame.IterationLog, res.Output)
		}

		if loopFrame.Iteration >= def.EffectiveMaxIterations(top) {
			if isFinalStep && res.FinishData != nil {
				runState.FinishData = res.FinishData
			}
			r.listener.OnNote(tabID, LoopNoteLine(top.Name, "hit iteration limit", fmt.Sprintf("%d iteration(s)", loopFrame.Iteration)))
			stepLog = append(stepLog, loopFrame.IterationLog...)
			loopFrame = nil
			runState.StepIdx++
			continue
		}

		r.listener.OnNote(tabID, LoopNoteLine(top.Name, fmt.Sprintf("iteration %d complete → continue", loopFrame.Iteration), ""))
		loopFrame.PrevTail = lastString(loopFrame.IterationLog)
		loopFrame.IterationLog = nil
		loopFrame.Iteration++
		loopFrame.InnerIdx = 0
		loopFrame.Retry = 0
		loopFrame.RetryText = ""
	}

	_ = RemoveAllWorkflowPlans(cwd)

	desc := ""
	var arts []string
	if runState.FinishData != nil {
		desc = runState.FinishData.Description
		arts = runState.FinishData.Artifacts
	}

	runState.Done = true
	r.listener.OnWorkflowDone(tabID, desc, arts)
	r.tracker.MarkFinal(cwd, src.Key(), def.Name, StatusDone, runState.StepIdx)

	return runState, nil
}

func lastString(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// BuildStepPrompt assembles the user-message prompt for a single workflow step.
func BuildStepPrompt(step Step, source Source, prevOutputs []string, pc *StepPromptCtx) string {
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
	if pc != nil && pc.NotesDir != "" {
		b.WriteString("\n\n")
		b.WriteString("Workflow notes directories:\n")
		b.WriteString("- Your notes directory: " + pc.NotesDir)
		if pc.PrevNotesDir != "" {
			b.WriteString("\n- Previous step's notes directory: " + pc.PrevNotesDir)
		}
		if pc.IsStartStep {
			b.WriteString("\n\nThis is the first step. Your notes directory (")
			b.WriteString(pc.NotesDir)
			b.WriteString(") MUST be a directory, not a file. Create it if it does not exist, then write one or more files inside it (for example ")
			b.WriteString(filepath.Join(pc.NotesDir, "plan.md"))
			b.WriteString("). Do NOT write a single file named \"start\". The workflow runner verifies the directory exists and contains files before step 1; if it is missing, empty, or a file, this step will be re-prompted to fix the directory before any work is done.")
		}
	}
	var loop *LoopPromptCtx
	remind := RemindNone
	if pc != nil {
		loop = pc.Loop
		remind = pc.Remind
	}
	b.WriteString("\n\n")
	b.WriteString(EndTurnInstructionBlock(loop))
	if remind != RemindNone {
		b.WriteString("\n\n")
		b.WriteString(EndTurnReminder(remind, pc.RemindDetail))
	}
	return strings.TrimSpace(b.String())
}

// EndTurnInstructionBlock renders the auto-injected end_turn contract for a step.
func EndTurnInstructionBlock(loop *LoopPromptCtx) string {
	var b strings.Builder
	if loop != nil {
		fmt.Fprintf(&b, "[Workflow loop %q · iteration %d of up to %d]", loop.Name, loop.Iteration, loop.MaxIterations)
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

// EndTurnReminder renders instructions when a step must be re-prompted.
func EndTurnReminder(k RemindKind, detail string) string {
	switch k {
	case RemindNoDecision:
		return "REMINDER: you called end_turn without a `decision`, which is required for the final step of a " +
			"loop iteration. You have already done the work shown above — do NOT repeat it. Call end_turn again now " +
			"with decision=\"continue\" or decision=\"break\"."
	case RemindFixPlanDir:
		msg := "REMINDER: the workflow notes directory is not usable"
		if detail != "" {
			msg += ": " + detail
		}
		msg += ". You must make it a directory containing files, then call end_turn."
		return msg
	case RemindNoFinishTool:
		return "REMINDER: you reached the final step of the workflow without providing final finish data. Call the finish tool or end_turn."
	default:
		return "REMINDER: your previous turn ended without calling end_turn. You have already done the work shown " +
			"above — do NOT repeat it. Call the end_turn tool now (see the instructions above for what to include)."
	}
}

// WorkflowNoteLine formats a single-line status note.
func WorkflowNoteLine(msg, detail string) string {
	res := "   " + msg
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

