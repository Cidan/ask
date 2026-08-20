# Architectural Blueprint: Modernizing `ask` with Native Google ADK 2.0

## 1. Executive Summary

`ask` is a terminal coding agent built in Go that utilizes Google's **Agent Development Kit 2.0 (`google.golang.org/adk/v2`)** and the **Google GenAI SDK (`google.golang.org/genai`)** against Vertex AI Gemini. 

While the core execution loop has been wired to ADK (`llmagent.New`, `gemini.NewModel`, `runner.New`), the codebase currently retains extensive legacy machinery and bespoke implementations for subsystems where ADK 2.0 provides native, battle-tested primitives. 

This document provides a comprehensive architectural audit of `ask`'s ADK integration, identifies areas of divergence and technical debt, contrasts current implementations against idiomatic ADK 2.0 patterns, and establishes a phased modernization roadmap.

---

## 2. Architectural Audit & Divergence Analysis

### 2.1. Multi-Step Workflows, Loops, and Orchestration

#### Current Implementation in `ask`
* **Custom Step Runner (`pkg/workflow/runner.go:134-389`)**: `ask` implements a synchronous state machine (`workflow.Runner`) that manages linear step progression, loop frames (`LoopRunFrame`), iteration limits, notes directory management, and retry handling.
* **Prompt-Injected Turn Termination (`pkg/workflow/runner.go:461-486`)**: Every workflow step injects a mandatory contract:
  ```text
  When you have finished this step, you MUST call the end_turn tool as your final action, with a `summary` of 1-3 sentences describing what you did and the outcome.
  ```
  For loop steps, it forces the model to decide whether to continue or break by supplying `decision: "continue"` or `decision: "break"`.
* **User Message History Concatenation (`pkg/workflow/runner.go:413-458`)**: When a step ends without `end_turn`, the runner re-prompts the model by concatenating the entire previous model output into the `user` prompt along with a reminder (`REMINDER: your previous turn ended without calling end_turn...`).

#### Limitations & Failure Modes
1. **Premature Turn Exits**: Modern reasoning models often invoke `end_turn` immediately after a superficial first step (such as reading a file), believing that calling `end_turn` is required to log progress. `coordinator.go` sees a recorded summary and marks the step complete, terminating execution before real work is done.
2. **Context Pollution**: Dumping raw assistant outputs back into the `user` role confuses multi-turn conversation structure, inflates prompt token overhead, and degrades model performance.
3. **Rigid Control Flow**: Hand-rolled loops cannot leverage parallel execution, conditional DAG branch routing, or native agent escalation.

#### Idiomatic ADK 2.0 Architecture
ADK 2.0 provides a first-class workflow hierarchy in `google.golang.org/adk/v2/agent/workflowagents`:
* **`sequentialagent.New(cfg)`**: Chains a sequence of `agent.Agent` steps. State is passed cleanly through the session without manual message concatenation.
* **`loopagent.New(cfg)`**: Repeatedly executes inner subagents up to `MaxIterations` or until terminated.
* **`exitlooptool.New()` (`google.golang.org/adk/v2/tool/exitlooptool`)**: Provides an official tool that lets any subagent in a loop exit cleanly by setting an escalation signal, eliminating string-based `decision="break"` prompt contracts.
* **`parallelagent.New(cfg)`**: Executes multiple subagents concurrently and merges their outputs.

---

### 2.2. Agent Skills Standard (`agentskills.io`)

#### Current Implementation in `ask`
* **Hand-Rolled Parser & Discovery (`pkg/engine/skills.go:66-239`)**:
  * Scans search directories (`~/.config/ask/skills`, `.agents/skills`, `.claude/skills`).
  * Implements custom line-by-line YAML frontmatter parsing (`ParseMarkdownFrontmatter`).
  * Enforces name validation via custom regex (`skillNameRe`).
  * Injects XML `<available_skills>` blocks into the static system prompt (`SkillsPromptBlock`).
  * Intercepts `/skill-name` slash commands and formats `<loaded_skill>` prompt bodies (`ExpandSkillInvocation`).

