package tools

import (
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/engine"
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

func TestTodosTool_SubagentIsolation(t *testing.T) {
	var events []engine.EngineEvent
	parentEnv := NewToolEnv(t.TempDir(), 1, true, func(ev engine.EngineEvent) {
		events = append(events, ev)
	}, nil)

	subEnv := NewSubagentToolEnv(parentEnv, "sub-123")
	if !subEnv.IsSubagent || subEnv.SubagentID != "sub-123" {
		t.Fatalf("subagent env not initialized properly: %+v", subEnv)
	}

	tool := TodosTool(subEnv)
	resp := runTool(t, tool, TodosParams{Todos: []TodoEntry{
		{Content: "sub task 1", Status: "in_progress"},
		{Content: "sub task 2", Status: "pending"},
	}})

	if resp.IsError {
		t.Fatalf("unexpected error response: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "subagent task list ignored") {
		t.Errorf("expected isolation notice, got %q", resp.Content)
	}
	if len(events) > 0 {
		t.Errorf("subagent todos call must not emit events to parent listener: got %d events", len(events))
	}
}
