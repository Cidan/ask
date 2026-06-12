package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func writeSubagent(t *testing.T, dir, name, frontmatterExtra, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + name + " agent\n" + frontmatterExtra + "---\n" + body
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverSubagents_PrecedenceAndFields(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	globalDir := filepath.Join(home, ".claude", "agents")
	projectDir := filepath.Join(cwd, ".claude", "agents")
	writeSubagent(t, globalDir, "reviewer", "tools: read, grep\nmodel: claude-haiku-4-5-20251001\nprovider: anthropic\n", "Review code carefully.")
	writeSubagent(t, projectDir, "reviewer", "", "Project reviewer.")
	writeSubagent(t, globalDir, "fixer", "tools: *\n", "Fix things.")

	defs := discoverSubagents(cwd)
	byName := map[string]subagentDef{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if len(defs) != 2 {
		t.Fatalf("want reviewer+fixer, got %d", len(defs))
	}
	if byName["reviewer"].Prompt != "Project reviewer." {
		t.Errorf("project def must win: %+v", byName["reviewer"])
	}
	if len(byName["fixer"].Tools) != 1 || byName["fixer"].Tools[0] != "*" {
		t.Errorf("tools parse wrong: %+v", byName["fixer"].Tools)
	}

	block := subagentsPromptBlock(defs)
	if !strings.Contains(block, "<name>reviewer</name>") || !strings.Contains(block, "task tool") {
		t.Errorf("agents prompt block wrong: %q", block)
	}
	if got := subagentsPromptBlock(nil); got != "" {
		t.Errorf("no agents must render nothing: %q", got)
	}
}

func TestSubagentTools_GrantSets(t *testing.T) {
	env, _ := newTestToolEnv(t)
	names := func(tools []fantasy.AgentTool) map[string]bool {
		out := map[string]bool{}
		for _, tl := range tools {
			out[tl.Info().Name] = true
		}
		return out
	}

	got := names(subagentTools(subagentDef{}, env))
	if len(got) != 4 || !got["read"] || !got["grep"] || got["bash"] {
		t.Errorf("default grant must be read-only: %v", got)
	}
	got = names(subagentTools(subagentDef{Tools: []string{"*"}}, env))
	if !got["bash"] || !got["write"] || !got["edit"] || got["task"] || got["ask_user_question"] {
		t.Errorf("star grant must be the coding core without task/modal tools: %v", got)
	}
	got = names(subagentTools(subagentDef{Tools: []string{"read", "bash", "bogus"}}, env))
	if len(got) != 2 || !got["read"] || !got["bash"] {
		t.Errorf("explicit grant must filter unknowns: %v", got)
	}
}

func TestAgentSpecByID(t *testing.T) {
	for _, id := range []string{"deepseek", "anthropic", "openai"} {
		spec, ok := agentSpecByID(id)
		if !ok || spec.id != id {
			t.Errorf("spec %s must resolve", id)
		}
	}
	if _, ok := agentSpecByID("claude"); ok {
		t.Error("subprocess providers are not specs")
	}
}

func TestAnthropicModelAlias(t *testing.T) {
	if got := anthropicModelAlias("haiku"); !strings.Contains(got, "haiku") || got == "haiku" {
		t.Errorf("alias must resolve to a catalog id: %q", got)
	}
	if got := anthropicModelAlias("claude-fable-5"); got != "claude-fable-5" {
		t.Errorf("full ids pass through: %q", got)
	}
}

func TestResolveSubagentModel(t *testing.T) {
	isolateHome(t)
	parent := &fakeLM{}

	// Nothing pinned → inherit the parent (budget 0 = inherit too).
	lm, budget, err := resolveSubagentModel(subagentDef{Name: "x"}, "deepseek", parent)
	if err != nil || lm != fantasy.LanguageModel(parent) || budget != 0 {
		t.Errorf("inherit failed: %v budget=%d %v", lm, budget, err)
	}

	// Cross-provider pin builds through the target spec and carries the
	// pinned model's output budget.
	child := &fakeLM{}
	swapDeepseekLM(t, child)
	lm, budget, err = resolveSubagentModel(subagentDef{Name: "x", Provider: "deepseek", Model: "deepseek-v4-flash"}, "anthropic", parent)
	if err != nil || lm != fantasy.LanguageModel(child) {
		t.Errorf("cross-provider resolve failed: %v %v", lm, err)
	}
	if want := deepseekSpec.maxOutputTokens("deepseek-v4-flash"); budget != want {
		t.Errorf("pinned budget = %d want %d", budget, want)
	}

	// Unknown provider errors clearly.
	if _, _, err := resolveSubagentModel(subagentDef{Name: "x", Provider: "codex"}, "deepseek", parent); err == nil ||
		!strings.Contains(err.Error(), "not an in-process provider") {
		t.Errorf("subprocess provider must be rejected: %v", err)
	}
}

func TestAgentTaskTool_NamedAgentCrossProvider(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeSubagent(t, filepath.Join(home, ".claude", "agents"), "researcher",
		"provider: deepseek\ntools: read, ls\n", "You are the researcher.")

	child := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("research report", fantasy.Usage{}),
	}}
	swapDeepseekLM(t, child)

	env, _ := newTestToolEnv(t)
	env.cwd = cwd
	parent := &fakeLM{}
	tool := agentTaskTool(env,
		func() fantasy.LanguageModel { return parent },
		func() int64 { return 111 })

	resp := runTool(t, tool, agentTaskParams{Prompt: "find the thing", Agent: "researcher"})
	if resp.IsError || resp.Content != "research report" {
		t.Fatalf("named agent run: %+v", resp)
	}
	// The child ran on the deepseek-built model with the def's prompt.
	calls := child.streamCalls()
	if len(calls) != 1 {
		t.Fatalf("child must run on the pinned provider, calls=%d", len(calls))
	}
	sys := messageText(calls[0].Prompt[0])
	if !strings.Contains(sys, "You are the researcher.") ||
		!strings.Contains(sys, "final message is returned verbatim") {
		t.Errorf("def prompt + report tail must form the child system prompt: %q", sys)
	}
	// The pinned provider's budget overrides the parent's.
	if want := deepseekSpec.maxOutputTokens(deepseekDefaultModel); calls[0].MaxOutputTokens == nil ||
		*calls[0].MaxOutputTokens != want {
		t.Errorf("pinned sub-agent budget = %v want %d", calls[0].MaxOutputTokens, want)
	}
	if len(parent.streamCalls()) != 0 {
		t.Error("parent model must not be called for a pinned subagent")
	}

	resp = runTool(t, tool, agentTaskParams{Prompt: "x", Agent: "nope"})
	if !resp.IsError || !strings.Contains(resp.Content, "unknown agent") {
		t.Errorf("unknown agent must error: %+v", resp)
	}
}

