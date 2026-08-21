package workflow

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
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
	SessionService     session.Service
	ArtifactService    artifact.Service
	Def                Def
	Source             Source
	Cwd                string
	TabID              int
	ModelBuilder       func(ctx context.Context, step Step) (model.LLM, error)
	ToolsBuilder       func(ctx context.Context, step Step, isLoop bool) ([]tool.Tool, error)
	ToolsetsBuilder    func(ctx context.Context, step Step, isLoop bool) ([]tool.Toolset, error)
	InstructionBuilder func(step Step, isStart bool, isFinal bool, loopCtx *LoopPromptCtx, notesDir, prevNotesDir string) string
}

// Runner executes multi-step workflow pipelines.

// NewRunner creates a new workflow Runner.

// Run executes the workflow def synchronously to completion or until context cancellation.

type Runner struct {
	cfg     WorkflowAgentConfig
	tracker *Tracker
}

func NewRunner(tracker *Tracker, cfg WorkflowAgentConfig) *Runner {
	if tracker == nil {
		tracker = GlobalTracker()
	}
	return &Runner{
		cfg:     cfg,
		tracker: tracker,
	}
}

func (r *Runner) Run(ctx context.Context, listener RunnerListener) (*RunState, error) {
	if err := r.cfg.Def.Validate(); err != nil {
		listener.OnWorkflowFailed(r.cfg.TabID, err.Error())
		return nil, err
	}

	wfAgent, err := CompileDefToADKWorkflow(ctx, r.cfg)
	if err != nil {
		listener.OnWorkflowFailed(r.cfg.TabID, err.Error())
		return nil, err
	}

	listener.OnWorkflowStarted(r.cfg.TabID, r.cfg.Def, r.cfg.Source)
	if r.tracker != nil {
		r.tracker.MarkWorking(r.cfg.Cwd, r.cfg.Source.Key(), r.cfg.Def.Name, r.cfg.TabID)
	}

	runState := &RunState{
		Workflow:  r.cfg.Def,
		Source:    r.cfg.Source,
		StartedAt: time.Now().UTC(),
		StepIdx:   0,
	}

	sessSvc := r.cfg.SessionService
	if sessSvc == nil {
		sessSvc = session.InMemoryService()
	}

	adkRunner, err := runner.New(runner.Config{
		AppName:           "ask-workflow",
		Agent:             wfAgent,
		SessionService:    sessSvc,
		ArtifactService:   r.cfg.ArtifactService,
		AutoCreateSession: true,
	})
	if err != nil {
		listener.OnWorkflowFailed(r.cfg.TabID, err.Error())
		if r.tracker != nil {
			r.tracker.MarkFinal(r.cfg.Cwd, r.cfg.Source.Key(), r.cfg.Def.Name, StatusFailed, 0)
		}
		return runState, err
	}

	userMsg := genai.NewContentFromText(r.cfg.Source.Display(), genai.RoleUser)
	sessionID := "wf-" + r.cfg.Source.Key()

	startedSteps := make(map[int]bool)
	doneSteps := make(map[int]bool)
	lastStepIdx := -1

	for event, err := range adkRunner.Run(ctx, "user", sessionID, userMsg, agent.RunConfig{}) {
		if err != nil {
			listener.OnWorkflowFailed(r.cfg.TabID, err.Error())
			if r.tracker != nil {
				r.tracker.MarkFinal(r.cfg.Cwd, r.cfg.Source.Key(), r.cfg.Def.Name, StatusFailed, lastStepIdx)
			}
			return runState, err
		}
		if event == nil {
			continue
		}

		if event.Author != "" && event.Author != "user" && event.Author != "ask_coder" {
			for i, s := range r.cfg.Def.Steps {
				if s.Name == event.Author {
					if lastStepIdx >= 0 && lastStepIdx != i && !doneSteps[lastStepIdx] {
						doneSteps[lastStepIdx] = true
						listener.OnWorkflowStepDone(r.cfg.TabID, lastStepIdx, fmt.Sprintf("completed step %s", r.cfg.Def.Steps[lastStepIdx].Name))
					}
					if !startedSteps[i] {
						startedSteps[i] = true
						lastStepIdx = i
						listener.OnWorkflowStepStarted(r.cfg.TabID, i, s.Name, s.Provider, s.Model)
					}
					break
				}
			}
		}

		if event.LLMResponse.Content != nil {
			for _, part := range event.LLMResponse.Content.Parts {
				if part.Text != "" {
					listener.OnNote(r.cfg.TabID, part.Text)
				}
			}
		}
	}

	for i := 0; i < len(r.cfg.Def.Steps); i++ {
		if !startedSteps[i] {
			startedSteps[i] = true
			listener.OnWorkflowStepStarted(r.cfg.TabID, i, r.cfg.Def.Steps[i].Name, r.cfg.Def.Steps[i].Provider, r.cfg.Def.Steps[i].Model)
		}
		if !doneSteps[i] {
			doneSteps[i] = true
			listener.OnWorkflowStepDone(r.cfg.TabID, i, fmt.Sprintf("completed step %s", r.cfg.Def.Steps[i].Name))
		}
	}

	runState.Done = true
	if r.tracker != nil {
		r.tracker.MarkFinal(r.cfg.Cwd, r.cfg.Source.Key(), r.cfg.Def.Name, StatusDone, len(r.cfg.Def.Steps)-1)
	}
	listener.OnWorkflowDone(r.cfg.TabID, "workflow completed", nil)

	return runState, nil
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

func lastString(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}
