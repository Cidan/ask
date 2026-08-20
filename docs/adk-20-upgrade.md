# Comprehensive Architectural Plan: ADK 2.0 Native Modernization

## 1. Executive Summary

`ask` is a modern terminal coding agent built in Go that utilizes Google's **Agent Development Kit 2.0 (`google.golang.org/adk/v2`)** and the **Google GenAI SDK (`google.golang.org/genai`)** against Vertex AI Gemini.

While previous modernization efforts migrated individual components (such as `functiontool.New`, `skilltoolset`, `mcptoolset`, `memory.Service`, and `FileSessionService`), several subsystems in `ask` still hand-build functionality where native, idiomatic ADK 2.0 primitives exist.

This document outlines the detailed architectural blueprint and implementation plan for adopting all remaining ADK 2.0 features (excluding Google Search grounding, which is intentionally replaced by Ask's first-class developer tooling).

Each section is designed as an independent, reviewable pull request (PR) milestone.

---

## 2. Target Architecture Overview

```
┌────────────────────────────────────────────────────────────────────────┐
│ PR 1: Runner Session Lifecycle (AutoCreateSession)                    │
│ • Eliminate manual session check/create boilerplate                   │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ PR 2: Self-Healing Tool Recovery (plugin/retryandreflect)             │
│ • Attach retry & reflection plugin for automatic error correction     │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ PR 3: Dynamic Tool Augmentation (plugin/functioncallmodifier)         │
│ • Migrate synthetic parameter injection to ADK modifier plugin        │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ PR 4: Native Subagent Delegation (tool/agenttool)                     │
│ • Wrap research and named subagents directly via agenttool.New        │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ PR 5: Standard Human-In-The-Loop Confirmation (tool/toolconfirmation) │
│ • Adopt adk_request_confirmation and ConfirmationProvider             │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ PR 6: Dynamic Instruction Interpolation (util/instructionutil)        │
│ • Dynamic session state & artifact interpolation in prompts           │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ PR 7: Session Artifact Management (artifact.Service & loadartifacts) │
│ • Native artifact storage and retrieval across turns and subagents    │
└──────────────────────────────────┬─────────────────────────────────────┘
                                   │
┌──────────────────────────────────▼─────────────────────────────────────┐
│ PR 8: Graph-Based Workflow Engine (google.golang.org/adk/v2/workflow) │
│ • Compile workflow definitions to ADK DAG Workflow Graphs            │
└────────────────────────────────────────────────────────────────────────┘
```

---

## 3. Detailed PR Milestones

---

### PR 1: Runner Session Lifecycle & Auto-Creation (`AutoCreateSession: true`) (COMPLETED & MERGED - PR #128)

#### Problem Statement
Currently, `pkg/engine/run.go` (lines 240-256) and `cmd/ask/agent_run.go` (lines 375-390) manually query `sessSvc.Get(...)` and, upon a not-found error, issue a manual `sessSvc.Create(...)` before instantiating `runner.New(...)`. This creates boilerplate and redundant session lookups on every single user turn.

#### Target Implementation
ADK 2.0 provides `runner.Config{AutoCreateSession: true}`. When set, `runner.Run` automatically initializes the session in the provided `session.Service` if it does not already exist.

#### File Changes
- **`pkg/engine/run.go`**:
  - Remove manual `sessSvc.Get` / `sessSvc.Create` block.
  - Configure `runner.New(runner.Config{AppName: "ask", Agent: agentInstance, SessionService: sessSvc, MemoryService: memSvc, AutoCreateSession: true})`.
- **`cmd/ask/agent_run.go`**:
  - Remove manual `s.sessSvc.Get` / `s.sessSvc.Create` block in `runTurn`.
  - Pass `AutoCreateSession: true` to `engine.RunnerBuilder`.
- **`pkg/engine/session.go`**:
  - Ensure headless sessions leverage `AutoCreateSession: true`.

#### Verification & Tests
- `pkg/engine/run_test.go`: Add test verifying that calling `engine.Run` with a non-existent `SessionID` successfully auto-creates the session and persists events in `FileSessionService`.
- `cmd/ask/agent_run_test.go`: Verify multi-turn conversational resumption and fresh session initialization.

---

### PR 2: Self-Healing Tool Recovery via `plugin/retryandreflect`

#### Problem Statement
When a tool call fails (e.g., regex error in grep, invalid path in read, or syntax error in edit), the current behavior either returns a raw error string in the tool result or relies on the next turn prompt to instruct the model to recover. LLMs often surrender, apologize, or stop early rather than correcting their arguments.

#### Target Implementation
ADK 2.0 provides `google.golang.org/adk/v2/plugin/retryandreflect`. When a tool execution returns an error, the plugin intercepts the failure, executes a structured reflection loop (`reflection.md`), and prompts the model to correct its arguments in the same turn without crashing or terminating the turn prematurely.

#### File Changes
- **`pkg/engine/run.go`**:
  - Import `google.golang.org/adk/v2/plugin/retryandreflect`.
  - Configure `PluginConfig` on `runner.Config`:
    ```go
    retryPlugin, err := retryandreflect.NewPlugin(retryandreflect.Config{
        MaxRetries: 2,
    })
    ```
- **`cmd/ask/agent_run.go`**:
  - Attach the `retryandreflect` plugin to the TUI runner configuration.
- **`pkg/engine/types.go`**:
  - Align `ToolResponse.IsError` with ADK's reflection handler so tool error events trigger reflection seamlessly.

#### Verification & Tests
- `pkg/engine/run_test.go`: Add `TestEngineRun_ToolRetryAndReflect` where a tool fails on the first invocation and succeeds on the second after reflection.
- `cmd/ask/agent_run_test.go`: Verify UI event stream correctly surfaces reflection attempts to the user without duplicating transcript entries.

---

### PR 3: Tool Parameter Augmentation via `plugin/functioncallmodifier`

#### Problem Statement
Ask currently mutates tool declarations and wraps handler functions in `pkg/tools/types.go` and `pkg/tools/bridge.go` to inject required metadata fields (such as the mandatory `description` phrase for UI headlines). Hand-rolling AST schema modifications risks normalization incompatibilities with GenAI schema converters.

#### Target Implementation
ADK 2.0 provides `google.golang.org/adk/v2/plugin/functioncallmodifier`. This plugin intercepts model requests before they hit the wire (`BeforeModelCallback`) and after model generation (`AfterModelCallback`), dynamically injecting synthetic argument schemas (`description`) and stripping them before tool execution.

#### File Changes
- **`pkg/tools/types.go` & `pkg/tools/bridge.go`**:
  - Remove manual JSON schema AST manipulation for `description` field injection.
  - Keep `functiontool.New[TArgs, TResults]` purely focused on the tool's typed parameters.
- **`pkg/engine/run.go` & `cmd/ask/agent_run.go`**:
  - Register `functioncallmodifier.NewPlugin(cfg)` with:
    ```go
    functioncallmodifier.NewPlugin(functioncallmodifier.FunctionCallModifierConfig{
        Predicate: func(toolName string) bool { return isNativeAskTool(toolName) },
        Args: map[string]*genai.Schema{
            "description": {
                Type:        "STRING",
                Description: "one short human-readable phrase (under 10 words) telling the user what this call is doing",
            },
        },
    })
    ```

#### Verification & Tests
- `pkg/tools/types_test.go` & `pkg/tools/bridge_test.go`: Verify tool declarations generate clean, strict schemas without conflicting `anyOf` or type arrays.
- `cmd/ask/tool_output_test.go`: Ensure headline phrase extraction remains 100% backward compatible.

---

### PR 4: Native Subagent Delegation via `tool/agenttool`

#### Problem Statement
In `cmd/ask/agent_tools_task.go` and `pkg/engine/subagents.go`, synchronous subagents (such as the default researcher or named subagents) are launched through custom execution runners that manually construct sub-sessions, extract results, and format outputs.

#### Target Implementation
ADK 2.0 provides `google.golang.org/adk/v2/tool/agenttool`. `agenttool.New(agent, &agenttool.Config{SkipSummarization: ...})` wraps any `agent.Agent` directly into an ADK `tool.Tool`, handling sub-session isolation, parameter validation against the agent's input schema, and response extraction.

#### File Changes
- **`pkg/engine/subagents.go`**:
  - Convert `BuildResearchSubagent` and `BuildNamedSubagent` into native ADK tools via `agenttool.New(agentInstance, nil)`.
- **`cmd/ask/agent_tools_task.go`**:
  - For synchronous task delegation, delegate directly through `agenttool`.
  - Retain background job manager integration (`run_in_background: true`) for asynchronous jobs while using `agenttool` under the hood.

#### Verification & Tests
- `pkg/engine/subagents_test.go`: Add tests verifying synchronous subagents execute with isolated sessions and report results through `agenttool`.
- `cmd/ask/agent_tools_test.go`: Verify `task` tool handles both synchronous and background tasks without regressions.

---

### PR 5: Standard Human-In-The-Loop Confirmation via `tool/toolconfirmation`

#### Problem Statement
Ask currently handles tool approval and permission rules through custom wrapper functions in `pkg/tools/env.go`, `pkg/engine/interaction.go`, and `cmd/ask/approval.go`. This decouples tool approval from the runner's native event stream.

#### Target Implementation
ADK 2.0 provides native Human-In-The-Loop confirmation:
- Tools and Toolsets declare confirmation requirements via `tool.ConfirmationProvider` or `RequireConfirmation: true`.
- When a tool requires approval, ADK emits an `adk_request_confirmation` event (`toolconfirmation.FunctionCallName`).
- The frontend extracts the inner intent using `toolconfirmation.OriginalCallFrom(fc)` and yields the prompt to the user.
- The user's response is returned as a standard function response (`{"confirmed": bool}`), and ADK resumes execution automatically.

#### File Changes
- **`pkg/engine/interaction.go`**:
  - Update `ApprovalRequest` to integrate with `toolconfirmation.ToolConfirmation`.
- **`pkg/tools/env.go` & `pkg/tools/types.go`**:
  - Use `tool.ConfirmationProvider` on tools that perform mutating operations (e.g. `write`, `edit`, `bash`).
- **`cmd/ask/event_adapter.go` & `cmd/ask/agent_run.go`**:
  - Handle `toolconfirmation.FunctionCallName` in the runner event loop, pop the approval modal, and feed the confirmation response back to the runner.

#### Verification & Tests
- `pkg/engine/run_test.go`: Add `TestEngineRun_ToolConfirmation_Approve` and `TestEngineRun_ToolConfirmation_Deny`.
- `cmd/ask/approval_test.go`: Verify that approving/denying tools correctly unblocks or terminates the agent step.

---

### PR 6: Dynamic Instruction Interpolation via `util/instructionutil`

#### Problem Statement
System prompts and step instructions in `pkg/engine/prompt.go` and `pkg/workflow/plans.go` currently use manual Go string interpolation and ad-hoc concatenation to inject environment variables, reminders, and notes directories.

#### Target Implementation
ADK 2.0 provides `google.golang.org/adk/v2/util/instructionutil.InjectSessionState(ctx, template)`. This resolves `{key_name}` placeholders dynamically from `session.State` and `{artifact.key_name}` from session artifacts at runtime.

#### File Changes
- **`pkg/engine/prompt.go`**:
  - Integrate `instructionutil.InjectSessionState` into `BuildInstructionProvider`.
  - Store runtime reminders, active git branch, and worktree info in `session.State` and reference them via standard template variables (`{git_branch}`, `{worktree_status}`).
- **`pkg/workflow/plans.go`**:
  - Standardize workflow step instruction templates using `{notes_dir}` and `{prev_notes_dir}` placeholders.

#### Verification & Tests
- `pkg/engine/prompt_test.go`: Add unit tests for `InjectSessionState` verifying variable substitution and missing variable error handling.
- `pkg/engine/run_test.go`: Test that state mutations during a multi-turn run dynamically update the agent instructions.

---

### PR 7: Session Artifact Management via `artifact.Service` & `loadartifactstool`

#### Problem Statement
Workflows and coding agents currently write plans, diffs, and intermediate notes directly to disk under `.ask/plans/<notesDir>`. Downstream steps must know exact filesystem paths to read previous outputs, and artifacts are not tracked in session history.

#### Target Implementation
ADK 2.0 provides `google.golang.org/adk/v2/artifact.Service` (`artifact.InMemoryService`, `gcsartifact`) and `google.golang.org/adk/v2/tool/loadartifactstool`.
- The runner is configured with `runner.Config{ArtifactService: artifact.InMemoryService()}`.
- Agents and workflow steps save named artifacts (e.g. `plan.md`, `implementation_diff.patch`, `review_notes.md`) to `ctx.Artifacts().Save(...)`.
- Downstream steps use `loadartifactstool.New()` to autonomously list and load artifacts into their context.

#### File Changes
- **`pkg/engine/run.go` & `cmd/ask/agent_run.go`**:
  - Configure `ArtifactService: artifact.InMemoryService()` on `runner.Config`.
  - Attach `loadartifactstool.New()` to the default toolset.
- **`pkg/workflow/runner.go`**:
  - Update workflow steps to save step summaries and notes into `ctx.Artifacts()`.

#### Verification & Tests
- `pkg/engine/run_test.go`: Test artifact creation by one turn and retrieval via `load_artifacts` by the next.
- `pkg/workflow/runner_test.go`: Test inter-step artifact passing in a multi-step workflow.

---

### PR 8: Graph-Based Workflow Engine via `google.golang.org/adk/v2/workflow`

#### Problem Statement
Ask's `pkg/workflow/runner.go` maintains a custom handwritten step execution loop for running workflows, tracking loop iterations, and handling step transitions. This duplicates the execution graph logic provided by ADK 2.0.

#### Target Implementation
ADK 2.0 includes a comprehensive DAG workflow engine in `google.golang.org/adk/v2/workflow`:
- **Nodes**: `AgentNode` (wraps LLM agent), `ToolNode` (executes tools directly), `FunctionNode` (executes Go logic), `JoinNode` (synchronizes concurrent branches).
- **Routing**: `StringRoute`, `IntRoute`, `BoolRoute`, `MultiRoute`, `Default`.
- **State Persistence**: `workflow.Persistence` automatically tracks node execution and state in `session.State`, enabling seamless workflow pause and resumption (`workflow.Resume`).

#### File Changes
- **`pkg/workflow/graph.go`**:
  - Implement `CompileDefToADKWorkflow(def Def, cfg WorkflowAgentConfig) (*workflow.Workflow, error)`.
  - Map linear steps to sequential `AgentNode` edges.
  - Map loop steps to cyclic edges controlled by `exitlooptool` and route conditions.
- **`pkg/workflow/runner.go`**:
  - Replace the custom `Runner.Run` state machine with `Workflow.Run(ctx)`.
  - Map workflow graph events to `RunnerListener` callbacks (`OnWorkflowStarted`, `OnWorkflowStepStarted`, `OnWorkflowStepDone`, `OnWorkflowDone`).

#### Verification & Tests
- `pkg/workflow/runner_test.go`: Migrate existing workflow test suite to execute against the ADK workflow graph engine.
- Verify 100% behavioral parity with existing `.ask/workflows/*.json` pipelines.

---

## 4. Execution Checklist

- [x] **PR 1**: Runner Session Lifecycle & Auto-Creation (`AutoCreateSession: true`) ([#128](https://github.com/Cidan/ask/pull/128))
- [ ] **PR 2**: Self-Healing Tool Recovery via `plugin/retryandreflect`
- [ ] **PR 3**: Tool Parameter Augmentation via `plugin/functioncallmodifier`
- [ ] **PR 4**: Native Subagent Delegation via `tool/agenttool`
- [ ] **PR 5**: Standard Human-In-The-Loop Confirmation via `tool/toolconfirmation`
- [ ] **PR 6**: Dynamic Instruction Interpolation via `util/instructionutil`
- [ ] **PR 7**: Session Artifact Management via `artifact.Service` & `loadartifactstool`
- [ ] **PR 8**: Graph-Based Workflow Engine via `google.golang.org/adk/v2/workflow`
