# Ask Engine Library & Tool Interface Architecture

This document details the design, API surface, and integration pattern for embedding `ask` as a library and tool in external agent frameworks (such as Google ADK in `github.com/Cidan/zeus`).

---

## 1. Motivation & Context

Following the modular package breakout (`pkg/engine`, `pkg/providers`, `pkg/tools`, `pkg/config`, `pkg/memory`, `pkg/workflow`), `ask` can now function independently of its Bubble Tea terminal user interface.

External orchestrators, background bots, or higher-level agents (like Zeus) require a clean Go API to delegate coding tasks and queries to `ask` without managing low-level tool setup or LLM wiring.

---

## 2. Core Design Principles

1. **Self-Configuring Runtime:**
   When given a target directory (`Cwd`), the engine automatically loads:
   - Project and user rules (`.claude/rules/`)
   - Repository and global workflows (`.ask/workflows/`)
   - Agent skills (`SKILL.md`) and subagents (`.claude/agents/*.md`)
   - Local MCP servers (`.mcp.json`)
   - Workspace vector memory (sqlite-vec)

2. **Unified Top-Level Entry Point:**
   A single top-level `Run(ctx, opts)` function handles both one-shot executions and ongoing multi-turn conversations.

3. **Session Persistence & Resumption:**
   Passing an existing `SessionID` seamlessly restores the conversation transcript, tool call history, and context window from disk (`~/.config/ask/agent-sessions/<provider>/`), saving updated state atomically after each turn.

4. **Real-Time Streaming Observability:**
   Callers can optionally pass an `EventListener` callback to receive real-time streaming tokens (`TextDeltaEvent`), tool calls/results (`ToolCallEvent`, `ToolResultEvent`), status changes (`StatusEvent`), and completion summaries (`DoneEvent`).

---

## 3. Go API Surface

### Top-Level Package (`github.com/Cidan/ask` & `pkg/engine`)

```go
package ask

import (
    "context"

    "charm.land/fantasy"
    "github.com/Cidan/ask/pkg/config"
    "github.com/Cidan/ask/pkg/engine"
)

// RunOptions defines the input parameters for executing an ask agent turn.
type RunOptions struct {
    // Prompt is the user query, instruction, or task description.
    Prompt string

    // SessionID is the unique session identifier. If empty, a new session is created.
    // If provided, prior conversation turns and tool calls are loaded from disk.
    SessionID string

    // Cwd is the target working directory. Defaults to os.Getwd().
    Cwd string

    // Config optionally overrides default configuration (~/.config/ask/ask.json).
    Config config.Config

    // Provider optionally overrides the LLM provider (e.g. "anthropic", "vertex", "deepseek").
    Provider string

    // Model optionally overrides the default model for the selected provider.
    Model string

    // Effort optionally sets reasoning/thinking effort level.
    Effort string

    // Files provides optional image/media attachments for models with vision.
    Files []fantasy.FilePart

    // EventListener receives real-time streaming deltas, tool calls, and lifecycle events.
    EventListener engine.EventListener

    // InteractionHandler manages tool approval prompts and user questions.
    // Defaults to HeadlessInteractionHandler{AutoApproveTools: true}.
    InteractionHandler engine.InteractionHandler

    // SkipAllPermissions bypasses confirmation prompts for all tools.
    SkipAllPermissions bool
}

// RunResult contains the outcome of the agent turn.
type RunResult struct {
    // SessionID is the session identifier used for this turn (persisted on disk).
    SessionID string

    // Response is the final assistant text output.
    Response string

    // Messages contains the complete message history up to this point.
    Messages []fantasy.Message

    // IsError indicates whether the turn failed.
    IsError bool

    // Error contains the failure error, if any.
    Error error
}

// Run executes an ask agent turn with the provided options.
func Run(ctx context.Context, opts RunOptions) (*RunResult, error)
```

---

## 4. Integration Example: Google ADK (Zeus)

To wire `ask` into Google's ADK (`google.golang.org/adk/v2`) within Zeus, define a `functiontool` (or lazy tool) wrapping `ask.Run`:

```go
package tools

import (
    "github.com/Cidan/ask"
    "github.com/Cidan/ask/pkg/engine"
    "google.golang.org/adk/v2/agent"
    "google.golang.org/adk/v2/tool"
    "google.golang.org/adk/v2/tool/functiontool"
)

type AskCodeArgs struct {
    Status     string `json:"status" jsonschema:"Status message describing the tool action"`
    Prompt     string `json:"prompt" jsonschema:"The coding instruction or question for the ask engine"`
    WorkingDir string `json:"working_dir,omitempty" jsonschema:"Target repository directory (defaults to current cwd)"`
    SessionID  string `json:"session_id,omitempty" jsonschema:"Session ID to continue an ongoing conversation"`
}

type AskCodeResult struct {
    SessionID string `json:"session_id"`
    Response  string `json:"response"`
    Error     string `json:"error,omitempty"`
}

func NewAskCodeTool() (tool.Tool, error) {
    return functiontool.New[AskCodeArgs, AskCodeResult](functiontool.Config{
        Name:        "ask_code",
        Description: "Delegate software engineering, code search, editing, and workflow execution to the ask engine.",
    }, func(ctx agent.Context, args AskCodeArgs) (AskCodeResult, error) {
        result, err := ask.Run(ctx, ask.RunOptions{
            Prompt:    args.Prompt,
            SessionID: args.SessionID,
            Cwd:       args.WorkingDir,
            EventListener: func(ev engine.EngineEvent) {
                // Optional: forward live tool calls or token deltas to Zeus gRPC stream / Web UI
            },
        })
        if err != nil {
            return AskCodeResult{Error: err.Error()}, nil
        }
        return AskCodeResult{
            SessionID: result.SessionID,
            Response:  result.Response,
        }, nil
    })
}
```

---

## 5. Implementation Roadmap

1. **`pkg/engine/run.go`:**
   - Implement `Run(ctx, opts)` and `(e *Engine) Run(ctx, opts)`.
   - Setup configuration loading, provider model instantiation, system prompt assembly, tool env construction, and session persistence integration.

2. **`pkg/engine/store.go`:**
   - Implement headless session store to save and load `agentSessionFile` transcripts under `~/.config/ask/agent-sessions/<provider>/`.

3. **`ask.go` (Root Package):**
   - Expose root-level `ask.Run`, `ask.RunOptions`, and `ask.RunResult`.

4. **Behavioral Tests (`pkg/engine/run_test.go`):**
   - Single-turn execution with mock language models.
   - Multi-turn resumption via `SessionID`.
   - Streaming event delivery (`TextDeltaEvent`, `ToolCallEvent`, `DoneEvent`).
