package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/exitlooptool"
	adkworkflow "google.golang.org/adk/v2/workflow"
)

// WorkflowAgentConfig carries everything the compiler needs to turn a
// Def into a runnable ADK graph. The builder callbacks are the seam the
// engine and the TUI fill in with their own model/tool wiring, and the
// seam tests swap for fakes.
type WorkflowAgentConfig struct {
	Def    Def
	Source Source
	Cwd    string
	TabID  int

	// ModelBuilder resolves the LLM for one step. Required.
	ModelBuilder func(ctx context.Context, step Step) (model.LLM, error)
	// ToolsBuilder and ToolsetsBuilder supply the step's tool surface.
	// StepRole tells them where the step sits, so the builder can attach
	// finish_workflow to the final step only, and so on.
	ToolsBuilder    func(ctx context.Context, step Step, role StepRole) ([]tool.Tool, error)
	ToolsetsBuilder func(ctx context.Context, step Step, role StepRole) ([]tool.Toolset, error)
	// InstructionBuilder renders the step's system instruction. Defaults
	// to BuildStepInstruction.
	InstructionBuilder func(step Step, src Source, pc *StepPromptCtx) string
	// MaxRetries bounds per-node retries on failure. Zero means
	// workflowDefaultMaxRetries; negative disables retries.
	MaxRetries int
	// BeforeModelCallbacks run before every LLM invocation.
	BeforeModelCallbacks []llmagent.BeforeModelCallback
}

// workflowDefaultMaxRetries is the per-node retry budget. Replaces the
// runner's hand-rolled stepErrorRetry loop with ADK's scheduler-level
// RetryConfig, which also handles the backoff.
const workflowDefaultMaxRetries = 3

// StepRole tells a ToolsBuilder where a step sits in the workflow, so it
// can decide which position-dependent tools to attach.
type StepRole struct {
	// InLoop is true when the step runs inside a loop container.
	InLoop bool
	// IsTail is true when the step is the last inner step of its loop —
	// the only step allowed to break the loop early via exit_loop.
	IsTail bool
	// IsFinal is true when the step is the last thing the whole workflow
	// runs (the last top-level step, or the tail of a final loop). Only
	// this step gets finish_workflow.
	IsFinal bool
}

// StepAgentInfo tracks metadata for one step agent in the compiled graph.
type StepAgentInfo struct {
	StepIndex int
	StepName  string
	Provider  string
	Model     string
	InLoop    bool
	LoopName  string
	InnerIdx  int
}

// Compiled is a Def rendered as an executable ADK graph, plus the lookup
// the event adapter needs to attribute events back to steps.
type Compiled struct {
	Workflow *adkworkflow.Workflow
	// AgentInfo maps an emitted event's Author (an ADK agent
	// name) to its metadata. Inner loop agents map to their
	// containing loop step index.
	AgentInfo map[string]StepAgentInfo
	// Models are the per-step models built at compile time. Close releases
	// them after the run — a subprocess-backed provider (Claude Code) forks
	// a child per step that must be terminated.
	Models []model.LLM
}