func TestAgentTaskTool_Background(t *testing.T) {
	isolateHome(t)
	env, msgs := newTestToolEnv(t)
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("bg report", fantasy.Usage{}),
	}}
	tool := agentTaskTool(env,
		func() fantasy.LanguageModel { return lm },
		func() int64 { return 0 })

	resp := runTool(t, tool, agentTaskParams{Prompt: "investigate", RunInBackground: true})
	if resp.IsError || !strings.Contains(resp.Content, "job-") {
		t.Fatalf("background start: %+v", resp)
	}
	jobID := ""
	for _, f := range strings.Fields(resp.Content) {
		if strings.HasPrefix(f, "job-") {
			jobID = strings.TrimRight(f, ";,.")
			break
		}
	}
	job := env.jobs.get(jobID)
	if job == nil {
		t.Fatalf("job %q must be registered", jobID)
	}
	select {
	case <-job.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("background agent must finish")
	}
	out, _, done, result := job.snapshot()
	if !done || result.exitCode != 0 || out != "bg report" {
		t.Errorf("job result wrong: out=%q done=%v code=%d", out, done, result.exitCode)
	}

	var started, ended bool
	for _, m := range *msgs {
		switch m.(type) {
		case bgTaskStartedMsg:
			started = true
		case bgTaskEndedMsg:
			ended = true
		}
	}
	if !started || !ended {
		t.Errorf("bg task UI signals missing: started=%v ended=%v", started, ended)
	}
}

func TestAgentTaskTool_DefaultResearcherUnchanged(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)
	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("the answer", fantasy.Usage{}),
	}}
	tool := agentTaskTool(env,
		func() fantasy.LanguageModel { return lm },
		func() int64 { return 222 })
	resp := runTool(t, tool, agentTaskParams{Prompt: "what is where"})
	if resp.IsError || resp.Content != "the answer" {
		t.Fatalf("default task run: %+v", resp)
	}
	sys := messageText(lm.streamCalls()[0].Prompt[0])
	if !strings.Contains(sys, "read-only research sub-agent") {
		t.Errorf("default system prompt must be the researcher: %q", sys)
	}
	// The default researcher inherits the parent session's budget.
	if got := lm.streamCalls()[0].MaxOutputTokens; got == nil || *got != 222 {
		t.Errorf("inherited sub-agent budget = %v want 222", got)
	}
	if resp = runTool(t, tool, agentTaskParams{}); !resp.IsError {
		t.Error("empty prompt must error")
	}
}

func TestRunTurn_SkillExpansionReachesWire(t *testing.T) {
	home := isolateHome(t)
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "ship", "", "Ship procedure body.")

	lm := &fakeLM{turns: [][]fantasy.StreamPart{
		textTurn("ok", fantasy.Usage{}),
	}}
	s := newTestAgentSession(t, lm, nil)
	if err := s.queueTurn("/ship to prod"); err != nil {
		t.Fatal(err)
	}
	readSessionMsgs(t, s.ch, isTurnComplete)
	wire := lm.streamCalls()[0].Prompt
	var userText string
	for _, m := range wire {
		if m.Role == fantasy.MessageRoleUser {
			userText = messageText(m)
		}
	}
	if !strings.Contains(userText, "Ship procedure body.") || !strings.Contains(userText, "arguments: to prod") {
		t.Errorf("skill must expand on the wire: %q", userText)
	}
}
