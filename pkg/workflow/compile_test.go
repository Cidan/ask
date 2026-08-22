package workflow

import (
	"context"
	"iter"
	"sort"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
)

type fakeLLM struct{ name string }

func (f *fakeLLM) Name() string { return f.name }
func (f *fakeLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {}
}

func testCompileConfig(def Def) WorkflowAgentConfig {
	return WorkflowAgentConfig{
		Def:    def,
		Source: NewTextSource(1, "source"),
		Cwd:    "/tmp/proj",
		ModelBuilder: func(ctx context.Context, step Step) (model.LLM, error) {
			return &fakeLLM{name: "fake"}, nil
		},
	}
}

func agentNames(c *Compiled) []string {
	out := make([]string, 0, len(c.AgentInfo))
	for name := range c.AgentInfo {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestCompileWorkflow_Linear(t *testing.T) {
	def := Def{Name: "wf", Steps: []Step{
		{Name: "plan", Prompt: "make a plan"},
		{Name: "implement", Prompt: "do it"},
		{Name: "review", Prompt: "check it"},
	}}

	c, err := CompileWorkflow(context.Background(), testCompileConfig(def))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if c.Workflow == nil {
		t.Fatal("compiled workflow is nil")
	}
	if c.Workflow.Name() != "wf" {
		t.Errorf("workflow name = %q, want wf", c.Workflow.Name())
	}

	for i, want := range []string{"plan", "implement", "review"} {
		idx, ok := c.StepIndex(want)
		if !ok {
			t.Fatalf("no step index recorded for agent %q (have %v)", want, agentNames(c))
		}
		if idx != i {
			t.Errorf("agent %q maps to step %d, want %d", want, idx, i)
		}
		if got := c.AgentInfo[want].StepName; got != want {
			t.Errorf("agent %q maps to step name %q", want, got)
		}
	}
}

// Inner loop agents report against the loop step the user authored, so
// the UI shows progress for "review-loop" rather than for compiler
// -generated inner node names.
func TestCompileWorkflow_LoopInnerAgentsMapToLoopStep(t *testing.T) {
	def := Def{Name: "wf", Steps: []Step{
		{Name: "plan", Prompt: "plan"},
		{Name: "fix-loop", Kind: "loop", MaxIterations: 3, ExitCondition: "tests pass", Steps: []Step{
			{Name: "edit", Prompt: "edit"},
			{Name: "verify", Prompt: "verify"},
		}},
	}}

	c, err := CompileWorkflow(context.Background(), testCompileConfig(def))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, agent := range []string{"fix_loop", "edit", "verify"} {
		idx, ok := c.StepIndex(agent)
		if !ok {
			t.Fatalf("no mapping for %q (have %v)", agent, agentNames(c))
		}
		if idx != 1 {
			t.Errorf("agent %q maps to step %d, want the loop step 1", agent, idx)
		}
	}
	if idx, _ := c.StepIndex("plan"); idx != 0 {
		t.Errorf("linear step should still map to 0, got %d", idx)
	}
}

// ADK requires agent names unique within a graph and rejects "user".
// Step names are free text and never checked for uniqueness, so the
// compiler has to enforce both.
func TestCompileWorkflow_AgentNamesSanitizedAndUnique(t *testing.T) {
	def := Def{Name: "wf", Steps: []Step{
		{Name: "do the thing", Prompt: "a"},
		{Name: "do the thing", Prompt: "b"},
		{Name: "user", Prompt: "c"},
		{Name: "!!!", Prompt: "d"},
		{Name: "2fast", Prompt: "e"},
	}}

	c, err := CompileWorkflow(context.Background(), testCompileConfig(def))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	names := agentNames(c)
	if len(names) != 5 {
		t.Fatalf("want 5 distinct agent names, got %d: %v", len(names), names)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate agent name %q", n)
		}
		seen[n] = true
		if n == "user" {
			t.Error(`"user" is reserved by ADK and must not be used as an agent name`)
		}
		if n == "" {
			t.Error("empty agent name")
		}
	}
	if _, ok := c.AgentInfo["do_the_thing"]; !ok {
		t.Errorf("expected spaces to become underscores, got %v", names)
	}
	if _, ok := c.AgentInfo["do_the_thing_2"]; !ok {
		t.Errorf("expected the duplicate to be suffixed, got %v", names)
	}
}

func TestCompileWorkflow_Errors(t *testing.T) {
	t.Run("invalid definition", func(t *testing.T) {
		if _, err := CompileWorkflow(context.Background(), testCompileConfig(Def{Name: "wf"})); err == nil {
			t.Error("a def with no steps must not compile")
		}
	})
	t.Run("missing model builder", func(t *testing.T) {
		cfg := testCompileConfig(Def{Name: "wf", Steps: []Step{{Name: "a", Prompt: "a"}}})
		cfg.ModelBuilder = nil
		if _, err := CompileWorkflow(context.Background(), cfg); err == nil {
			t.Error("compiling without a model builder must fail")
		}
	})
	t.Run("model builder failure names the step", func(t *testing.T) {
		cfg := testCompileConfig(Def{Name: "wf", Steps: []Step{{Name: "broken", Prompt: "x"}}})
		cfg.ModelBuilder = func(ctx context.Context, step Step) (model.LLM, error) {
			return nil, context.DeadlineExceeded
		}
		_, err := CompileWorkflow(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), "broken") {
			t.Errorf("error should name the failing step, got %v", err)
		}
	})
}

