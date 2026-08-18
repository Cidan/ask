package tools

import (
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/workflow"
)

func TestTodosTool(t *testing.T) {
	env, events := newTestToolEnv(t)
	tool := TodosTool(env)
	resp := runTool(t, tool, TodosParams{Todos: []TodoEntry{
		{Content: "first", Status: "completed"},
		{Content: "second", Status: "in_progress", ActiveForm: "Doing second"},
		{Content: "third", Status: "pending"},
	}})
	if resp.IsError {
		t.Fatalf("todos: %s", resp.Content)
	}
	var got []engine.TodoItem
	for _, m := range *events {
		if tm, ok := m.(engine.TodoUpdateEvent); ok {
			got = tm.Todos
		}
	}
	if len(got) != 3 || got[1].ActiveForm != "Doing second" || got[0].Status != "completed" {
		t.Errorf("TodoUpdateEvent payload wrong: %+v", got)
	}
	if !strings.Contains(resp.Content, "the moment the in_progress item is done") {
		t.Errorf("in-flight ack should nudge the next update; got %q", resp.Content)
	}

	// Pending items but nothing in_progress -> nudge to start one.
	resp = runTool(t, tool, TodosParams{Todos: []TodoEntry{
		{Content: "a", Status: "completed"},
		{Content: "b", Status: "pending"},
	}})
	if !strings.Contains(resp.Content, "no item is in_progress") {
		t.Errorf("stalled list should nudge starting an item; got %q", resp.Content)
	}

	// Everything completed -> clean ack, no nudge.
	resp = runTool(t, tool, TodosParams{Todos: []TodoEntry{
		{Content: "a", Status: "completed"},
	}})
	if strings.Contains(resp.Content, "—") {
		t.Errorf("fully-completed list should ack without a nudge; got %q", resp.Content)
	}

	if resp = runTool(t, tool, TodosParams{Todos: []TodoEntry{{Content: "x", Status: "bogus"}}}); !resp.IsError {
		t.Error("invalid status must error")
	}
	if resp = runTool(t, tool, TodosParams{Todos: []TodoEntry{
		{Content: "a", Status: "in_progress"},
		{Content: "b", Status: "in_progress"},
	}}); !resp.IsError {
		t.Error("two in_progress items must error")
	}
}

func TestTodosWorkflowGuard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := workflow.SaveAll(cwd, []workflow.Def{
		{Name: "ship-it", Scope: workflow.ScopeRepo, Steps: []workflow.Step{
			{Name: "do", Provider: "deepseek", Model: "deepseek-chat", Prompt: "go"},
		}},
	}); err != nil {
		t.Fatalf("workflow.SaveAll: %v", err)
	}

	var events []engine.EngineEvent
	env := NewToolEnv(cwd, 1, true, true, func(ev engine.EngineEvent) { events = append(events, ev) }, nil)
	if !env.WorkflowsAvailable {
		t.Fatal("env.WorkflowsAvailable should be true when the project defines a workflow")
	}
	tool := TodosTool(env)

	list := TodosParams{Todos: []TodoEntry{
		{Content: "a", Status: "in_progress"},
		{Content: "b", Status: "pending"},
	}}

	// First call: rejected, list NOT applied
	resp := runTool(t, tool, list)
	if resp.IsError {
		t.Fatalf("guard notice should not be an error response: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "NOT applied") || !strings.Contains(resp.Content, "workflow_list") {
		t.Errorf("guard should steer to workflow_list; got %q", resp.Content)
	}
	for _, m := range events {
		if _, ok := m.(engine.TodoUpdateEvent); ok {
			t.Fatal("rejected todos call must not emit a TodoUpdateEvent")
		}
	}

	// Second call: guard already fired this session -> list goes through.
	events = nil
	resp = runTool(t, tool, list)
	if resp.IsError || strings.Contains(resp.Content, "NOT applied") {
		t.Fatalf("guard must fire at most once; second call should apply: %q", resp.Content)
	}
	applied := false
	for _, m := range events {
		if _, ok := m.(engine.TodoUpdateEvent); ok {
			applied = true
		}
	}
	if !applied {
		t.Error("second todos call should apply the list")
	}
}

func TestTodosWorkflowGuard_DisarmedByCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := workflow.SaveAll(cwd, []workflow.Def{
		{Name: "ship-it", Scope: workflow.ScopeRepo, Steps: []workflow.Step{
			{Name: "do", Provider: "deepseek", Model: "deepseek-chat", Prompt: "go"},
		}},
	}); err != nil {
		t.Fatalf("workflow.SaveAll: %v", err)
	}

	var events []engine.EngineEvent
	env := NewToolEnv(cwd, 1, true, true, func(ev engine.EngineEvent) { events = append(events, ev) }, nil)
	wfTools := WorkflowTools(env)
	var listTool Tool
	for _, wt := range wfTools {
		if wt.Info().Name == "workflow_list" {
			listTool = wt
			break
		}
	}
	if listTool == nil {
		t.Fatal("workflow_list tool not found")
	}

	// Calling workflow_list directly disarms stage 1.
	runTool(t, listTool, WorkflowListInput{})
	if !env.WorkflowsChecked {
		t.Fatal("calling workflow_list must set env.WorkflowsChecked=true")
	}

	todosTool := TodosTool(env)
	list := TodosParams{Todos: []TodoEntry{
		{Content: "a", Status: "in_progress"},
	}}

	// Stage 2 decision guard fires because no workflow was proposed.
	resp := runTool(t, todosTool, list)
	if !strings.Contains(resp.Content, "You consulted the workflows but you are now about to do this work inline") {
		t.Fatalf("expected decision guard notice, got %q", resp.Content)
	}

	// Re-sending passes stage 2.
	resp = runTool(t, todosTool, list)
	if strings.Contains(resp.Content, "NOT applied") {
		t.Fatalf("second call after decision guard must apply, got: %q", resp.Content)
	}
}