#### Idiomatic ADK 2.0 Architecture
ADK 2.0 provides native, built-in support for the Agent Skills standard via `google.golang.org/adk/v2/tool/skilltoolset`:
* **`skill.Source` (`google.golang.org/adk/v2/tool/skilltoolset/skill`)**:
  * `skill.NewFileSystemSource(fs)` and `skill.NewMergedSource(...)` handle hierarchical multi-directory discovery.
  * `skill.ParseBytes` and `skill.Validate` provide standardized frontmatter parsing and validation.
* **`skilltoolset.New(ctx, cfg)`**:
  * Registers skills directly as an ADK `tool.Toolset` on the agent.
  * Exposes skills dynamically to the model with standard progressive disclosure, removing the need for manual XML prompt rendering.

---

### 2.3. Model Context Protocol (MCP) Client

#### Current Implementation in `ask`
* **Hand-Rolled Connection Manager (`pkg/tools/mcp.go:87-293`)**:
  * `MCPManager` and `mcpServerConn` manage client lifecycles over stdio, SSE, and streamable HTTP transports directly via `github.com/modelcontextprotocol/go-sdk/mcp`.
  * Manually listens to `ToolListChangedHandler` and triggers tool re-discovery.
  * Custom `mcpAgentTool` adapts MCP tools into `ask`'s internal `engine.Tool` interface.
  * Custom elicitation questions are mapped manually into Bubble Tea modals.

#### Idiomatic ADK 2.0 Architecture
ADK 2.0 includes a dedicated MCP integration in `google.golang.org/adk/v2/tool/mcptoolset`:
* **`mcptoolset.New(cfg)`**:
  * Accepts any `mcp.Transport` (stdio, SSE, streamable).
  * Implements `tool.Toolset` natively, making MCP tools directly compatible with any ADK `llmagent`.
  * Supports dynamic tool filtering via `ToolFilter: tool.Predicate`.
  * Integrates with ADK's Human-In-The-Loop confirmation engine via `RequireConfirmationProvider: tool.ConfirmationProvider`.

---

### 2.4. Memory Management & Long-Term Recall

#### Current Implementation in `ask`
* **Custom SQLite Vector Store (`pkg/memory/memory.go:41-294`)**:
  * Implements a standalone SQLite database with `sqlite-vec` CGO bindings and a local C++ embedding model (`embeddinggemma-300M-Q8_0.gguf`) driven by `llama.cpp` static builds.
  * Does **not** implement ADK's `memory.Service` interface.
* **Side-Channel Prompt & Tool Interception (`pkg/memory/recall.go`, `cmd/ask/agent_run.go:311`)**:
  * Recall entries are injected into user prompts by appending text (`turn.text = turn.text + "\n\n" + mem`).
  * File read/edit/write tools are wrapped to append recall footers to tool outputs.

#### Idiomatic ADK 2.0 Architecture
ADK 2.0 defines a standard contract for memory ingestion and retrieval in `google.golang.org/adk/v2/memory`:
```go
type Service interface {
    AddSessionToMemory(ctx context.Context, s session.Session) error
    SearchMemory(ctx context.Context, req *SearchRequest) (*SearchResponse, error)
}
```
* **ADK Memory Tools**:
  * `google.golang.org/adk/v2/tool/loadmemorytool`: Exposes a standard `load_memory` tool so the model can autonomously search long-term memory when relevant.
  * `google.golang.org/adk/v2/tool/preloadmemorytool`: Automatically retrieves and injects memory entries into context before turn execution.
* **Runner Integration**:
  * `runner.Config{MemoryService: ...}` accepts any `memory.Service` implementation (whether backed by SQLite-vec, Vertex AI Memory Bank, or in-memory storage) and manages ingestion automatically.

---

### 2.5. Tool Typing, Schema Reflection, and Registry

