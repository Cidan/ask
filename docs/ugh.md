# Architectural Analysis: ADK 2.0 Usage in `ask` vs `zeus`

This document details why `ask` experiences premature turn terminations, lazy LLM behaviors, incomplete workflows, and context confusion after migrating to Google ADK 2.0 (`google.golang.org/adk/v2`), contrasting it against the reference implementation in `zeus` (`//apps/zeus`).

---

## Executive Summary

1. **The Phantom ADK Migration in `cmd/ask`**: Despite ADK 2.0 being present in `go.mod`, `ask`'s primary execution engine in `cmd/ask/agent_run.go` does **not** use the ADK agent loop. It runs a 500-line hand-rolled loop over raw `genai.Client.Models.GenerateContentStream`. `pkg/engine/run.go` contains an incomplete ADK runner wrapper, but `cmd/ask` does not use it for interactive or workflow sessions.
2. **The `end_turn` Premature Exit Trap**: Workflows (`pkg/workflow/runner.go`) inject prompts saying *"You MUST call the end_turn tool as your final action..."*. As soon as the LLM does one minor step (like reading a file), it calls `end_turn`. The tool responds with `"end_turn recorded..."`, the LLM outputs a 1-sentence summary, and stops emitting tool calls. `coordinator.go` sees a registered summary and marks the step complete, leaving the task unfinished.
3. **Double-Declared Tools & System Prompt Bloat**: `pkg/engine/prompt.go` embeds hundreds of lines of hardcoded tool manuals with JSON schemas directly in the system prompt while simultaneously passing `FunctionDeclarations` on the wire. This creates token waste, conflicting schema definitions, and model confusion.
4. **Tool Guard Rejections Causing Model Surrender**: The `GateTodosBeforeMutate` and two-stage workflow guards actively intercept and reject `write`, `edit`, and `todos` with large error notices. LLMs interpret tool rejections as authorization barriers, immediately giving up, apologizing, and ending their turn.
5. **Lack of Native ADK Session & State Management**: `zeus` leverages ADK's `session.Service`, `plugin.Plugin`, `StateDelta`, and `InstructionProvider` to manage session history, context truncation, and reprimands. `ask` manually converts custom message structs (`engine.Message` ↔ `genai.Content`) and creates throwaway in-memory services.

---

## Core Differences: `zeus` vs `ask`

| Architectural Component | `zeus` (Idiomatic ADK 2.0) | `ask` (Current Implementation) |
|---|---|---|
| **Agent Execution Loop** | Standard ADK `runner.Runner` + `llmagent.Agent`. Multi-step tool calls, event persistence, and stream handling are fully managed by the ADK runner. | Hand-rolled `stepIdx < maxSteps` loop in `cmd/ask/agent_run.go` calling raw `genai.GenerateContentStream`. |
| **Tool Implementation** | Standard ADK `functiontool.New[Args, Result]` using Go struct tags (`jsonschema:"..."`). Handlers receive `agent.Context` with access to `ctx.State()` and `ctx.Actions().StateDelta`. | Custom `TypedTool[T]`, custom JSON-schema reflection, custom `FlattenNullableTypes`, and manual wire declarations in `pkg/tools/types.go`. |
| **Turn Continuity & Verification** | **ADK State Delta + Dynamic Instruction Provider**: Let the agent run. If verification is missing, set `StateDelta: {"verification_reprimand": true}` and re-invoke `runner.Run(ctx, user, session, nil)` without polluting the prompt. | Injected prompt instructions telling the model to call `end_turn`. Calling `end_turn` returns a success message immediately, causing the model to wrap up and exit. |
| **System Prompts** | `llmagent.Config.InstructionProvider` (`func(ctx agent.ReadonlyContext) (string, error)`). Evaluates prompt context per-invocation from session state. | Giant static string in `prompt.go` containing duplicate tool schemas, rigid workflow guard rules, and error-reprompt concatenations. |
| **Memory & Truncation** | Official ADK `memory.Service` + standard `loadmemorytool` / `preloadmemorytool` + an ADK `plugin.Plugin` (`memory_saver`) for automated context truncation. | Custom SQLite/vector logic, tool wrappers (`wrapFileToolsWithMemory`) that inject recall text into file read results, and prompt string prefixing. |
| **Subagents** | `RunnerFactory.Build(ephemeral=true, instructions)` builds a lightweight ADK runner with `InMemoryService`, executed in parallel via `errgroup`. | Custom subagent runners with isolated processes/goroutines and ad-hoc tool sharing. |

---

## Detailed Failure Mode Analysis

### 1. Why Turns End Short in Workflows

In `pkg/workflow/runner.go`:
```go
// EndTurnInstructionBlock renders the auto-injected end_turn contract for a step.
b.WriteString("When you have finished this step, you MUST call the end_turn tool as your final action, with " +
    "a `summary` of 1-3 sentences describing what you did and the outcome. This records your progress in the " +
    "workflow log; it does not cut your turn short.")
```

