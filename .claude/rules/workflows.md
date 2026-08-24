---
paths:
  - "pkg/workflow/**"
  - "pkg/tools/artifact.go"
  - "pkg/tools/workflow.go"
  - "pkg/engine/workflow_run.go"
  - "cmd/ask/workflow_graph.go"
  - "cmd/ask/workflow_store.go"
  - "cmd/ask/workflows.go"
  - "cmd/ask/workflows_picker.go"
  - "cmd/ask/workflows_screen.go"
  - "cmd/ask/chat_workflow.go"
---
# Workflows

A workflow (`workflow.Def`, pkg/workflow/def.go) is a named list of
steps that runs as ONE ADK workflow graph against a source — an issue,
a chat transcript, or free text. `pkg/workflow` owns the schema, the
store, the compiler, the progress adapter, and the tracker; `cmd/ask`
only converts at the boundary (`workflow_store.go` aliases
`config.WorkflowDef` ↔ `workflow.Def`) and renders.

## Schema

- `Def{Name, Description, Steps, Scope, Plugin}`. `Scope` and `Plugin`
  are `json:"-"`: the storage location IS the scope, so every on-disk
  shape is scope-free. `Description` is the author's statement of what
  the workflow is for; `workflow_list` surfaces it so the agent judges
  fit against stated intent, not step names.
- `Step{Name, Kind, Provider, Model, Prompt, Steps, MaxIterations,
  ExitCondition}`. `Kind == ""` is an agent step; `Kind == "loop"` is a
  loop whose `Steps` are its inner agent steps. Loops are one layer deep
  — `Def.Validate` rejects a loop inside a loop, and the builder never
  offers "+ New loop" inside one. `EffectiveMaxIterations` defaults to 20.
- A step with an empty `Provider`/`Model` runs on the session's provider
  and model (`workflowStepModel`, cmd/ask/workflow_graph.go; the
  headless path falls back to the configured provider then
  `providers.DefaultProviderID()`). Provider ids come from the registry
  — see `providers.md`; never write one as a literal.

## Scopes and the store (pkg/workflow/store.go)

| Scope | Where | Notes |
|---|---|---|
| `user` | `projectConfig.Workflows.Items` in `~/.config/ask/ask.json` | machine-local, the default |
| `repo` | `<root>/.ask/workflows/<FileName(name)>.json` | committed; root via `config.ProjectRoot` so every worktree sees the same files |
| `global` | `~/.config/ask/workflows/*.json` | machine-local, visible from every project |
| `plugin` | the installed plugin's `workflows/*.json` | read-only (`Def.ReadOnly`); copy into another scope to edit |

- `ListAll` merges global → repo → user → plugin. Names are unique
  within a scope only. `ResolveByName` without a scope returns the first
  match in that order; `resolveWorkflowByName` (cmd/ask) and the
  mutating tools return `ErrWorkflowAmbiguous` instead of guessing.
- All writes go through `SaveAll`/`MutateWorkflows` under the config
  lock: the merged list is split by scope, user goes to ask.json, repo
  and global go through `syncWorkflowFiles` (write changed files, remove
  stale ones, remove an emptied dir). Plugin defs are never written back.
- `copyWorkflowDef` copies across scopes; a name clash in the target
  scope is an error that asks for `new_name` (the builder auto-suffixes
  `-2`, `-3`, … instead). `cloneWorkflowSteps` deep-copies the step tree.
- Malformed or duplicate files are skipped, never fatal.

## Compile → ADK graph (pkg/workflow/compile.go)

`CompileWorkflow(ctx, WorkflowAgentConfig)` turns a `Def` into
`Compiled{Workflow, AgentInfo}`:

- Every top-level step is one node, chained `Start -> n0 -> n1 -> …`.
  An agent step is an `AgentNode` over an `llmagent`; a loop step is an
  `AgentNode` over ADK's `loopagent` with the inner steps as sub-agents
  and `MaxIterations` from the def.
- `ToolsBuilder` receives a `StepRole{InLoop, IsTail, IsFinal}`. Only
  the tail inner step of a loop gets `exitlooptool` (`withExitLoopTool`):
  `exit_loop` sets `Actions.Escalate`, which is what `loopagent` watches,
  and no other ask tool touches `Actions`, so a non-tail step cannot
  break a loop. `end_turn` still accepts a `decision` param, but nothing
  reads `env.PendingEndTurn` — loop control is `exit_loop` only.
