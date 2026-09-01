package tools

import (
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/workflow"
)

func workflowToolByName(t *testing.T, env *ToolEnv, name string) Tool {
	t.Helper()
	for _, tool := range WorkflowTools(env) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("workflow tool %q missing", name)
	return nil
}

func TestWorkflowTools_CoversEveryWorkflowTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	want := []string{
		"workflow_list", "workflow_get", "workflow_create",
		"workflow_edit", "workflow_delete", "workflow_copy",
	}
	got := map[string]bool{}
	for _, tool := range WorkflowTools(env) {
		got[tool.Name()] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing workflow core tool %s", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tool count %d want %d: %v", len(got), len(want), got)
	}
}

func TestWorkflowCRUDRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env, _ := newTestToolEnv(t)

	list := workflowToolByName(t, env, "workflow_list")
	resp, err := RunToolWithJSON(testAgentCtx(), list, `{}`)
	if err != nil || resp.IsError {
		t.Fatalf("list: %+v %v", resp, err)
	}

	create := workflowToolByName(t, env, "workflow_create")
	resp, err = RunToolWithJSON(testAgentCtx(), create, `{"name":"review","steps":[{"name":"step1","provider":"deepseek","prompt":"review the issue"}]}`)
	if err != nil || resp.IsError {
		t.Fatalf("create: %+v %v", resp, err)
	}

	items := workflow.ListAll(env.Cwd)
	if len(items) != 1 || items[0].Name != "review" {
		t.Fatalf("workflow must persist: %+v", items)
	}

	// Duplicate create gates
	resp, _ = RunToolWithJSON(testAgentCtx(), create, `{"name":"review","steps":[{"name":"s","provider":"deepseek","prompt":"p"}]}`)
	if !resp.IsError || !strings.Contains(resp.Content, "already exists") {
		t.Errorf("duplicate create must error: %+v", resp)
	}

	// Delete
	del := workflowToolByName(t, env, "workflow_delete")
	resp, err = RunToolWithJSON(testAgentCtx(), del, `{"name":"review"}`)
	if err != nil || resp.IsError {
		t.Fatalf("delete: %+v %v", resp, err)
	}
	items = workflow.ListAll(env.Cwd)
	if len(items) != 0 {
		t.Fatalf("delete should leave 0 workflows: %+v", items)
	}
}

func TestWorkflowGetAndList_RenderFullDefinitionAsContentText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env, _ := newTestToolEnv(t)

	create := workflowToolByName(t, env, "workflow_create")
	createJSON := `{
		"name": "ship-render",
		"scope": "global",
		"description": "Ship code changes safely",
		"steps": [
			{
				"name": "Validate plan",
				"provider": "vertex",
				"model": "gemini-3.7-flash",
				"prompt": "Review task and produce plan"
			},
			{
				"name": "Implement and validate",
				"kind": "loop",
				"maxIterations": 15,
				"exitCondition": "Validation passes completely",
				"steps": [
					{"name": "Execute work", "provider": "vertex", "prompt": "Make changes and run tests"},
					{"name": "Validate implementation", "provider": "vertex", "prompt": "Review changes against plan"}
				]
			}
		]
	}`
	if resp, err := RunToolWithJSON(testAgentCtx(), create, createJSON); err != nil || resp.IsError {
		t.Fatalf("create failed: %+v %v", resp, err)
	}

	// workflow_get must render the whole definition — including every step
	// prompt and the loop exit condition — into the human-readable content
	// text (the MCP TextContent). Providers that forward only the text field
	// (claudecode) and session resume both drop the structured data payload,
	// so the definition has to live in the text or the model never sees it.
	// Assert on the isolated content text, not the whole response map, so the
	// structured data (which also carries prompts) cannot mask a regression.
	getRes, _, err := WorkflowGetCore(env.Cwd, WorkflowGetInput{Name: "ship-render"})
	if err != nil || getRes.IsError {
		t.Fatalf("get failed: %+v %v", getRes, err)
	}
	getContent := MCPResultText(getRes)
	wantGet := []string{
		"Workflow: ship-render (scope: global)",
		"Description: Ship code changes safely",
		"1. Validate plan",
		"provider=vertex",
		"model=gemini-3.7-flash",
		"Review task and produce plan",
		"2. Implement and validate",
		"maxIterations=15",
		"Exit condition: Validation passes completely",
		"2.1. Execute work",
		"Make changes and run tests",
		"2.2. Validate implementation",
		"Review changes against plan",
	}
	for _, want := range wantGet {
		if !strings.Contains(getContent, want) {
			t.Errorf("workflow_get content missing %q\ngot:\n%s", want, getContent)
		}
	}

	// workflow_list must surface names, scopes, descriptions and the step
	// outline, but deliberately omit prompts to keep the listing small.
	listRes, _, err := WorkflowListCore(env.Cwd, WorkflowListInput{})
	if err != nil || listRes.IsError {
		t.Fatalf("list failed: %+v %v", listRes, err)
	}
	listContent := MCPResultText(listRes)
	wantList := []string{
		"ship-render (scope: global)",
		"Description: Ship code changes safely",
		"Validate plan [agent]",
		"Implement and validate [loop]",
	}
	for _, want := range wantList {
		if !strings.Contains(listContent, want) {
			t.Errorf("workflow_list content missing %q\ngot:\n%s", want, listContent)
		}
	}
	if strings.Contains(listContent, "Make changes and run tests") {
		t.Errorf("workflow_list content must omit step prompts\ngot:\n%s", listContent)
	}
}