// Close releases every per-step model. It is safe to call on a nil Compiled
// and after a partial compile.
func (c *Compiled) Close() error {
	if c == nil {
		return nil
	}
	for _, m := range c.Models {
		if closer, ok := m.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	c.Models = nil
	return nil
}

// StepIndex resolves an event author to a top-level step index.
func (c *Compiled) StepIndex(author string) (int, bool) {
	if c == nil || author == "" {
		return 0, false
	}
	info, ok := c.AgentInfo[author]
	return info.StepIndex, ok
}

// CompileWorkflow compiles a Def into an ADK workflow graph.
//
// Every top-level step becomes one node, chained Start -> n0 -> n1 -> …:
//
//   - An agent step becomes an AgentNode wrapping an llmagent.
//   - A loop step becomes an AgentNode wrapping a loopagent whose
//     sub-agents are the inner steps, each carrying ADK's exit_loop
//     tool. Any inner step calling exit_loop escalates and ends the
//     loop; otherwise it runs to MaxIterations.
//
// Two llmagent settings carry the workflow semantics and must not be
// dropped:
//
//   - IncludeContentsNone gives each step its own context. Without it a
//     step inherits every prior step's events, and ADK renders those
//     foreign events as prose — every tool call and every full tool
//     result — so step 3 would carry steps 1 and 2 in full.
//   - InstructionProvider, never Config.Instruction. Step prompts are
//     user-authored and routinely contain braces; a static Instruction
//     is run through ADK's state interpolator and hard-fails the
//     invocation on the first `{...}`.
func CompileWorkflow(ctx context.Context, cfg WorkflowAgentConfig) (*Compiled, error) {
	if err := cfg.Def.Validate(); err != nil {
		return nil, err
	}
	if cfg.ModelBuilder == nil {
		return nil, errors.New("workflow compile: model builder is required")
	}

	names := newAgentNamer()
	out := &Compiled{
		AgentInfo: map[string]StepAgentInfo{},
	}

	var nodes []adkworkflow.Node
	for i, top := range cfg.Def.Steps {
		isFinal := i == len(cfg.Def.Steps)-1

		var node adkworkflow.Node
		var err error
		if top.IsLoop() {
			node, err = compileLoopNode(ctx, cfg, top, i, isFinal, names, out)
		} else {
			node, err = compileStepNode(ctx, cfg, top, i, -1, isFinal, nil, names, out)
		}
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return nil, errors.New("workflow compile: definition has no executable steps")
	}

	edges := append([]adkworkflow.Edge{{From: adkworkflow.Start, To: nodes[0]}}, adkworkflow.Chain(nodes...)...)
	wf, err := adkworkflow.New(cfg.Def.Name, edges)
	if err != nil {
		return nil, fmt.Errorf("workflow compile: %w", err)
	}
	out.Workflow = wf
	return out, nil
}

// compileStepNode builds the AgentNode for one agent step. pc carries
// the loop framing when the step is an inner loop step.
func compileStepNode(ctx context.Context, cfg WorkflowAgentConfig, step Step, stepIdx, innerIdx int, isFinal bool, loop *LoopPromptCtx, names *agentNamer, out *Compiled) (adkworkflow.Node, error) {
	ag, err := buildStepAgent(ctx, cfg, step, stepIdx, innerIdx, isFinal, loop, names, out)
	if err != nil {
		return nil, err
	}
	return adkworkflow.NewAgentNode(ag, nodeConfig(cfg))
}

// compileLoopNode wraps a loop step's inner agents in an ADK loopagent.
func compileLoopNode(ctx context.Context, cfg WorkflowAgentConfig, loopStep Step, stepIdx int, isFinal bool, names *agentNamer, out *Compiled) (adkworkflow.Node, error) {
	maxIter := cfg.Def.EffectiveMaxIterations(loopStep)

	var inner []agent.Agent
	for j, innerStep := range loopStep.Steps {
		pc := &LoopPromptCtx{
			Name:          loopStep.Name,
			MaxIterations: maxIter,
			ExitCondition: loopStep.ExitCondition,
			IsTail:        j == len(loopStep.Steps)-1,
		}
		ag, err := buildStepAgent(ctx, cfg, innerStep, stepIdx, j, isFinal && pc.IsTail, pc, names, out)
		if err != nil {
			return nil, err
		}
		inner = append(inner, ag)
	}

	loopName := names.claim(loopStep.Name, "loop")
	out.AgentInfo[loopName] = StepAgentInfo{
		StepIndex: stepIdx,
		StepName:  loopStep.Name,
		InLoop:    true,
		LoopName:  loopStep.Name,
		InnerIdx:  -1,
	}

	loopAg, err := loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:        loopName,
			Description: loopStep.ExitCondition,
			SubAgents:   inner,
		},
		MaxIterations: uint(maxIter),
	})
	if err != nil {
		return nil, fmt.Errorf("workflow compile: loop %q: %w", loopStep.Name, err)
	}
	return adkworkflow.NewAgentNode(loopAg, nodeConfig(cfg))
}

