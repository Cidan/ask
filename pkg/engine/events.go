package engine

// EventKind identifies the type of an EngineEvent.
type EventKind string

const (
	EventKindTextDelta       EventKind = "text_delta"
	EventKindAssistantText   EventKind = "assistant_text"
	EventKindStatus          EventKind = "status"
	EventKindToolCall        EventKind = "tool_call"
	EventKindToolResult      EventKind = "tool_result"
	EventKindToolDiff        EventKind = "tool_diff"
	EventKindUsage           EventKind = "usage"
	EventKindCost            EventKind = "cost"
	EventKindModelInfo       EventKind = "model_info"
	EventKindTodoUpdate      EventKind = "todo_update"
	EventKindSubagentStarted EventKind = "subagent_started"
	EventKindSubagentEnded   EventKind = "subagent_ended"
	EventKindBgTaskStarted   EventKind = "bg_task_started"
	EventKindBgTaskEnded     EventKind = "bg_task_ended"
	EventKindDone            EventKind = "done"
	EventKindExited          EventKind = "exited"
	EventKindTurnComplete    EventKind = "turn_complete"
	EventKindWorkflowStarted EventKind = "workflow_started"
	EventKindWorkflowStep    EventKind = "workflow_step"
	EventKindWorkflowDone    EventKind = "workflow_done"
	EventKindWorkflowFailed  EventKind = "workflow_failed"
)

// EngineEvent is the common interface implemented by all events emitted by the ask engine.
type EngineEvent interface {
	Kind() EventKind
	GetTabID() int
}

// BaseEvent provides the common tabID tracking.
type BaseEvent struct {
	TabID int `json:"tab_id"`
}

func (b BaseEvent) GetTabID() int { return b.TabID }

// TextDeltaEvent is emitted as assistant text streams token-by-token.
type TextDeltaEvent struct {
	BaseEvent
	Delta string `json:"delta"`
}

func (TextDeltaEvent) Kind() EventKind { return EventKindTextDelta }

// AssistantTextEvent is emitted when a complete text block from the assistant finishes.
type AssistantTextEvent struct {
	BaseEvent
	Text string `json:"text"`
}

func (AssistantTextEvent) Kind() EventKind { return EventKindAssistantText }

// StatusEvent indicates the agent's current state (e.g. "thinking…", "running tool…").
type StatusEvent struct {
	BaseEvent
	Status string `json:"status"`
}

func (StatusEvent) Kind() EventKind { return EventKindStatus }

// ToolCallEvent is emitted when the agent calls a tool.
type ToolCallEvent struct {
	BaseEvent
	ToolUseID  string         `json:"tool_use_id"`
	ToolName   string         `json:"tool_name"`
	Input      map[string]any `json:"input"`
	Background bool           `json:"background,omitempty"`
}

func (ToolCallEvent) Kind() EventKind { return EventKindToolCall }