func TestRenderWorkflowDef_BranchesAndDefaults(t *testing.T) {
	if got := workflowScopeLabel(workflow.Def{}); got != "user" {
		t.Errorf("empty scope should default to user, got %q", got)
	}
	if got := workflowScopeLabel(workflow.Def{Scope: workflow.ScopeRepo}); got != "repo" {
		t.Errorf("explicit scope should pass through, got %q", got)
	}

	// One definition exercising the branches the create→get scenario does
	// not: a bare agent step (no provider/model/prompt), a provider-only
	// step, a multi-line prompt, a loop with no maxIterations and no exit
	// condition, and an empty description under a plugin scope.
	d := workflow.Def{
		Name:   "edge",
		Plugin: "kit@market",
		Scope:  workflow.ScopePlugin,
		Steps: []workflow.Step{
			{Name: "bare"},
			{Name: "provider only", Provider: "vertex"},
			{Name: "multiline", Provider: "vertex", Model: "gemini", Prompt: "line one\nline two"},
			{Name: "loop no meta", Kind: "loop", Steps: []workflow.Step{{Name: "inner"}}},
		},
	}
	got := renderWorkflowDef(d)
	want := []string{
		"Workflow: edge (scope: plugin) [plugin: kit@market]",
		"Description: (none)",
		"1. bare  [agent]",
		"2. provider only  [agent · provider=vertex]",
		"3. multiline  [agent · provider=vertex · model=gemini]",
		"     line one",
		"     line two",
		"4. loop no meta  [loop]",
		"   4.1. inner  [agent]",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("renderWorkflowDef missing %q\ngot:\n%s", w, got)
		}
	}
	if n := strings.Count(got, "Prompt:"); n != 1 {
		t.Errorf("only the one step with a prompt should render a Prompt block, got %d\n%s", n, got)
	}
	if strings.Contains(got, "maxIterations=") {
		t.Errorf("loop without a cap must not render maxIterations\n%s", got)
	}
	if strings.Contains(got, "Exit condition:") {
		t.Errorf("loop without an exit condition must not render one\n%s", got)
	}
}

func TestRenderWorkflowList_BranchesAndDefaults(t *testing.T) {
	if got := renderWorkflowList(nil); got != "No workflows are defined." {
		t.Errorf("empty list should be explicit, got %q", got)
	}

	defs := []workflow.Def{
		{Name: "alpha", Scope: workflow.ScopeGlobal, Steps: []workflow.Step{{Name: "one"}}},
		{
			Name:        "beta",
			Scope:       workflow.ScopePlugin,
			Plugin:      "kit@market",
			Description: "does beta",
			Steps: []workflow.Step{
				{Name: "x", Prompt: "secret prompt"},
				{Name: "loopy", Kind: "loop", Steps: []workflow.Step{{Name: "i"}}},
			},
		},
	}
	got := renderWorkflowList(defs)
	want := []string{
		"2 workflow(s):",
		"alpha (scope: global)",
		"  Description: (no description)",
		"  Steps: one [agent]",
		"beta (scope: plugin) [plugin: kit@market]",
		"  Description: does beta",
		"  Steps: x [agent] → loopy [loop]",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("renderWorkflowList missing %q\ngot:\n%s", w, got)
		}
	}
	if strings.Contains(got, "secret prompt") {
		t.Errorf("listing must omit step prompts\n%s", got)
	}
}

func TestWorkflowGetCore_NotFoundReturnsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env, _ := newTestToolEnv(t)

	res, out, err := WorkflowGetCore(env.Cwd, WorkflowGetInput{Name: "does-not-exist"})
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an error result for a missing workflow, got %+v", res)
	}
	if out.Workflow.Name != "" || len(out.Workflow.Steps) != 0 {
		t.Errorf("error result must carry an empty workflow, got %+v", out.Workflow)
	}
	if txt := MCPResultText(res); !strings.Contains(txt, "does-not-exist") || !strings.Contains(txt, "not found") {
		t.Errorf("error text should name the missing workflow, got %q", txt)
	}
}

func TestWorkflowListCore_EmptyRendersExplicitText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env, _ := newTestToolEnv(t)

	res, out, err := WorkflowListCore(env.Cwd, WorkflowListInput{})
	if err != nil || res.IsError {
		t.Fatalf("list failed: %+v %v", res, err)
	}
	if len(out.Workflows) != 0 {
		t.Fatalf("expected no workflows in a fresh HOME, got %d", len(out.Workflows))
	}
	if txt := MCPResultText(res); txt != "No workflows are defined." {
		t.Errorf("empty listing content should be explicit, got %q", txt)
	}
}