When an LLM executes a workflow step:
1. The model sees the instruction that calling `end_turn` is required.
2. After performing an initial read or trivial action, it calls `end_turn(summary="I analyzed the code and prepared the plan")`.
3. `agentEndTurnTool` sets `env.PendingEndTurn` and returns `"end_turn recorded... Finish your turn normally"`.
4. The LLM receives `"end_turn recorded"` as the tool response, writes `"I've recorded the end_turn summary."`, and emits no further tool calls.
5. `agent_run.go` detects `len(turnToolCalls) == 0` and terminates the turn loop.
6. `coordinator.go` checks `session.env.PendingEndTurn`, sees the summary, and informs `pkg/workflow/runner.go` that the step succeeded.
7. The runner advances to the next step or marks the workflow done, even though no code was modified or verified.

### 2. Re-Prompting via User Message Concatenation

When `pkg/workflow/runner.go` detects a missing `end_turn`, it re-prompts the model by mutating the user prompt:
```text
Previous step output:
<entire previous model generation dump>

REMINDER: your previous turn ended without calling end_turn. You have already done the work shown above — do NOT repeat it. Call the end_turn tool now.
```

**Why this fails:**
- Dumping raw assistant outputs back into the `user` role confuses the model's turn context and causes token waste.
- The prompt explicitly says *"do NOT repeat it. Call the end_turn tool now."* The model complies literally: it calls `end_turn` without doing any remaining work.

**How Zeus solves this:**
- In `zeus` (`internal/handler/handler.go`), re-prompts do **not** touch user messages.
- Zeus sets `StateDelta: {"verification_reprimand": true}` in the session and restarts `runner.Run(ctx, user, session, nil)` (`userMsg = nil`).
- The `InstructionProvider` dynamically appends the reprimand to the **system instruction**:
  ```go
  if v, err := ctx.ReadonlyState().Get("verification_reprimand"); err == nil && v == true {
      instr += "\n\nCRITICAL FAILURE RISK: You were blocked from ending your turn because you did not verify your statements..."
  }
  ```

### 3. Defensive Tool Interceptions & Model Surrender

`ask` implements strict pre-condition checks in `agent_tools_todos.go` and `agent_tools_file.go`:
- `GateTodosBeforeMutate`: Blocks `write` and `edit` until a `todos` call has been made.
- Two-stage workflow guard: Blocks `todos` until `workflow_list` is called, and blocks inline execution until `finalized_plan` is confirmed.

When an LLM tries to write code, it receives a failure result with instructions telling it to call `workflow_list` or `todos`. Modern models frequently treat tool execution failures as a signal that file modification is forbidden, resulting in an immediate apology, an `end_turn` call, and an early exit.

### 4. Dual Engine Architecture Drift

There are two separate execution paths in `ask`:
1. `cmd/ask/agent_run.go`: Used by the TUI and `Coordinator`. Completely custom loop around `genai.Client.Models.GenerateContentStream`.
2. `pkg/engine/run.go`: Partial ADK 2.0 `runner.Runner` wrapper with `llmagent.New`. Only called by the `task` subagent tool and standalone library tests.

This split means improvements to `pkg/engine` do not affect the main interactive application, and bugs fixed in `agent_run.go` do not benefit the engine library.

---

## Roadmap to an Idiomatic ADK 2.0 Architecture

### Step 1: Unify the Agent Loop onto ADK `runner.Runner`
- Eliminate `cmd/ask/agent_run.go`'s hand-rolled loop.
- Have both interactive TUI tabs and workflow step executions route through `pkg/engine`'s ADK runner.
- Stream events from `runner.Run(ctx, userID, sessionID, userMsg)` directly to Bubble Tea (`tea.Msg`), matching `zeus`'s `Handler.HandleStream`.

### Step 2: Migrate Tools to `functiontool.New`
- Replace `TypedTool[T]` in `pkg/tools/types.go` with standard `functiontool.New[Args, Result]`.
- Define tool parameter structs with Go field tags (`json:"..." jsonschema:"..."`).
- Allow tools to receive `agent.Context` to interact with session state (`ctx.State().Set(...)`).

### Step 3: Implement Dynamic `InstructionProvider`
- Remove all hardcoded tool manuals, JSON schema definitions, and workflow guard explanations from `pkg/engine/prompt.go`.
- Use `llmagent.Config.InstructionProvider` to dynamically supply system instructions based on the active session state, current working directory, and workflow execution mode.

### Step 4: Adopt Zeus's State-Driven Turn Verification Pattern
- Remove the prompt constraint forcing models to call `end_turn` as their final action.
- Let the ADK runner run until the model naturally finishes its turn segment.
- If a workflow step requires specific verification or summary artifacts that were not produced, record a state delta (`step_incomplete: true`) and re-run the runner with `userMsg = nil` so the `InstructionProvider` injects the correction dynamically.

### Step 5: Native ADK Memory & Context Truncation
- Implement ADK's `memory.Service` interface backed by sqlite-vec or embedded storage.
- Register standard `loadmemorytool.New()` and `preloadmemorytool.New()`.
- Add an ADK `plugin.Plugin` with `AfterRunCallback` to monitor context window size and prune older session events cleanly in the session store instead of performing ad-hoc string truncation.
