package tools

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

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

// reminderConvo builds the contents a model request carries: user messages,
// and model steps each followed by the responses to its calls.
type reminderConvo struct {
	contents []*genai.Content
	seq      int
}

func (c *reminderConvo) user(text string) {
	c.contents = append(c.contents, genai.NewContentFromText(text, genai.RoleUser))
}

func (c *reminderConvo) reply(text string) {
	c.contents = append(c.contents, genai.NewContentFromText(text, genai.RoleModel))
}

// reads appends n steps, each calling read parallel times at once.
func (c *reminderConvo) reads(n, parallel int) {
	for range n {
		var calls []*genai.FunctionCall
		var responses []map[string]any
		for range parallel {
			calls = append(calls, &genai.FunctionCall{Name: "read", Args: map[string]any{"file_path": "a.go"}})
			responses = append(responses, map[string]any{"content": "package a"})
		}
		c.step(calls, responses)
	}
}

// todos appends one step calling todos with list, alongside a read, whose
// todos response is response.
func (c *reminderConvo) todos(response map[string]any, list ...TodoEntry) {
	items := make([]any, len(list))
	for i, td := range list {
		items[i] = map[string]any{"content": td.Content, "status": td.Status}
	}
	c.step(
		[]*genai.FunctionCall{
			{Name: "read", Args: map[string]any{"file_path": "a.go"}},
			{Name: "todos", Args: map[string]any{"todos": items, "description": "plan"}},
		},
		[]map[string]any{{"content": "package a"}, response},
	)
}

func (c *reminderConvo) step(calls []*genai.FunctionCall, responses []map[string]any) {
	reply := &genai.Content{Role: genai.RoleModel}
	results := &genai.Content{Role: genai.RoleUser}
	for i, fc := range calls {
		c.seq++
		fc.ID = fmt.Sprintf("call-%d", c.seq)
		reply.Parts = append(reply.Parts, &genai.Part{FunctionCall: fc})
		results.Parts = append(results.Parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: fc.ID, Name: fc.Name, Response: responses[i]}})
	}
	c.contents = append(c.contents, reply, results)
}