#### Current Implementation in `ask`
* **Three-Layer Custom Tool System**:
  1. `engine.Tool` interface (`pkg/engine/types.go:39-45`): Custom interface requiring `Name()`, `Description()`, `Info() ToolInfo`, `Declaration() *genai.FunctionDeclaration`, and `Run(ctx, map[string]any) (ToolResponse, error)`.
  2. `TypedTool[T]` (`pkg/tools/types.go:27-88`): Reflects struct schemas via `jsonschema.For[T]`, marshals/unmarshals into generic `map[string]any`, and applies `FlattenNullableTypes` (`pkg/tools/bridge.go:94-122`) to drop `"null"` from `type: ["null", "string"]`.
  3. `AsADKTool` / `AsADKTools` (`pkg/engine/types.go:48-99`): Adapts `engine.Tool` back to ADK `tool.Tool` using `functiontool.New[map[string]any, any]`.
* **Deferred Tool Registry (`pkg/tools/registry.go`)**:
  * Hides non-core tools behind meta-tools (`search_tools` and `invoke_tool`), requiring multi-hop round trips for tool discovery.

#### Limitations
* Redundant serialization and deserialization across struct types, JSON strings, and `map[string]any`.
* Custom schema manipulation logic is prone to subtle schema normalization bugs across different Gemini versions.

#### Idiomatic ADK 2.0 Architecture
ADK 2.0's `functiontool.New[TArgs, TResults]` natively handles struct reflection, schema generation, parameter validation, and execution:
```go
type EditArgs struct {
    FilePath string `json:"file_path" jsonschema:"description=Path to the file to edit"`
    Content  string `json:"content" jsonschema:"description=New content for the file"`
}

type EditResult struct {
    Diff    string `json:"diff,omitempty"`
    Success bool   `json:"success"`
}

tool, err := functiontool.New(functiontool.Config{
    Name:        "edit",
    Description: "Replaces file contents with exact matching.",
}, func(ctx agent.Context, args EditArgs) (EditResult, error) {
    // Direct access to ctx.State(), ctx.Actions().StateDelta, etc.
    return EditResult{Success: true}, nil
})
```

---

### 2.6. Provider Authentication & Seams

#### Current Implementation in `ask`
* **Process-Global Environment Mutation (`pkg/providers/vertex.go:60-82`)**:
  ```go
  var VertexApplyEnv = func(path string) {
      _ = os.Setenv(VertexEnvApplicationCredentials, path)
  }
  ```
  When a custom service account key is specified, `ask` calls `os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)`. This mutates global process state and can cause race conditions or leak credentials into concurrent child processes.

#### Idiomatic ADK 2.0 Architecture
`genai.ClientConfig` supports clean credential injection without touching environment variables:
```go
cfg := &genai.ClientConfig{
    Backend:     genai.BackendVertexAI,
    Project:     project,
    Location:    location,
    Credentials: customAuthCredentials, // Pass *auth.Credentials directly
}
```

---

### 2.7. Session Persistence & Event Sourcing

#### Current Implementation in `ask`
* **Dual Persistence Layer**:
  * `pkg/engine/store.go:74` implements a fully compliant `FileSessionService` (`session.Service`).
  * However, `cmd/ask/agent_run.go:409-565` maintains a duplicate in-memory slice `s.messages []engine.Message`.
  * When `r.Run(...)` executes, ADK automatically calls `s.sessSvc.AppendEvent(...)`. Yet, at the end of every turn, `agentSession.runTurn` calls `s.persist()` (`cmd/ask/agent_run.go:564`), which unmarshals and re-saves the entire session file a second time.

#### Idiomatic ADK 2.0 Architecture
* Make `session.Service` the single source of truth.
* Stream events directly from `runner.Run(...)` to the Bubble Tea UI via `LoadHistoryEntriesFromEvents`, eliminating redundant message slices and double-disk writes.

---

### 2.8. Tool Guards & Turn Surrender

#### Current Implementation in `ask`
* **Defensive Tool Errors (`pkg/tools/todos.go:19-45`, `pkg/tools/file.go:16-30`)**:
  * `GateTodosBeforeMutate` blocks `write`/`edit` tools if `todos` has not been called, returning an error message.
  * The two-stage workflow guard intercepts `todos` calls with verbose rejection notices directing the model to call `workflow_list`.