// Loop steps get ADK's exit_loop tool, which is how a step breaks out:
// it sets Actions.Escalate, which is what loopagent watches for.
func TestWithExitLoopTool(t *testing.T) {
	got, err := withExitLoopTool(nil)
	if err != nil {
		t.Fatalf("withExitLoopTool: %v", err)
	}
	if len(got) != 1 || got[0].Name() != "exit_loop" {
		t.Fatalf("expected exit_loop to be added, got %v", toolNames(got))
	}

	again, err := withExitLoopTool(got)
	if err != nil {
		t.Fatalf("withExitLoopTool: %v", err)
	}
	if len(again) != 1 {
		t.Errorf("exit_loop must not be added twice, got %v", toolNames(again))
	}
}

func toolNames(ts []tool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		if t != nil {
			out = append(out, t.Name())
		}
	}
	return out
}

func TestBuildStepInstruction(t *testing.T) {
	src := NewTextSource(1, "the source")
	step := Step{Name: "plan", Prompt: "Write the plan."}

	got := BuildStepInstruction(step, src, &StepPromptCtx{IsStartStep: true})
	if !strings.Contains(got, "Write the plan.") {
		t.Errorf("instruction must carry the author's prompt: %q", got)
	}
	if !strings.Contains(got, "end_turn") {
		t.Errorf("instruction must carry the end_turn contract: %q", got)
	}
	if !strings.Contains(got, "call the todos tool") {
		t.Errorf("instruction must mandate the todos tool: %q", got)
	}
	if strings.Contains(got, "Previous step output:") {
		t.Errorf("previous-step threading belongs to the graph now: %q", got)
	}
	if strings.Contains(got, "notes director") || strings.Contains(got, "ask/plans") {
		t.Errorf("notes directories are gone: %q", got)
	}
}

func TestBuildStepInstruction_LoopFraming(t *testing.T) {
	src := NewTextSource(1, "src")
	step := Step{Name: "verify", Prompt: "Check."}
	loop := &LoopPromptCtx{Name: "fix", MaxIterations: 5, ExitCondition: "tests pass", IsTail: true}

	got := BuildStepInstruction(step, src, &StepPromptCtx{Loop: loop})
	for _, want := range []string{"fix", "5", "tests pass", "exit_loop"} {
		if !strings.Contains(got, want) {
			t.Errorf("loop instruction missing %q: %q", want, got)
		}
	}
	// Breaking is exit_loop now, not an end_turn argument.
	if strings.Contains(got, `decision`) {
		t.Errorf("loop control moved to exit_loop; instruction should not mention a decision arg: %q", got)
	}
}