func TestWorkflowTools_LoopWorkflowPromptAndExitConditionPreservation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env, _ := newTestToolEnv(t)

	create := workflowToolByName(t, env, "workflow_create")
	get := workflowToolByName(t, env, "workflow_get")
	edit := workflowToolByName(t, env, "workflow_edit")
	copyTool := workflowToolByName(t, env, "workflow_copy")

	// 1. Create a loop workflow
	createJSON := `{
		"name": "ship-test",
		"scope": "global",
		"description": "Ship code changes safely",
		"steps": [
			{
				"name": "Validate plan",
				"provider": "vertex",
				"model": "gemini-3.7-flash",
				"prompt": "Review task and produce plan"
			},
			{
				"name": "Implement and validate",
				"kind": "loop",
				"maxIterations": 15,
				"exitCondition": "Validation passes completely",
				"steps": [
					{
						"name": "Execute work",
						"provider": "vertex",
						"model": "gemini-3.7-flash",
						"prompt": "Make changes and run tests"
					},
					{
						"name": "Validate implementation",
						"provider": "vertex",
						"model": "gemini-3.7-flash",
						"prompt": "Review changes against plan"
					}
				]
			}
		]
	}`

	resp, err := RunToolWithJSON(testAgentCtx(), create, createJSON)
	if err != nil || resp.IsError {
		t.Fatalf("create failed: %+v %v", resp, err)
	}

	// 2. Get the workflow and verify ExitCondition, Prompt, and inner Steps
	resp, err = RunToolWithJSON(testAgentCtx(), get, `{"name":"ship-test"}`)
	if err != nil || resp.IsError {
		t.Fatalf("get failed: %+v %v", resp, err)
	}
	listOutput, err := RunToolWithJSON(testAgentCtx(), get, `{"name":"ship-test","scope":"global"}`)
	if err != nil || listOutput.IsError {
		t.Fatalf("scoped get failed: %+v %v", listOutput, err)
	}

	wfs := workflow.ListAll(env.Cwd)
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow in store, got %d", len(wfs))
	}
	wf := wfs[0]
	if len(wf.Steps) != 2 || wf.Steps[1].Kind != "loop" || wf.Steps[1].ExitCondition != "Validation passes completely" {
		t.Errorf("expected exit condition to persist in store: %+v", wf.Steps[1])
	}
	if len(wf.Steps[1].Steps) != 2 || wf.Steps[1].Steps[0].Prompt != "Make changes and run tests" {
		t.Errorf("expected inner step prompts to persist: %+v", wf.Steps[1].Steps)
	}

	// 3. Edit description and steps
	editJSON := `{
		"name": "ship-test",
		"description": "Updated description",
		"steps": [
			{
				"name": "Validate plan updated",
				"provider": "vertex",
				"model": "gemini-3.7-flash",
				"prompt": "Updated plan prompt"
			},
			{
				"name": "Implement and validate updated",
				"kind": "loop",
				"maxIterations": 10,
				"exitCondition": "Updated exit condition",
				"steps": [
					{
						"name": "Execute work updated",
						"provider": "vertex",
						"model": "gemini-3.7-flash",
						"prompt": "Updated execute prompt"
					}
				]
			}
		]
	}`
	resp, err = RunToolWithJSON(testAgentCtx(), edit, editJSON)
	if err != nil || resp.IsError {
		t.Fatalf("edit failed: %+v %v", resp, err)
	}

	wfs = workflow.ListAll(env.Cwd)
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow after edit, got %d", len(wfs))
	}
	wf = wfs[0]
	if wf.Description != "Updated description" {
		t.Errorf("expected updated description, got %q", wf.Description)
	}
	if wf.Steps[1].ExitCondition != "Updated exit condition" {
		t.Errorf("expected updated exit condition, got %q", wf.Steps[1].ExitCondition)
	}
	if wf.Steps[1].Steps[0].Prompt != "Updated execute prompt" {
		t.Errorf("expected updated inner prompt, got %q", wf.Steps[1].Steps[0].Prompt)
	}

	// 4. Copy to user scope
	copyJSON := `{"name":"ship-test","scope":"global","to":"user","new_name":"ship-user"}`
	resp, err = RunToolWithJSON(testAgentCtx(), copyTool, copyJSON)
	if err != nil || resp.IsError {
		t.Fatalf("copy failed: %+v %v", resp, err)
	}

	copiedDef, err := workflow.ResolveByName(env.Cwd, "ship-user", workflow.ScopeUser)
	if err != nil {
		t.Fatalf("failed to resolve copied user workflow: %v", err)
	}
	if copiedDef.Description != "Updated description" {
		t.Errorf("expected description preserved on copy, got %q", copiedDef.Description)
	}
	if len(copiedDef.Steps) != 2 || copiedDef.Steps[1].ExitCondition != "Updated exit condition" {
		t.Errorf("expected exit condition preserved on copy: %+v", copiedDef.Steps)
	}
	if len(copiedDef.Steps[1].Steps) != 1 || copiedDef.Steps[1].Steps[0].Prompt != "Updated execute prompt" {
		t.Errorf("expected inner prompt preserved on copy: %+v", copiedDef.Steps[1].Steps)
	}
}
