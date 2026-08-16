package tools

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/workflow"
)

func workflowToolByName(t *testing.T, env *ToolEnv, name string) fantasy.AgentTool {
	t.Helper()
	for _, tool := range WorkflowTools(env) {
		if tool.Info().Name == name {
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
		"clear_plans",
	}
	got := map[string]bool{}
	for _, tool := range WorkflowTools(env) {
		got[tool.Info().Name] = true
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
	resp, err := list.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "workflow_list", Input: `{}`})
	if err != nil || resp.IsError {
		t.Fatalf("list: %+v %v", resp, err)
	}

	create := workflowToolByName(t, env, "workflow_create")
	resp, err = create.Run(context.Background(), fantasy.ToolCall{
		ID: "2", Name: "workflow_create",
		Input: `{"name":"review","steps":[{"name":"step1","provider":"deepseek","prompt":"review the issue"}]}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("create: %+v %v", resp, err)
	}

	items := workflow.ListAll(env.Cwd)
	if len(items) != 1 || items[0].Name != "review" {
		t.Fatalf("workflow must persist: %+v", items)
	}

	// Duplicate create gates
	resp, _ = create.Run(context.Background(), fantasy.ToolCall{
		ID: "3", Name: "workflow_create",
		Input: `{"name":"review","steps":[{"name":"s","provider":"deepseek","prompt":"p"}]}`,
	})
	if !resp.IsError || !strings.Contains(resp.Content, "already exists") {
		t.Errorf("duplicate create must error: %+v", resp)
	}

	// Delete
	del := workflowToolByName(t, env, "workflow_delete")
	resp, err = del.Run(context.Background(), fantasy.ToolCall{ID: "4", Name: "workflow_delete", Input: `{"name":"review"}`})
	if err != nil || resp.IsError {
		t.Fatalf("delete: %+v %v", resp, err)
	}
	items = workflow.ListAll(env.Cwd)
	if len(items) != 0 {
		t.Fatalf("delete should leave 0 workflows: %+v", items)
	}
}

func TestClearPlansTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	clearTool := workflowToolByName(t, env, "clear_plans")

	writeTestFile(t, env.Cwd, "ask/plans/start/plan.md", "# Plan")
	writeTestFile(t, env.Cwd, "ask/plans/step-1/notes.md", "Notes")

	resp, err := clearTool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "clear_plans", Input: `{}`})
	if err != nil || resp.IsError {
		t.Fatalf("clear_plans failed: %+v %v", resp, err)
	}

	if !strings.Contains(resp.Content, "cleared") {
		t.Errorf("unexpected clear response: %q", resp.Content)
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

	resp, err := create.Run(context.Background(), fantasy.ToolCall{ID: "c1", Name: "workflow_create", Input: createJSON})
	if err != nil || resp.IsError {
		t.Fatalf("create failed: %+v %v", resp, err)
	}

	// 2. Get the workflow and verify ExitCondition, Prompt, and inner Steps
	resp, err = get.Run(context.Background(), fantasy.ToolCall{ID: "g1", Name: "workflow_get", Input: `{"name":"ship-test"}`})
	if err != nil || resp.IsError {
		t.Fatalf("get failed: %+v %v", resp, err)
	}
	// Verify response or parsed output contains the fields
	listOutput, err := get.Run(context.Background(), fantasy.ToolCall{ID: "g2", Name: "workflow_get", Input: `{"name":"ship-test","scope":"global"}`})
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
	resp, err = edit.Run(context.Background(), fantasy.ToolCall{ID: "e1", Name: "workflow_edit", Input: editJSON})
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
	resp, err = copyTool.Run(context.Background(), fantasy.ToolCall{ID: "cp1", Name: "workflow_copy", Input: copyJSON})
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