// remind runs hook over the conversation and returns the reminder text it
// appended, or "" when it appended nothing.
func (c *reminderConvo) remind(t *testing.T, hook *TodoReminderHook) string {
	t.Helper()
	req := &model.LLMRequest{Contents: slices.Clone(c.contents)}
	if err := hook.ProcessRequest(testAgentCtx(), req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	for i, content := range c.contents {
		if req.Contents[i] != content {
			t.Fatalf("content %d was replaced; the reminder must only append", i)
		}
	}
	switch extra := req.Contents[len(c.contents):]; len(extra) {
	case 0:
		return ""
	case 1:
		got := extra[0]
		if got.Role != genai.RoleUser || len(got.Parts) != 1 {
			t.Fatalf("the reminder must be one user text part: %+v", got)
		}
		text := got.Parts[0].Text
		if !strings.HasPrefix(text, "<system_reminder>\n") || !strings.HasSuffix(text, "\n</system_reminder>") {
			t.Fatalf("the reminder is not a reminder block: %q", text)
		}
		if !strings.Contains(text, "Do not mention this reminder to the user.") {
			t.Fatalf("the reminder must ask the model not to mention it: %q", text)
		}
		return text
	default:
		t.Fatalf("appended %d contents, want at most one", len(extra))
		return ""
	}
}

var todosApplied = map[string]any{"applied": true, "total": 3}

func TestTodoReminderHook_RemindsAnOpenListEveryFiveSteps(t *testing.T) {
	env, _ := newTestToolEnv(t)
	hook := NewTodoReminderHook(env)
	if hook.Name() != "remind_todos" || hook.Description() == "" || hook.IsLongRunning() {
		t.Fatal("hook identity")
	}

	var c reminderConvo
	c.user("fix the bug")
	c.todos(todosApplied,
		TodoEntry{Content: "Find the bug", Status: "completed"},
		TodoEntry{Content: "Run the tests", Status: "in_progress"},
		TodoEntry{Content: "Write the fix", Status: "pending"},
	)
	if got := c.remind(t, hook); got != "" {
		t.Fatalf("the step that updated the list is not a step without one: %q", got)
	}
	var reminded []int
	for step := 1; step <= 12; step++ {
		c.reads(1, 1)
		got := c.remind(t, hook)
		if got == "" {
			continue
		}
		reminded = append(reminded, step)
		if !strings.Contains(got, `"Run the tests"`) || !strings.Contains(got, fmt.Sprintf("last %d steps", step)) {
			t.Errorf("step %d reminder must name the in_progress item and the step count: %q", step, got)
		}
	}
	if !slices.Equal(reminded, []int{5, 10}) {
		t.Fatalf("reminded after steps %v, want [5 10]", reminded)
	}

	c.todos(todosApplied,
		TodoEntry{Content: "Find the bug", Status: "completed"},
		TodoEntry{Content: "Run the tests", Status: "completed"},
		TodoEntry{Content: "Write the fix", Status: "pending"},
	)
	c.reads(5, 1)
	if got := c.remind(t, hook); !strings.Contains(got, "pending items but none in progress") || !strings.Contains(got, "last 5 steps") {
		t.Fatalf("a list with nothing in progress is reminded to start an item: %q", got)
	}
}

func TestTodoReminderHook_RemindsATurnWithoutAListOnce(t *testing.T) {
	env, _ := newTestToolEnv(t)
	hook := NewTodoReminderHook(env)

	var c reminderConvo
	c.user("explain the build")
	var reminded []int
	for step := 1; step <= 20; step++ {
		c.reads(1, 3)
		if got := c.remind(t, hook); got != "" {
			reminded = append(reminded, step)
			if !strings.Contains(got, "8 tool-calling steps") || !strings.Contains(got, "create one with todos") {
				t.Errorf("no-list reminder wording: %q", got)
			}
		}
	}
	if !slices.Equal(reminded, []int{8}) {
		t.Fatalf("reminded after steps %v, want [8]: a parallel batch is one step, and the reminder fires once", reminded)
	}
}

// A finished list is no open list: the count starts again from the call that
// completed it.
func TestTodoReminderHook_CompletedListCountsFromItsCompletion(t *testing.T) {
	env, _ := newTestToolEnv(t)
	hook := NewTodoReminderHook(env)

	var c reminderConvo
	c.user("fix the bug")
	c.reads(3, 1)
	c.todos(todosApplied, TodoEntry{Content: "Fix the bug", Status: "completed"})
	c.reads(7, 1)
	if got := c.remind(t, hook); got != "" {
		t.Fatalf("seven steps after the list finished: %q", got)
	}
	c.reads(1, 1)
	if got := c.remind(t, hook); !strings.Contains(got, "8 tool-calling steps") {
		t.Fatalf("eight steps after the list finished: %q", got)
	}
}

// A todos call the tool rejected changed nothing, so its list does not count
// and its step is one more step without a list.
func TestTodoReminderHook_IgnoresARejectedTodosCall(t *testing.T) {
	env, _ := newTestToolEnv(t)
	hook := NewTodoReminderHook(env)

	var c reminderConvo
	c.user("fix the bug")
	c.todos(map[string]any{"error": "keep at most one todo in_progress at a time"},
		TodoEntry{Content: "a", Status: "in_progress"},
		TodoEntry{Content: "b", Status: "in_progress"},
	)
	c.reads(7, 1)
	got := c.remind(t, hook)
	if !strings.Contains(got, "8 tool-calling steps") || strings.Contains(got, `"a"`) {
		t.Fatalf("a rejected list must not count: %q", got)
	}
}

// Steps count from the turn's own user message, so a list from an earlier
// turn neither carries over nor adds its steps.
func TestTodoReminderHook_NewUserMessageStartsTheCount(t *testing.T) {
	env, _ := newTestToolEnv(t)
	hook := NewTodoReminderHook(env)

	var c reminderConvo
	c.user("fix the bug")
	c.todos(todosApplied, TodoEntry{Content: "Fix the bug", Status: "in_progress"})
	c.reads(3, 1)
	c.reply("done")
	c.user("now the docs")
	c.reads(2, 1)
	if got := c.remind(t, hook); got != "" {
		t.Fatalf("the earlier turn's list and steps carried over: %q", got)
	}
	c.reads(6, 1)
	if got := c.remind(t, hook); !strings.Contains(got, "8 tool-calling steps") || strings.Contains(got, "Fix the bug") {
		t.Fatalf("the new turn has no list of its own: %q", got)
	}
}

func TestTodoReminderHook_SubagentIsNeverReminded(t *testing.T) {
	parent, _ := newTestToolEnv(t)
	hook := NewTodoReminderHook(NewSubagentToolEnv(parent, "sub-1"))

	var c reminderConvo
	c.user("investigate")
	c.reads(8, 1)
	if got := c.remind(t, hook); got != "" {
		t.Fatalf("a subagent's task list is ignored, so it is never reminded: %q", got)
	}
}

// The recall block attaches to the last user text content and an image
// content reads as a user message, so the reminder runs between the two.
func TestCoreTools_RunTheReminderBetweenRecallAndImages(t *testing.T) {
	env, _ := newTestToolEnv(t)
	at := map[string]int{}
	for i, tl := range CoreTools(env, func() []Tool { return nil }, false) {
		at[tl.Name()] = i
	}
	recall, okRecall := at["preload_memory"]
	reminder, okReminder := at["remind_todos"]
	images, okImages := at["inject_tool_images"]
	if !okRecall || !okReminder || !okImages || recall >= reminder || reminder >= images {
		t.Fatalf("processor order: preload_memory=%d remind_todos=%d inject_tool_images=%d", recall, reminder, images)
	}
	if !IsCoreTool("remind_todos") {
		t.Fatal("remind_todos is core")
	}
}