// Only the tail step is told how to break, because only the tail step has
// the exit_loop tool. A non-tail step is told it cannot end the loop.
func TestBuildStepInstruction_NonTailLoopStepCannotBreak(t *testing.T) {
	src := NewTextSource(1, "src")
	step := Step{Name: "edit", Prompt: "Edit."}
	loop := &LoopPromptCtx{Name: "fix", MaxIterations: 5, IsTail: false}

	got := BuildStepInstruction(step, src, &StepPromptCtx{Loop: loop})
	if strings.Contains(got, "exit_loop") {
		t.Errorf("a non-tail step must not be told about exit_loop: %q", got)
	}
	if !strings.Contains(got, "NOT its last step") {
		t.Errorf("a non-tail step should be told it cannot end the loop: %q", got)
	}
}

// The final step is told to report the run's outcome with finish_workflow.
func TestBuildStepInstruction_FinalStepReportsWithFinishWorkflow(t *testing.T) {
	src := NewTextSource(1, "src")
	step := Step{Name: "ship", Prompt: "Open the PR."}

	final := BuildStepInstruction(step, src, &StepPromptCtx{IsWorkflowFinalStep: true})
	if !strings.Contains(final, "finish_workflow") {
		t.Errorf("the final step must be told to call finish_workflow: %q", final)
	}

	mid := BuildStepInstruction(step, src, &StepPromptCtx{})
	if strings.Contains(mid, "finish_workflow") {
		t.Errorf("a non-final step must not be told about finish_workflow: %q", mid)
	}
	// Every step is told it can pass data forward.
	if !strings.Contains(mid, "save_artifact") {
		t.Errorf("every step should learn about save_artifact: %q", mid)
	}
}

// recordRoles compiles def with a ToolsBuilder that records the StepRole
// handed to each step, keyed by step name.
func recordRoles(t *testing.T, def Def) map[string]StepRole {
	t.Helper()
	roles := map[string]StepRole{}
	cfg := testCompileConfig(def)
	cfg.ToolsBuilder = func(_ context.Context, step Step, role StepRole) ([]tool.Tool, error) {
		roles[step.Name] = role
		return nil, nil
	}
	if _, err := CompileWorkflow(context.Background(), cfg); err != nil {
		t.Fatalf("compile: %v", err)
	}
	return roles
}

// The role handed to each step is what decides its position-dependent
// tools: IsTail gates exit_loop (only the tail step may break a loop),
// IsFinal gates finish_workflow (only the last step reports the outcome).
func TestCompileWorkflow_StepRoles(t *testing.T) {
	def := Def{Name: "wf", Steps: []Step{
		{Name: "a", Prompt: "a"},
		{Name: "fix", Kind: "loop", Steps: []Step{
			{Name: "b", Prompt: "b"},
			{Name: "c", Prompt: "c"},
			{Name: "d", Prompt: "d"},
		}},
		{Name: "e", Prompt: "e"},
	}}
	roles := recordRoles(t, def)

	want := map[string]StepRole{
		"a": {InLoop: false, IsTail: false, IsFinal: false},
		"b": {InLoop: true, IsTail: false, IsFinal: false},
		"c": {InLoop: true, IsTail: false, IsFinal: false},
		"d": {InLoop: true, IsTail: true, IsFinal: false}, // tail, but the loop is not the final step
		"e": {InLoop: false, IsTail: false, IsFinal: true},
	}
	for name, w := range want {
		if roles[name] != w {
			t.Errorf("role[%q] = %+v, want %+v", name, roles[name], w)
		}
	}
}

// When the workflow ends in a loop, the loop's tail step is BOTH the
// break point and the final step, so it gets exit_loop and finish_workflow.
func TestCompileWorkflow_FinalLoopTailIsFinal(t *testing.T) {
	def := Def{Name: "wf", Steps: []Step{
		{Name: "a", Prompt: "a"},
		{Name: "fix", Kind: "loop", Steps: []Step{
			{Name: "b", Prompt: "b"},
			{Name: "c", Prompt: "c"},
		}},
	}}
	roles := recordRoles(t, def)

	if r := roles["b"]; r.IsTail || r.IsFinal {
		t.Errorf("non-tail loop step b = %+v, want neither tail nor final", r)
	}
	if r := roles["c"]; !r.IsTail || !r.IsFinal {
		t.Errorf("tail of the final loop c = %+v, want both tail and final", r)
	}
}