* **Failure Mode**:
  * Modern LLMs interpret tool error responses as authorization barriers or system failures. Instead of correcting their behavior, they frequently apologize, call `end_turn`, and surrender prematurely.

#### Idiomatic ADK 2.0 Architecture
* **Native Human-in-the-Loop Confirmation (`google.golang.org/adk/v2/tool/toolconfirmation`)**:
  * Use ADK's native `toolconfirmation.FunctionCallName` (`adk_request_confirmation`) and `tool.ConfirmationProvider`.
  * When a mutating action requires approval, ADK pauses the runner and yields a confirmation action cleanly, avoiding error injections in the conversation history.

---

## 3. Architecture Comparison Matrix

| Component | `ask` (Current) | ADK 2.0 (Target Modern Architecture) |
|---|---|---|
| **Workflow Pipelines** | Hand-rolled synchronous runner in `pkg/workflow/runner.go` | `google.golang.org/adk/v2/agent/workflowagents/sequentialagent` |
| **Workflow Loops** | Prompt-injected `end_turn(decision="break")` | `google.golang.org/adk/v2/agent/workflowagents/loopagent` + `tool/exitlooptool` |
| **Agent Skills** | Custom regex parser & `<available_skills>` prompt builder in `pkg/engine/skills.go` | `google.golang.org/adk/v2/tool/skilltoolset` + `skill.Source` |
| **MCP Integration** | Custom client manager & adapter in `pkg/tools/mcp.go` | `google.golang.org/adk/v2/tool/mcptoolset` (`mcptoolset.New`) |
| **Memory System** | Standalone SQLite vector database + prompt concatenation | `google.golang.org/adk/v2/memory.Service` + `loadmemorytool` / `preloadmemorytool` |
| **Tool Definitions** | Custom `engine.Tool` + `TypedTool[T]` + `FlattenNullableTypes` | Native `google.golang.org/adk/v2/tool/functiontool` (`functiontool.New[TArgs, TResults]`) |
| **Turn Verification** | User-prompt dumps with `REMINDER:` strings | `agent.Context` with `StateDelta` + dynamic `InstructionProvider` |
| **Authentication** | `os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", ...)` | `genai.ClientConfig{Credentials: ...}` |
| **Session Storage** | Dual `engine.Message` arrays + redundant `s.persist()` | Single `session.Service` managed exclusively by `runner.Runner` |
| **System Prompts** | 500-line static prompt with embedded tool manuals | Dynamic `llmagent.Config.InstructionProvider` |

---

## 4. Phased Modernization Roadmap

```
┌────────────────────────────────────────────────────────────────────────┐
│ Phase 1: Tooling & Provider Cleanup                                   │
│ • Unify tools onto functiontool.New[TArgs, TResults]                   │
│ • Eliminate os.Setenv in Vertex provider via genai.ClientConfig        │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ Phase 2: Skills & MCP Native Adoption                                 │
│ • Replace custom skills parser with adk/v2/tool/skilltoolset           │
│ • Replace custom MCP manager with adk/v2/tool/mcptoolset               │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ Phase 3: Workflows to ADK WorkflowAgents                              │
│ • Replace pkg/workflow/runner.go with sequentialagent & loopagent      │
│ • Replace end_turn prompt mandates with exitlooptool                   │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ Phase 4: Memory System Standard Alignment                             │
│ • Implement adk/v2/memory.Service interface on sqlite-vec storage      │
│ • Wire loadmemorytool and preloadmemorytool into runner.Runner         │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ Phase 5: Session Single Source of Truth & Dynamic Instructions        │
│ • Remove duplicate s.messages array and redundant persist() saves      │
│ • Adopt dynamic InstructionProvider driven by session state deltas     │
└────────────────────────────────────────────────────────────────────────┘
```