- Two `llmagent` settings carry the semantics and must not be dropped:
  `IncludeContents: IncludeContentsNone` (a step sees the previous node's
  output plus its own work, not every prior step's events rendered as
  prose) and `InstructionProvider: literalInstruction(...)` (never
  `Config.Instruction` — step prompts are user-authored, contain braces,
  and ADK interpolates that field and fails the run on the first one).
- `nodeConfig` attaches `RetryConfig` with `MaxRetries` attempts
  (default 3; negative disables).
- `agentNamer` sanitises and de-duplicates step names into ADK agent
  names (unique per graph, never `user`). `AgentInfo` maps each agent
  name back to `StepAgentInfo{StepIndex, …}`; inner loop agents map to
  their loop's top-level index.

## Running (one session for the whole graph)

- TUI: `Coordinator.runWorkflowGraph` (cmd/ask/workflow_graph.go) starts
  one provider session with `SkipAllPermissions: true, InWorkflow: true`,
  compiles with `tuiWorkflowCompileConfig` (the session's current tools
  plus `tools.WorkflowStepTools`, the session's MCP toolsets, the skill
  toolset), wraps the graph with `engine.WorkflowGraphAgent` (`agent.New`
  over `Workflow.Run` — `*workflow.Workflow` is not itself an
  `agent.Agent`), sets `sess.workflowAgent`/`sess.workflowProgress`, and
  queues one turn with `src.Display()`. Tool execution, cost accounting,
  and cancellation behave exactly as in a chat turn. The loop ends on
  `turnCompleteMsg` or channel close; `providerDoneMsg` before
  `turnCompleteMsg` is the order it depends on.
- Headless: `Engine.RunWorkflow` (pkg/engine/workflow_run.go) does the
  same on its own `session.InMemoryService`, then
  `IngestWorkflowMemory` files the session into `pkg/memory`.
- Both paths build identical graphs; keep `WorkflowCompileConfig` and
  `tuiWorkflowCompileConfig` in step when adding to either.

## Progress (pkg/workflow/progress.go)