// ToolResultEvent is emitted when a tool completes execution.
type ToolResultEvent struct {
	BaseEvent
	ToolUseID  string `json:"tool_use_id"`
	ToolName   string `json:"tool_name"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
	Background bool   `json:"background,omitempty"`
}

func (ToolResultEvent) Kind() EventKind { return EventKindToolResult }

// ToolDiffEvent is emitted when a tool mutates a file and produces a unified diff.
type ToolDiffEvent struct {
	BaseEvent
	Path string `json:"path"`
	Diff string `json:"diff"`
}

func (ToolDiffEvent) Kind() EventKind { return EventKindToolDiff }

// UsageEvent records token consumption for an API step.
type UsageEvent struct {
	BaseEvent
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func (UsageEvent) Kind() EventKind { return EventKindUsage }

// CostEvent records estimated monetary cost for an API step.
type CostEvent struct {
	BaseEvent
	CostUSD float64 `json:"cost_usd"`
}

func (CostEvent) Kind() EventKind { return EventKindCost }

// ModelInfoEvent communicates the resolved model ID for the active session.
type ModelInfoEvent struct {
	BaseEvent
	Model string `json:"model"`
}

func (ModelInfoEvent) Kind() EventKind { return EventKindModelInfo }

// TodoItem represents a single item in the task list.
type TodoItem struct {
	Status     string `json:"status"`
	Content    string `json:"content"`
	ActiveForm string `json:"active_form,omitempty"`
}

// TodoUpdateEvent is emitted when the session's todo list is updated.
type TodoUpdateEvent struct {
	BaseEvent
	Todos []TodoItem `json:"todos"`
}

func (TodoUpdateEvent) Kind() EventKind { return EventKindTodoUpdate }

// SubagentStartedEvent is emitted when a subagent starts execution.
type SubagentStartedEvent struct {
	BaseEvent
	SubagentID  string `json:"subagent_id"`
	AgentType   string `json:"agent_type"`
	Description string `json:"description"`
	Background  bool   `json:"background,omitempty"`
}

func (SubagentStartedEvent) Kind() EventKind { return EventKindSubagentStarted }

// SubagentEndedEvent is emitted when a subagent completes execution.
type SubagentEndedEvent struct {
	BaseEvent
	SubagentID string `json:"subagent_id"`
	IsError    bool   `json:"is_error,omitempty"`
}

func (SubagentEndedEvent) Kind() EventKind { return EventKindSubagentEnded }

// BgTaskStartedEvent is emitted when a background job starts.
type BgTaskStartedEvent struct {
	BaseEvent
	JobID       string `json:"job_id"`
	Description string `json:"description"`
}

func (BgTaskStartedEvent) Kind() EventKind { return EventKindBgTaskStarted }

// BgTaskEndedEvent is emitted when a background job completes.
type BgTaskEndedEvent struct {
	BaseEvent
	JobID    string `json:"job_id"`
	ExitCode int    `json:"exit_code"`
}

func (BgTaskEndedEvent) Kind() EventKind { return EventKindBgTaskEnded }

// ResultSummary contains the outcome of a provider turn.
type ResultSummary struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
}

// DoneEvent is emitted when a turn completes.
type DoneEvent struct {
	BaseEvent
	Result ResultSummary `json:"result"`
	Error  error         `json:"error,omitempty"`
}

func (DoneEvent) Kind() EventKind { return EventKindDone }

// ExitedEvent is emitted when the session goroutine closes.
type ExitedEvent struct {
	BaseEvent
}

func (ExitedEvent) Kind() EventKind { return EventKindExited }

// TurnCompleteEvent signals the conclusion of a turn's event sequence.
type TurnCompleteEvent struct {
	BaseEvent
}

func (TurnCompleteEvent) Kind() EventKind { return EventKindTurnComplete }

// WorkflowStartedEvent is emitted when a workflow begins execution.
type WorkflowStartedEvent struct {
	BaseEvent
	Workflow string `json:"workflow"`
	Source   string `json:"source"`
}

func (WorkflowStartedEvent) Kind() EventKind { return EventKindWorkflowStarted }

// WorkflowStepStartedEvent is emitted when an individual workflow step begins.
type WorkflowStepStartedEvent struct {
	BaseEvent
	StepIdx  int    `json:"step_idx"`
	StepName string `json:"step_name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (WorkflowStepStartedEvent) Kind() EventKind { return EventKindWorkflowStep }

// WorkflowStepDoneEvent is emitted when an individual step completes.
type WorkflowStepDoneEvent struct {
	BaseEvent
	StepIdx int    `json:"step_idx"`
	Summary string `json:"summary"`
}

func (WorkflowStepDoneEvent) Kind() EventKind { return EventKindWorkflowStep }

// WorkflowDoneEvent is emitted when an entire workflow finishes.
type WorkflowDoneEvent struct {
	BaseEvent
	Description string   `json:"description"`
	Artifacts   []string `json:"artifacts"`
}

func (WorkflowDoneEvent) Kind() EventKind { return EventKindWorkflowDone }

// WorkflowFailedEvent is emitted when a workflow fails or is cancelled.
type WorkflowFailedEvent struct {
	BaseEvent
	Reason string `json:"reason"`
}

func (WorkflowFailedEvent) Kind() EventKind { return EventKindWorkflowFailed }

// EventListener is a callback function that handles stream events from the engine.
type EventListener func(event EngineEvent)
