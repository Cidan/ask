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