`Progress.Observe(event)` turns the ADK event stream into
`RunnerListener` callbacks. A step starts when an event authored by its
agent arrives (`enter`), and completes when its successor starts, when
it calls `exit_loop`, or when `Finish(nil)` runs. `Finish(err)` leaves
every unfinished step unfinished and reports the failure. Never
fabricate step events — `TestProgress_FailureDoesNotFabricateRemainingSteps`
pins this. `captureToolSignals` reads `end_turn.summary` (the step's log
line; falls back to the step's last text via `firstLine`), `exit_loop`
(loop break note), and `finish_workflow` (the run's `FinishData`); the
TUI also reads `env.PendingFinishData` as a backup before `Finish`.

## Step contract

- `BuildStepInstruction(step, source, pc)` = the prompt, then
  `source.RefBlock()` (`Reference: owner/repo#N`, the chat transcript, or
  free text), then the save_artifact/todos guidance, then
  `EndTurnInstructionBlock` (the `end_turn` summary contract; inside a
  loop the iteration framing, and only the tail step is told about
  `exit_loop`). Previous-step output is not threaded in — the graph
  passes it as the next node's input.
- `tools.WorkflowStepTools(env, isFinal)` (pkg/tools/artifact.go):
  `save_artifact` + ADK `load_artifacts` on every step for structured
  handoff (one runner invocation ⇒ one `ArtifactService` spanning all
  steps), and `finish_workflow` on the final step only. ADK ships only
  `load_artifacts`; `SaveArtifactTool` is the save half.
- The model-facing `workflow_list/get/create/edit/delete/copy` tools are
  in pkg/tools/workflow.go; `workflow_list` disarms the todos guard
  (`env.MarkWorkflowsChecked`). Their placement is in `tools.md`.

## Tracker (pkg/workflow/tracker.go, cmd/ask/workflows.go)

- `workflow.GlobalTracker()` is the one instance; `workflowTracker()` is
  the TUI accessor. Keys are `Source.Key()`: `<provider>:<owner/repo>#<n>`
  for issues, `chat:<tab>:<nanos>:<nonce>` / `text:…` otherwise.
- `MarkWorking` is in-memory only (drops any disk record); `MarkStep`
  bumps the index; `MarkFinal` keeps `StartedAt` and persists `done` /
  `failed` into `projectConfig.Workflows.Sessions`. Only terminal
  statuses land on disk — runs are not resumable across restarts.
- Every transition calls the listener → `broadcastWorkflowStatus` →
  `workflowStatusChangedMsg` on a goroutine (never a synchronous
  `Program.Send` from inside `Update`). The kanban reads the tracker at
  render time for its `▸ ✓ ✗` glyph.

## Entry paths and the tab

- `f` on a focused issue (`dispatchIssueWorkflow`, issues.go) and
  `ActionChatWorkflow` (Ctrl+F, `dispatchChatWorkflow`) open the same
  picker (`workflows_picker.go`; Enter emits `spawnWorkflowTabMsg`). A
  key with a run already in flight focuses that tab; no workflows ⇒ a
  toast pointing at the builder.
- `app.supplantWorkflow` (tabs.go) runs the workflow INSIDE the origin
  tab: a tab already hosting a run or streaming a shell command refuses
  with a toast; a step provider without credentials refuses with
  `workflowProviderWarnings` (`tools.WorkflowProviderWarnings`); a tab
  that is mid-turn parks the request on `pendingWorkflow` and launches
  it when the turn completes. Otherwise the provider/session state is
  snapshotted into `workflowTabSnapshot`, the live session is killed,
  `skipAllPermissions` is forced on, every overlay is dismissed, and
  `globalCoordinator.RunWorkflow` starts.
- While `m.workflowRun != nil` the transcript shows only the step log:
  a `▸ name (provider/model)` header at `WorkflowStepStartedMsg`, the
  summary at `WorkflowStepDoneMsg`, `⟳ loop …` notes, and `✗ workflow
  failed: …` on failure. Assistant text, tool calls, tool results, and
  diffs are suppressed: while `m.workflowRun != nil` the arrival
  handlers simply don't push those items into `m.transcript` (the
  source of truth that `projectHistory` renders from). Tab titling
  is skipped.
- `workflowTabHandleKey`: `ActionTabClose` closes; scroll keys reach the
  viewport; Enter on a finished run restores the snapshot
  (`restoreSupplantedTab`, the step log stays in history) or closes a
  dedicated tab; everything else goes to `updateInput` so the user can
  queue text for the running step.
- Modal requests on a workflow tab never block: `askToolRequestMsg` gets
  `askReply{headless: true}` (the tool returns
  `WorkflowHeadlessAskNotice`), `finalizedPlanRequestMsg` is approved
  headless, `approvalRequestMsg` is denied, `sudoPasswordRequestedMsg`
  is cancelled.
- `closeTab` on a run that has not finished calls `MarkFinal(failed)`
  first.

## Builder (`ActionScreenWorkflows`, Ctrl+W / `/workflows`)

`workflowsScreen.updateKey` dispatches overlays first (confirm > rename
> prompt textarea > provider picker), then by pane:

| Pane | Keys |
|---|---|
| Left (list) | Enter open / `+ New workflow`; `r` rename; `e` edit Description; `d` delete (confirm); `c` copy to the next scope; `s` move (user → repo → global → user); Tab → right; Esc back to chat |
| Right (steps, `stepRows()` flat list) | Enter on an affordance row creates a step / loop / inner step, on a real row opens its detail; `d` delete (confirm); Tab/Esc → left |
| Right (step detail) | Up/Down over Name / Provider / Model / Prompt (loop: Name / Max iterations / Exit condition); Enter edits the field; typing on Name starts an inline rename; Esc → steps |

- Model opens the shared Ctrl+M picker (`openModelPickerForStep` sets
  `modelPickerState.stepTarget`; `applyModelPickerToStep` writes both
  `Provider` and `Model`). Do not add a builder-local model overlay.
- `runningGuard` blocks rename/edit/delete/move and step edits while
  the workflow is running anywhere in the process
  (`ActiveWorkflowNames`) or when it is a plugin workflow; `c` is the
  way out for plugin workflows.
- Every commit writes to disk immediately (`commitItems` →
  `saveAllWorkflows`).