func buildStepAgent(ctx context.Context, cfg WorkflowAgentConfig, step Step, stepIdx, innerIdx int, isFinal bool, loop *LoopPromptCtx, names *agentNamer, out *Compiled) (agent.Agent, error) {
	llm, err := cfg.ModelBuilder(ctx, step)
	if err != nil {
		return nil, fmt.Errorf("workflow compile: model for step %q: %w", step.Name, err)
	}
	out.Models = append(out.Models, llm)

	role := StepRole{InLoop: loop != nil, IsFinal: isFinal}
	if loop != nil {
		role.IsTail = loop.IsTail
	}

	var tools []tool.Tool
	if cfg.ToolsBuilder != nil {
		tools, err = cfg.ToolsBuilder(ctx, step, role)
		if err != nil {
			return nil, fmt.Errorf("workflow compile: tools for step %q: %w", step.Name, err)
		}
	}
	// Only the tail step of a loop may break out early. exit_loop sets
	// Actions.Escalate, which is the only way an ask tool ends a
	// loopagent, and no other tool touches Actions — so withholding it
	// from the non-tail steps makes an early break structurally
	// impossible for them, rather than something caught after the fact.
	if role.InLoop && role.IsTail {
		tools, err = withExitLoopTool(tools)
		if err != nil {
			return nil, fmt.Errorf("workflow compile: step %q: %w", step.Name, err)
		}
	}

	var toolsets []tool.Toolset
	if cfg.ToolsetsBuilder != nil {
		toolsets, err = cfg.ToolsetsBuilder(ctx, step, role)
		if err != nil {
			return nil, fmt.Errorf("workflow compile: toolsets for step %q: %w", step.Name, err)
		}
	}

	build := cfg.InstructionBuilder
	if build == nil {
		build = BuildStepInstruction
	}
	instruction := build(step, cfg.Source, &StepPromptCtx{
		Loop:                loop,
		IsStartStep:         stepIdx == 0,
		IsWorkflowFinalStep: isFinal,
	})

	name := names.claim(step.Name, "step")
	info := StepAgentInfo{
		StepIndex: stepIdx,
		StepName:  step.Name,
		Provider:  step.Provider,
		Model:     step.Model,
		InLoop:    loop != nil,
		InnerIdx:  innerIdx,
	}
	if loop != nil {
		info.LoopName = loop.Name
	}
	out.AgentInfo[name] = info

	return llmagent.New(llmagent.Config{
		Name:        name,
		Description: firstLine(step.Prompt),
		Model:       llm,
		// Never Config.Instruction: step prompts are user-authored and
		// ADK interpolates that field, failing the run on any brace.
		InstructionProvider: literalInstruction(instruction),
		// Each step gets its own context; see CompileWorkflow.
		IncludeContents:      llmagent.IncludeContentsNone,
		Tools:                tools,
		Toolsets:             toolsets,
		BeforeModelCallbacks: cfg.BeforeModelCallbacks,
	})
}

// literalInstruction adapts a fixed string to an InstructionProvider so
// ADK treats it as text rather than as a `{placeholder}` template.
func literalInstruction(s string) llmagent.InstructionProvider {
	return func(agent.ReadonlyContext) (string, error) { return s, nil }
}

func withExitLoopTool(tools []tool.Tool) ([]tool.Tool, error) {
	exitTool, err := exitlooptool.New()
	if err != nil {
		return nil, fmt.Errorf("exit_loop tool: %w", err)
	}
	for _, t := range tools {
		if t != nil && t.Name() == exitTool.Name() {
			return tools, nil
		}
	}
	return append(tools, exitTool), nil
}

func nodeConfig(cfg WorkflowAgentConfig) adkworkflow.NodeConfig {
	retries := cfg.MaxRetries
	if retries == 0 {
		retries = workflowDefaultMaxRetries
	}
	if retries < 0 {
		return adkworkflow.NodeConfig{}
	}
	rc := adkworkflow.DefaultRetryConfig()
	rc.MaxAttempts = retries
	return adkworkflow.NodeConfig{RetryConfig: rc}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// agentNamer turns user-written step names into ADK agent names. ADK
// requires them unique within a graph and rejects "user"; step names are
// free text and not checked for uniqueness, so both have to be enforced
// here rather than assumed.
type agentNamer struct {
	used map[string]bool
}

func newAgentNamer() *agentNamer { return &agentNamer{used: map[string]bool{}} }

func (n *agentNamer) claim(raw, fallback string) string {
	base := sanitizeAgentName(raw)
	if base == "" || strings.EqualFold(base, "user") {
		base = fallback
	}
	name := base
	for i := 2; n.used[name]; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	n.used[name] = true
	return name
}

func sanitizeAgentName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '_' || r == '-' || r == ' ':
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return ""
	}
	if r := rune(out[0]); unicode.IsDigit(r) {
		out = "_" + out
	}
	return out
}
