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
	out := make([]string, 0, len(c.StepIndexByAgent))
	for name := range c.StepIndexByAgent {
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
		if got := c.StepNameByAgent[want]; got != want {
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
	if _, ok := c.StepIndexByAgent["do_the_thing"]; !ok {
		t.Errorf("expected spaces to become underscores, got %v", names)
	}
	if _, ok := c.StepIndexByAgent["do_the_thing_2"]; !ok {
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