### Phase 1: Tooling & Provider Cleanup (COMPLETED & VERIFIED)
1. **Migrate Tools to `functiontool.New`**:
   - Standardized all tools in `pkg/tools/` (file, bash, search, todos, web, workflow, ask, memory, registry, mcp) on native ADK `tool.Tool` and `functiontool.New[TArgs, TResults]`.
   - Replaced custom `engine.Tool` interface with an alias to `google.golang.org/adk/v2/tool.Tool`.
   - Replaced custom JSON reflection tags with standard `jsonschema:"..."` struct tags adhering to `github.com/google/jsonschema-go`.
   - Removed `TypedTool[T]`, custom schema wrappers, and intermediate unmarshaling layers.
2. **Clean Vertex AI Authentication**:
   - Modernized `pkg/providers/vertex.go` to load `*auth.Credentials` directly via `cloud.google.com/go/auth/credentials` and pass into `genai.ClientConfig{Credentials: ...}`.
   - Eliminated process-global `os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", ...)` mutation hack.
   - Verified 100% test pass rate with zero regressions across all unit and behavioral suites.

### Phase 2: Skills & MCP Native Adoption (COMPLETED & VERIFIED)
1. **Adopt `skilltoolset`**:
   - Replaced `pkg/engine/skills.go` with ADK 2.0 native `skill.Source` (`NewSkillSource`), `skilltoolset.New`, and `skill.ParseBytes` / `skill.Validate`.
   - Wired `NewSkillToolset` into `llmagent.Config.Toolsets` across `pkg/engine/run.go` and `cmd/ask/agent_run.go`.
   - Enabled dynamic skill injection via `SkillToolset.ProcessRequest`, eliminating redundant static prompt XML when toolsets are attached while preserving `/skill-name` user slash command expansion.
2. **Adopt `mcptoolset`**:
   - Replaced custom client tool wrapper in `pkg/tools/mcp.go` with native `mcptoolset.New` and `tool.Toolset`.
   - Integrated `MCPManager.Toolsets()` into agent runtimes for native ADK tool execution.
   - Preserved full transport support (stdio, SSE, streamable HTTP, and OAuth) with graceful error handling and reconnects.
3. **Comprehensive Test Verification**:
   - Added `TestDiscoverSkills_ADKSource`, `TestSkillToolset_Integration`, `TestMCPToolset_AttachAndCall`, and `TestMCPManager_ToolsetsLifecycle`.
   - Verified 100% test pass rate with zero regressions across the entire suite.

### Phase 3: Workflows to ADK WorkflowAgents
1. **Migrate Pipeline Runner**:
   - Replace the custom state machine in `pkg/workflow/runner.go` with `sequentialagent.New` and `loopagent.New`.
   - Convert workflow steps into ADK subagents.
2. **Standardize Loop Terminations**:
   - Attach `exitlooptool.New()` to loop subagents.
   - Remove `end_turn` prompt mandates and string-parsed decision parameters.
3. **State-Driven Turn Reprimands**:
   - Replace user-prompt error dumps with session state deltas (`StateDelta: {"step_incomplete": true}`) and dynamic system instruction updates.

### Phase 4: Memory System Standard Alignment
1. **Implement `memory.Service`**:
   - Refactor `pkg/memory/memory.go` so `memory.Service` implements `google.golang.org/adk/v2/memory.Service` (`AddSessionToMemory`, `SearchMemory`).
2. **Attach Native Memory Tools**:
   - Register `loadmemorytool.New()` and `preloadmemorytool.New()` in the toolset.
   - Pass the memory service to `runner.Config{MemoryService: ...}`.
   - Remove side-channel tool wrappers and prompt-string concatenations.

### Phase 5: Session Single Source of Truth & Dynamic Instructions
1. **Unify Session Persistence**:
   - Eliminate `s.messages` in `cmd/ask/agent_run.go` and remove `s.persist()`.
   - Rely solely on `FileSessionService` event appending during `runner.Run`.
2. **Dynamic `InstructionProvider`**:
   - Move dynamic context injection (rules, git status, environment, state deltas) into `llmagent.Config.InstructionProvider`.
   - Remove static tool descriptions from the system prompt to avoid token bloat and schema drift.
