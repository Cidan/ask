# ADK 2.0 adoption status

`ask` runs on Google's **Agent Development Kit 2.0** (`google.golang.org/adk/v2`)
and the GenAI SDK against Vertex AI Gemini.

This document tracks what is actually wired. An earlier revision listed
eight PRs as `[x] COMPLETED & MERGED`; an audit found that five of them
had merged as unreachable code — the ADK symbol was imported, a builder
was written, a test was written against the builder, and nothing in the
production path ever called it. The table below is kept honest on
purpose: **a row is only "done" when a production caller reaches it.**

---

## Status

| Capability | ADK surface | Status |
|---|---|---|
| Agent loop | `runner.Runner` + `llmagent` | **done** — `cmd/ask/agent_run.go`, `pkg/engine/run.go` |
| Session lifecycle | `runner.Config{AutoCreateSession}` | **done** — `pkg/engine/run.go` |
| Sessions on disk | `session.Service` (`NewFileSessionService`) | **done** |
| Tools | `functiontool.New`, `tool.Toolset` | **done** |
| Skills / MCP | `skilltoolset`, `mcptoolset` | **done** |
| Memory | `memory.Service` (`pkg/memory`) | **done** — including workflow-run ingestion |
| Workflow engine | `workflow.Workflow` graph + `AgentNode` | **done** — see below |
| Loops | `loopagent` + `exitlooptool` | **done** — compiled into the graph |
| Per-node retry | `workflow.NodeConfig.RetryConfig` | **done** — replaced the hand-rolled retry loop |
| Step context isolation | `llmagent.IncludeContentsNone` | **done** |
| Self-healing tool errors | `plugin/retryandreflect` | **registered, never fires** — see Gaps |
| Parameter injection | `plugin/functioncallmodifier` | **inert** — see Gaps |
| Subagent delegation | `tool/agenttool` | **not wired** — see Gaps |
| Human-in-the-loop | `tool/toolconfirmation` | **not wired** — see Gaps |
| Artifacts | `artifact.Service`, `loadartifactstool` | **dropped** — see Gaps |
| Dynamic instructions | `util/instructionutil` | **deliberately not used** — see Gaps |
| Parallel / fan-out | `JoinNode`, `NodeConfig.ParallelWorker` | **planned** |
| Pause / resume, HITL | `workflow.Persistence`, `Workflow.Resume`, `NewRequestInputEvent` | **planned** |

---

## Workflow engine

A `workflow.Def` compiles to an ADK graph (`pkg/workflow/compile.go`):

- Each top-level agent step becomes an `AgentNode` wrapping an
  `llmagent`, chained `Start -> n0 -> n1 -> …`.
- A `kind: "loop"` step becomes an `AgentNode` wrapping a `loopagent`
  whose sub-agents are the inner steps, each carrying `exit_loop`. A step
  calls `exit_loop` to break (it sets `Actions.Escalate`, which is what
  `loopagent` watches); otherwise the loop runs to `MaxIterations`.
- Per-node `RetryConfig` replaces the runner's `stepErrorRetry` loop.

Two `llmagent` settings carry the workflow's semantics and must not be
dropped:

- **`IncludeContents: IncludeContentsNone`.** Without it a step inherits
  the whole session, and ADK's `ConvertForeignEvent` renders every prior
  step's events as prose — each tool call and each full tool result — so
  step 3 would carry steps 1 and 2 in their entirety. With it, a step
  sees the handoff from the step before it and its own work.
- **`InstructionProvider`, never `Config.Instruction`.** Step prompts are
  user-authored and routinely contain braces. ADK interpolates the static
  `Instruction` field against session state and fails the invocation on
  the first unknown `{name}`.

`*workflow.Workflow` is not an `agent.Agent` (the interface has an
unexported method), so `engine.WorkflowGraphAgent` wraps it via
`agent.New(agent.Config{Run: wf.Run})` and hands that to the runner.

Progress reporting lives in `pkg/workflow/progress.go`. Every callback is
driven by a real event: a step starts when an event authored by its agent
arrives and finishes when its successor starts or the run ends cleanly.
Steps that never ran are never reported. The previous graph runner closed
out every remaining step as completed and hardcoded a successful finish,
so a chain that died at step 1 of 5 rendered 5/5 green.

### What the migration removed

- The handwritten `Runner.Run` state machine, `LoopRunFrame`, and the
  re-prompt machinery (`RemindNoSummary` / `RemindNoDecision` /
  `RemindFixPlanDir`).
- `RunGraph` and `BuildWorkflowAgent` — two dead parallel engines. Note
  `RunGraph` never used the graph; it called `BuildWorkflowAgent`.
- `StepExecutor` / `ExecuteStep`. A run is now one agent session for the
  whole graph, not one provider session per step.
- The `ask/plans/` notes directories (`pkg/workflow/plans.go`) and the
  `clear_plans` tool. Step handoff is the graph's node output; durable
  reasoning goes to `pkg/memory` via `IngestWorkflowMemory`.

---

## Gaps

Recorded so the next reader does not mistake an import for an
integration.

**`plugin/retryandreflect`** is in `DefaultPlugins()` but cannot fire.
ADK triggers it from `OnToolErrorCallback`, i.e. a non-nil Go `error`
from the tool. Every ask tool returns `(NewTextErrorResponse(...), nil)`,
and `tools.NewTool` turns that into a *successful* return carrying
`is_error: true` in the result map. Wiring it means bridging
`ToolResponse.IsError` to ADK's error channel.

**`plugin/functioncallmodifier`** is registered with a predicate that
always returns `false` (`pkg/engine/plugins.go`), so it never applies.
PR #132 disabled it to fix a proto validation error; the manual JSON
schema surgery it was meant to replace is still in
`pkg/tools/bridge.go`.

**`tool/agenttool`** — `BuildResearchSubagent`, `BuildNamedSubagent`,
`BuildResearchAgentTool`, and `BuildNamedAgentTool` have no production
callers. The `task` tool still spawns a nested `engine.Run`.

**`tool/toolconfirmation`** — no tool declares `RequireConfirmation` or a
`ConfirmationProvider`, so ADK never emits `adk_request_confirmation` and
the handling in `run.go` / `agent_run.go` is unreachable. Approval is the
in-tool blocking path in `pkg/tools/env.go`.

**Artifacts** were dropped rather than wired. ADK ships only
`InMemoryService` and `gcsartifact`; ask's was rebuilt per turn and
nothing ever saved to it, so `loadartifactstool` could only return empty
while costing tokens on every request. Node outputs cover step handoff
and `pkg/memory` covers durable state.

**`util/instructionutil`** is deliberately not used. ask's instruction
text is user documentation inlined verbatim, not a template — see the
comment on `BuildInstructionProvider`.

---

## Planned

**Parallel / fan-out.** A `parallel` step kind alongside `loop`, compiled
to fan-out edges plus a `JoinNode`, with `NodeConfig.ParallelWorker` for
list-typed inputs. Needs builder UI and a store schema addition.

**Pause / resume and HITL.** `workflow.Persistence` plus
`Workflow.Resume`, and `NewRequestInputEvent` routed to ask's question
modal — replacing today's behaviour where workflow tabs auto-decline
every prompt.
