package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Cidan/ask/pkg/engine"
)

const TodosToolDescription = `Replace your task list for this session. The user watches this list live — it is the progress UI for long tasks, so it must track reality at every moment, not retrospectively.

Cadence contract — one call per transition:
  - Plan: create the list and mark the first item in_progress BEFORE you start working on it.
  - The moment an item is done: call todos again, marking it completed and the next item in_progress in the same call.
  - Never batch: doing all the work and then reporting every item completed in one final call is a failure mode — the user stared at a stale list the whole run.

Send the FULL list every time (it replaces the previous one). Keep exactly one item in_progress while work is underway. Skip the tool entirely for trivial single-step tasks.`

type TodoEntry struct {
	Content    string `json:"content" jsonschema:"imperative description of the task"`
	Status     string `json:"status" jsonschema:"current state of the task (pending, in_progress, completed)"`
	ActiveForm string `json:"active_form,omitempty" jsonschema:"present-continuous label shown while the task is in_progress"`
}

type TodosParams struct {
	Todos       []TodoEntry `json:"todos" jsonschema:"the complete task list, replacing any previous list"`
	Description string      `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// TodosResult is the todos tool's response.
type TodosResult struct {
	Applied   bool   `json:"applied,omitempty" jsonschema:"true when the list was accepted"`
	Total     int    `json:"total,omitempty" jsonschema:"number of todos in the list"`
	Completed int    `json:"completed,omitempty" jsonschema:"number of completed todos"`
	Nudge     string `json:"nudge,omitempty" jsonschema:"reminder about when to call todos again"`
	Notice    string `json:"notice,omitempty" jsonschema:"guidance the caller must act on before retrying"`
}

// TodosTool returns the native task list management tool.
func TodosTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"todos",
		TodosToolDescription,
		func(ctx agent.Context, p TodosParams) (TodosResult, error) {
			if env != nil && env.IsSubagent {
				return TodosResult{Applied: true, Nudge: "subagent task list ignored - isolated from parent session"}, nil
			}
			inProgress := 0
			completed := 0
			items := make([]engine.TodoItem, 0, len(p.Todos))
			for i, td := range p.Todos {
				switch td.Status {
				case "pending":
				case "in_progress":
					inProgress++
				case "completed":
					completed++
				default:
					return TodosResult{}, fmt.Errorf(
						"todos[%d] has invalid status %q (want pending, in_progress, or completed)", i, td.Status)
				}
				if td.Content == "" {
					return TodosResult{}, fmt.Errorf("todos[%d] has empty content", i)
				}
				items = append(items, engine.TodoItem{
					Content:    td.Content,
					ActiveForm: td.ActiveForm,
					Status:     td.Status,
				})
			}
			if inProgress > 1 {
				return TodosResult{}, errors.New("keep at most one todo in_progress at a time")
			}
			if env.Emit != nil {
				env.Emit(engine.TodoUpdateEvent{
					BaseEvent: engine.BaseEvent{TabID: env.TabID},
					Todos:     items,
				})
			}
			note := ""
			switch {
			case inProgress == 1 && completed < len(items):
				note = " — call todos again the moment the in_progress item is done; do not batch completions"
			case inProgress == 0 && completed < len(items):
				note = " — no item is in_progress; mark the one you are about to work on before continuing"
			}
			return TodosResult{Applied: true, Total: len(items), Completed: completed, Nudge: strings.TrimPrefix(note, " — ")}, nil
		},
	)
}

const (
	// todoReminderListSteps is how many steps a task list with open items
	// may go unchanged before each reminder; todoReminderNoListSteps is the
	// one step count at which a turn with no open list is reminded.
	todoReminderListSteps   = 5
	todoReminderNoListSteps = 8

	todoReminderInProgress = "Your task list marks %q as in progress and has not changed in the last %d steps. If that item is done, call todos now: mark it completed and the next item in_progress."
	todoReminderStalled    = "Your task list has pending items but none in progress, and has not changed in the last %d steps. Call todos to mark the item you are working on in_progress."
	todoReminderNoList     = "You have run %d tool-calling steps without a task list update. If you have a task list, update it to match your progress; if the task has several steps and no list yet, create one with todos. If the task is a single lookup, ignore this reminder."
	todoReminderQuiet      = " Do not mention this reminder to the user."
)

// TodoReminderHook is a request processor, not a callable tool: before each
// model call it counts the steps since the turn's last applied todos call
// and, every few steps, appends a reminder to keep the task list current as
// a user content of its own. The reminder lives in that one request only;
// the next request is rebuilt from the session without it.
//
// Register it after preload_memory and before inject_tool_images: the
// recall block attaches to the last user text content, which the reminder
// would otherwise be, and an injected image content would read as the
// turn's user message.
type TodoReminderHook struct {
	env *ToolEnv
}

// NewTodoReminderHook builds the reminder hook. A subagent's env makes it a
// no-op, since a subagent's task list is ignored.
func NewTodoReminderHook(env *ToolEnv) *TodoReminderHook {
	return &TodoReminderHook{env: env}
}

func (h *TodoReminderHook) Name() string { return "remind_todos" }
func (h *TodoReminderHook) Description() string {
	return "Reminds the model to keep its task list current."
}
func (h *TodoReminderHook) IsLongRunning() bool { return false }

// ProcessRequest implements ADK's request processor.
func (h *TodoReminderHook) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	if h == nil || req == nil || (h.env != nil && h.env.IsSubagent) {
		return nil
	}
	if text := todoReminder(req.Contents); text != "" {
		req.Contents = append(req.Contents, genai.NewContentFromText(engine.ReminderBlock(text+todoReminderQuiet), genai.RoleUser))
	}
	return nil
}

// todoReminder returns the reminder this request carries, or "". A step is
// one model content that calls tools, however many calls it makes in
// parallel. Steps count from the turn's user message, or from the latest
// todos call the tool applied, whose list decides the wording.
func todoReminder(contents []*genai.Content) string {
	start := lastUserTextContent(contents)
	applied := map[string]bool{}
	steps := 0
	for i := len(contents) - 1; i > start; i-- {
		c := contents[i]
		if c == nil {
			continue
		}
		if c.Role != genai.RoleModel {
			for _, p := range c.Parts {
				if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "todos" {
					ok, _ := p.FunctionResponse.Response["applied"].(bool)
					applied[p.FunctionResponse.ID] = ok
				}
			}
			continue
		}
		if !hasFunctionCall(c) {
			continue
		}
		if list, ok := appliedTodos(c, applied); ok {
			return todoReminderFor(list, steps)
		}
		steps++
	}
	return todoReminderFor(nil, steps)
}

func hasFunctionCall(c *genai.Content) bool {
	for _, p := range c.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

// appliedTodos returns the list of c's last todos call whose response
// reports it applied.
func appliedTodos(c *genai.Content, applied map[string]bool) ([]TodoEntry, bool) {
	for j := len(c.Parts) - 1; j >= 0; j-- {
		p := c.Parts[j]
		if p == nil || p.FunctionCall == nil || p.FunctionCall.Name != "todos" || !applied[p.FunctionCall.ID] {
			continue
		}
		raw, err := json.Marshal(p.FunctionCall.Args)
		if err != nil {
			continue
		}
		var params TodosParams
		if err := json.Unmarshal(raw, &params); err != nil {
			continue
		}
		return params.Todos, true
	}
	return nil, false
}

// todoReminderFor words the reminder for list, unchanged for steps steps.
// A list with no open item counts as no list.
func todoReminderFor(list []TodoEntry, steps int) string {
	inProgress, pending := "", 0
	for _, td := range list {
		switch td.Status {
		case "in_progress":
			inProgress = td.Content
		case "pending":
			pending++
		}
	}
	due := steps > 0 && steps%todoReminderListSteps == 0
	switch {
	case inProgress != "" && due:
		return fmt.Sprintf(todoReminderInProgress, inProgress, steps)
	case inProgress == "" && pending > 0 && due:
		return fmt.Sprintf(todoReminderStalled, steps)
	case inProgress == "" && pending == 0 && steps == todoReminderNoListSteps:
		return fmt.Sprintf(todoReminderNoList, steps)
	}
	return ""
}
