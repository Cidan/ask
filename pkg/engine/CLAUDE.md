# pkg/engine

The headless agent runtime. `cmd/ask` (the TUI) and the headless
`engine.Run` / `Engine.RunWorkflow` both sit on it. Imports ADK, GenAI,
`pkg/config`, `pkg/providers`, `pkg/memory`, `pkg/plugin`,
`pkg/workflow` — never Bubble Tea.

Related: `.claude/rules/tools.md`, `.claude/rules/providers.md`,
`.claude/rules/workflows.md`, `.claude/rules/skills-plugins.md`,
`pkg/memory/CLAUDE.md`.

## Entry points

- `engine.New(Options{Config, InteractionHandler, EventListener})`. A nil
  handler becomes `HeadlessInteractionHandler{AutoApproveTools: true}`.
- `Engine.Run(ctx, RunOptions)` — one headless turn. Provider:
  `RunOptions.Provider` → `Config.Provider` → `providers.DefaultProviderID()`;
  model via `providers.ResolveModelID`; effort defaults to `medium`; an
  empty `SessionID` mints a uuid. Builds the model with `ModelBuilder`,
  tools with the registered `ToolFactory`, the skill toolset with
  `NewSkillToolset`, and an `llmagent` named `ask_coder` whose
  instruction is `BuildSystemPrompt` behind an `InstructionProvider`.
- `Session` (session.go) — the per-tab runtime the TUI uses.
  `NewSession` starts the goroutine; `QueueTurn` / `QueueTurnSync` feed
  it; `InterruptTurn` cancels the turn's context, and the turn's
  `DoneEvent` then carries the context's error, whatever the runner
  returned — ADK v2.0.0 can end a cancelled turn with a panic it
  recovered from its own node (`range function continued iteration`),
  fixed upstream after v2.4.0; `Close` emits
  `ExitedEvent`. First turn emits `ModelInfoEvent`. Session id defaults
  to `ses-<modelID>`.
- User input goes through `Session.Submit` and the `TurnGate`
  (turn_gate.go), which keeps the turn state under one lock: a message
  either steers the turn that is running (`Steer`) or is refused and runs
  as a turn of its own (`QueueTurn`). Deciding "is a turn running?" apart
  from queueing is what used to strand a message sent as a turn ended.
  `beforeModelCallback` takes what was steered (`Take`) before the next
  model call — never once the turn was interrupted or the call's context
  is done, since that call will not be answered — appends it as one user
  content with its files (`TurnInput.Content`), persists it, and emits
  `MidTurnDrainedEvent`. What the turn never took comes back from `End`
  and runs as the very next turn. `InterruptTurn` marks the turn
  interrupted: `IsBusy` is false at once, so what the user sends after
  Esc is a new turn, not steering for one that is unwinding.
- Closing a turn (`closeTurn`): every turn ends with
  `providers.CloseTurn(llm)`; one that stopped before finishing
  (interrupted or failed) also gets `CloseInterruptedCalls`, which answers
  each of its function calls left without a response with
  `ToolInterruptedError`, in one appended event. A history holding an
  unanswered call is one providers reject, so without it every later turn
  of the session failed.
- `Coordinator` — tab id → `*Session` plus workflow cancel funcs
  (`Dispatch`, `InterruptSession`, `IsBusy`, `CancelWorkflow`). No UI
  types.
- Workflows: `Engine.CompileWorkflow`, `Engine.RunWorkflow`,
  `WorkflowGraphAgent`, `WorkflowCompileConfig` (shared by TUI and
  headless so both compile identical graphs). A run gets its own
  `session.InMemoryService()`. Detail in `.claude/rules/workflows.md`.

## Model and runner construction

- `ModelBuilder` (swappable) calls `p.BuildModel` and wraps it in
  `usageModel` (usage.go) and `retryingModel` (retry.go). Every model in
  the process — tab titles, sub-agents, workflow steps — goes through it.
- `usageModel` completes the `providers.Usage` record on every final
  response — derived from the metadata when the adapter attached none,
  stamped with the build's provider and model, priced unless the
  provider reported a cost, and — for a context reading — stamped with
  the window it is measured against (`ResolveContextWindow`, the window
  the compactor uses) unless the adapter set one, so it is persisted with
  the session event and a resumed meter needs no provider to relearn it.
  Read a response's record with `ResponseUsage` (falls back to the
  metadata for a model not built here); `StampUsage` attributes and
  prices one from a caller that knows its model (the memory extractor,
  deslop, tab titles). `UsageEvent` carries the record (`NewUsageEvent`);
  `RunResult.Usage`/`CostUSD` total a headless run's calls, which the
  TUI charges to the parent session for a sub-agent. Both wrappers
  forward `io.Closer`, `providers.HistoryRebaser`,
  `providers.ContextWindowResolver`, and `providers.TurnCloser`.
- `CloseModel(m)` closes a model that holds resources (the Claude Code
  provider forks a `claude -p` child on first use and implements
  `io.Closer`; `retryingModel` forwards `Close`). Callers that own a
  model for a bounded scope close it: `Session.Close`, `engine.Run`
  (deferred), and the TUI's `agentSession.shutdown` / tab-title path. A
  no-op for in-process models.
- `retryingModel`: a failure before any content streamed retries without
  bound while `isTransientError` holds; backoff from
  `config.AgentRetryOptions` (`UI.Retry.InitialDelayMs`,
  `UI.Retry.BackoffFactor`) clamped to `retryMaxDelay` (30s). Transient:
  429, 5xx, overload, timeout, connection errors, OpenRouter's "Provider
  returned error". Hard: 400/401/403/404, invalid key, context length,
  `invalid_request`, anything unclassified. After content streamed, the
  error surfaces as-is.
- `RunnerBuilder` (swappable): `runner.New` with `AppName: "ask"`,
  `AutoCreateSession`, `MemoryService` = `pkgmemory.Default()` when open
  else in-memory, `ArtifactService` = `artifact.InMemoryService()`,
  plugins = `DefaultPlugins()`.
- `DefaultPlugins` is only `retryandreflect` (2 retries, invocation
  scope): a tool returning a Go error gets guidance and a retry in the
  same turn. No `functioncallmodifier` — `description` is a real field on
  every native tool's params struct.
- `GenerateStream` is the swappable GenAI streaming hook `cmd/ask` uses.

## Interaction and events

- `InteractionHandler`: `AskQuestion`, `RequestApproval`, `ConfirmPlan`,
  `RequestSudoPassword`. The TUI implements it over its modals.
  `HeadlessInteractionHandler` answers question/plan with `Headless:
  true`, approval with `AutoApproveTools`, sudo with `Cancelled: true`.
  The optional `SudoPasswordPrompter` capability receives the whole
  `SudoPasswordRequest` (prompt plus `Retry`, set when the same sudo asks
  again after rejecting a password) in place of `RequestSudoPassword`.
- Approval is deliberately NOT ADK's `toolconfirmation` flow (no
  suspend/resume for a chat turn): a mutating tool blocks on the handler
  and returns the denial inline. `IsConfirmationCall` /
  `UnwrapConfirmationCall` stay so an MCP server that declares
  confirmation renders the inner call.
- `EngineEvent` (`Kind()`, `GetTabID()`) + `EventListener`. Text deltas,
  `AssistantTextEvent` (one per completed text), status, tool call /
  result / diff, usage, cost, model info, todos, subagent and background
  task start/end, `DoneEvent`, `TurnCompleteEvent`, `ExitedEvent`,
  `MidTurnDrainedEvent`, `ContextCompactedEvent`, `Workflow*Event`,
  `ExtensionsChangedEvent`, `MCPStatusChangedEvent`.
- Ordering contract: every turn ends with `DoneEvent` then
  `TurnCompleteEvent`, on success and on error. `cmd/ask/event_adapter.go`
  depends on it.
- Loop detection is `checkLoopDetection` in `cmd/ask/agent_run.go`, not
  here.

## Auto-compaction (`compact.go`)

- `Compactor` keeps one agent's conversation inside its model's context
  window, **the same way on every provider** — no provider is exempt.
  One compactor per agent and model: `BeforeModel` cuts, `AfterModel`
  (an `llmagent.AfterModelCallback`) records the model's usage. Built in
  `session.go`, `run.go`, `workflow_run.go` (one per workflow step, via
  `workflow.WorkflowAgentConfig.ModelCallbacksBuilder`),
  `cmd/ask/agent_run.go` (`newCompactor` + `compactCallbacks`) and
  `cmd/ask/workflow_graph.go` (one per step). Never share one across
  agents: its watermark and usage reading belong to one model.
- **It rewrites `LLMRequest.Contents` only; the transcript is never
  touched.** ADK rebuilds `Contents` from the append-only event log on
  every model call, so the cut is a view the model sees and `/resume`
  does not. `ContextCompactedEvent` tells the UI it happened.
- Trigger `CompactTriggerRatio` (0.90) of the window, target
  `CompactTargetRatio` (0.50), both hardcoded; `AutoCompactEnabled`
  reads the one config toggle (`config.AutoCompact`, on when nil). The
  TUI and `Session` read it per call; switched off, they call `Reset`,
  which drops a standing cut so the whole history goes out again.
- **The window is asked for on every call**, never fixed when the
  compactor is built: `providers.ResolveContextWindow(ctx, opts.Model)`,
  falling back to `CompactOptions.ContextWindow` for a model that cannot
  tell. A provider can learn the real window after the session starts;
  a window fixed at construction made Claude Code compact at 90% of the
  catalog's 200k guess on a 1M model. `CompactionResult.ContextWindow` is
  the window the cut was measured against, so the notice agrees with the
  meter.
- **A cut may land anywhere except on a tool result**: at a user
  message, or at a model reply — between two tool round-trips of one
  turn, the only place a long agentic turn (one prompt, many tool calls)
  can be cut at all. ADK puts a result in the content right after its
  call, so this never orphans a call; cutting on a result would, and
  OpenRouter rejects that with a non-retryable 400.
- Index 0 is pinned (the original request) and an elision notice is
  spliced in behind it — the fixed `compactElisionNotice` without an
  archive, `historyNotice` with one. Everything else, mid-turn steering
  included, is dropped oldest first.
- The boundary is remembered as a `contentMark` (fingerprint plus
  ordinal, since a transcript can repeat a content verbatim). Without
  that watermark the next call would observe the reduced usage, not
  trigger, send the full history again, and oscillate across 90%. The
  fingerprint is role plus **first part only**: the memory recall hook
  appends a `<memory>` part to the latest user message on every request
  (not persisted), and that must not stop a watermark resolving a turn
  later.
- **Model that holds its own history**: whenever the view's start moves
  (a new cut, a watermark that no longer resolves, a `Reset`) the
  compactor calls `providers.RebaseHistory(opts.Model)`; Claude Code's
  model then replaces its child with one seeded from the cut view.
  `retryingModel` forwards `RebaseHistory`.
- Sizing is a local char estimate calibrated per call against the
  usage record's `ContextTokens` (the provider's total) reading — what the model held after its
  latest reply, i.e. the view up to the request's last model content.
  Never the prompt count: Claude Code reports only the uncached slice
  there. Before any reading (a resumed session) the raw estimate
  triggers on its own. The system instruction and tool declarations
  (including `ParametersJsonSchema`, where ADK's function tools keep
  their schema) are not in `Contents` and cannot be dropped, so they are
  subtracted from the target as a floor.
- Tests (`compact_test.go`): unit tests plus `TestScenario_*`, real
  `Session`s over a 10k-token window whose scripted model counts tokens
  denser than the estimator. `compact_claudecode_test.go` runs the real
  Claude Code model against a fake `claude` child
  (`providers.ClaudeCodeStart`); `compact_workflow_integration_test.go`
  compacts a headless workflow step.

## History archive (`history*.go`)

What a cut drops is kept, not lost. `CompactOptions.History` is the
agent's `HistoryArchive`; every compactor site sets one, and the
compactor owns it (`Compactor.Close`).

- **Every dropped content is archived exactly once.** A new cut
  archives `contents[archivedTo:start]` as the next chunk, where
  `archivedTo` is where the archive's last chunk ended (its end mark is
  that cut's watermark) — 1 when it does not resolve. Re-applying a
  standing cut archives nothing. A failed append heals at the next cut.
- **The archive's end is where a fresh view resumes.** With no cut
  standing (a resumed session, a rebuilt compactor, auto-compaction
  switched back on after `Reset`) and an end mark that resolves, the
  compactor seeds its watermark from it, so the model sees exactly the
  view it had and nothing is archived twice. An end that does not
  resolve belongs to a context the request does not share (a workflow
  loop's next iteration): no seeding, and its own cut archives from 1.
- **Storage**: SQLite (`mattn/go-sqlite3`, already linked for memory)
  with an FTS4 index — FTS5 needs a build tag every importer of this
  package would inherit. File archives are `<id>.history.db` next to
  `<id>.json` (`HistoryArchivePath`: beside the transcript wherever
  `PathFor` finds it, else where `Create` will write it), created on the
  first chunk, 0600. Workflow steps use `NewMemoryHistoryArchive`. One
  connection per archive (an in-memory database is per connection), so
  never run a query while another's rows are open.
- **Rendering** (`historyEntries`): one entry per part — user,
  assistant, thinking, tool call (name + indented args JSON), tool result
  / tool error (`ToolResultText`), image / file placeholders — with text
  runs of one kind merged. Thought signatures and the recall hook's
  `RecallBlock` part on a user message are left out. Each entry is a
  header line `[entry N: kind name]` plus its body; line and entry
  numbers run on across chunks.
- **Tools** (`HistoryTools`): `history_summary` (one chunk, or the list
  by headline), `history_read` (line or chunk anchor, forward/backward,
  `next_line`, lines clipped at `historyLineCap`, output at
  `historyReadCap`), `history_search` (FTS4 keywords — all words,
  phrases, `prefix*`, OR, NOT — with a quoted-terms retry on bad syntax,
  or `literal` substring; one hit per line, newest first). The compactor
  packs them onto the request itself once the archive holds a chunk
  (`packHistoryTools`, ADK's own `ProcessRequest` packing), bound to its
  own archive; a name the agent already has keeps its tool.
- **Summary at the cut**: `CompactOptions.Summarizer` runs before the
  request goes out, oldest pending chunk first, so the cutting request
  already carries the new chunk's summary. `ModelChunkSummarizer` is a
  sub-agent (`history_summarizer`) on a fresh `ModelBuilder` instance of
  the agent's own provider and model, with the history tools over the
  same archive, in its own in-memory session, under a context that
  inherits only the turn's cancellation. Failures are recorded and
  retried at the next cut or seed, up to `historySummaryMaxAttempts`; an
  interrupted attempt does not count. `OnSummarize(chunk, running)`
  drives the status (`CompactionStatus`).
- **Notice** (`historyNotice`): the chunks by headline (older than
  `compactNoticeMaxHeadlines` folded), the newest chunk's summary, and
  the tool names. Built when the view's start moves and reused verbatim
  until it moves again, so it never busts the prompt cache on its own.
  Cut planning reserves its size and the tools' declarations.
- `CompactionResult` / `ContextCompactedEvent` carry `Chunk`,
  `Archived`, `Headline`; `CompactionSummary` names the chunk.
- Tests: `history_test.go` (store), `history_tools_test.go`,
  `compact_history_test.go` (archive-once, resume, reset, loop
  iteration, summary order and retries, tool packing, budget),
  `history_summarize_test.go` (the summarizer agent, a long `Session`
  whose archive plus final view must equal its transcript before and
  after resume, and `Run` billing).

## Tools

- `Tool = tool.Tool` (ADK); `AsADKTools`, `RunADKTool`,
  `NewStandaloneAgentContext` (an `agent.Context` outside a runner).
- `RegisterToolFactory` is called from `pkg/tools/core.go`'s `init`.
  `ToolFactoryArgs` carries cwd, tab id, permission flags,
  `AttachWebSearch`, `WorkflowStep`, `WorkflowFinalStep`.
- `ToolResultText(resp)` finds any tool result's human-readable field and
  whether it is an error; `AppendToolResultText` appends to it. Every UI
  path reads results through these. `ExtractToolInfo` → `ToolInfo`.

## Sessions (store.go, types.go)

- `FileSessionService` implements ADK `session.Service` over atomic JSON
  at `~/.config/ask/agent-sessions/<provider>/<EncodeProjectDir(cwd)>/<id>.json`
  (`NewFileSessionServiceWithBaseDir` for tests). `AppendEvent` writes
  each non-partial event as it arrives — nothing is buffered to turn end.
- `EncodeProjectDir`: non-alphanumerics → `-`; over 200 chars it is cut
  and suffixed with a base-36 hash. `PathFor(id)` checks the cwd's
  directory, then globs every project directory.
- `StoredSessionFile{Version: 1, AppName: "ask", UserID: "user"}`;
  `MessagesFromEvents`, `ReadStoredSessionFile`, `SessionPreview*` feed
  the resume picker; `SessionStore` is the older `Save` / `SaveEvents`
  adapter over the same service.
- `Message` has typed parts (`ThoughtPart`, `ToolCallPart`,
  `ToolResultPart`, `FilePart`) with a custom `UnmarshalJSON`, so parts
  survive a JSON round-trip. The TUI's resume, replay, and cross-provider
  `materialize` are `cmd/ask/agent_session.go` over this service.

## Prompt assembly (prompt.go)

`BuildSystemPrompt(PromptOptions{Cwd, InWorkflow, GitStatusFn,
DisableSkillsPrompt, SystemPrompt, AgentInstructions})` emits, in order:
`AgentCoderPrompt` (static head; `InWorkflow` swaps three paragraphs for
workflow-step text) → `<env>` (cwd, git repo flag, platform, date, git
status from `AgentGitStatus`, 40-line cap) → `ProjectContext(cwd)` →
`<available_skills>` (unless `DisableSkillsPrompt`) →
`<available_agents>` → `providers.SteeringPrompt`.

- `ProjectContext(cwd)` is everything any session in cwd is told about
  its project: `<project_instructions>` → `<project_rules>` (eager) →
  `<included_docs>` (`@`-links, seeded from each document's `Links`,
  resolved against its own `Root`) → `<project_memory>` (the top-weighted
  concepts, when `memory.IsOpen()`, 2s timeout). A tab, a named agent,
  and a workflow step all get it — a headless run is a tab without a
  screen, never a session that knows less about the project. Path-scoped
  rules and instruction files below cwd arrive on reads
  (`WrapContextAwareTools`), which assumes this block was delivered.
- A named agent (`AgentInstructions`, `RunOptions.AgentInstructions`):
  its own instructions replace `AgentCoderPrompt`, no
  `<available_agents>` (it cannot start one), and the headless steering
  (`SteeringOptions.Headless`: no delegation, no stopping for a go-ahead).
  `SystemPrompt` stays a verbatim whole-prompt override.
- Workflow steps: `WorkflowStepInstructions(cwd)` (workflow_run.go) is
  the `InstructionBuilder` for TUI and headless runs alike — the step's
  own `BuildStepInstruction`, then `<env>`, `ProjectContext`, and the
  workflow steering, the shared part built once per run.
- Headless tools (`tools.BuildCoreTools`, `BuildSubagentTools`) come
  wrapped like a tab's: memory-aware file tools and context-aware reads.
  `RewrapContextAwareTools` gives a tool set fresh lazy-context state —
  each workflow step starts its own context, so a rule an earlier step
  was handed must reach it again.
- Built once per session and reused verbatim for prefix caching.
  `BuildInstructionProvider` caches it and appends only the state blocks
  `<system_reminder>`, `<step_incomplete>`, `<extra_instructions>`.
- Never `instructionutil.InjectSessionState`, never user-authored text in
  `llmagent.Config.Instruction`: ADK treats `{name}` as a state lookup,
  deletes unknown `{name?}`, and fails on a brace it cannot resolve.
  Always an `InstructionProvider`.

### Context files

- `AgentContextFileNames`: `CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md`,
  `agents.md`, `CRUSH.md`, `.cursorrules`,
  `.github/copilot-instructions.md`.
- `AgentContextScopes(cwd)`: `~/.claude` first (own link root), then
  project root (`config.ProjectRoot`) down to cwd, general before
  specific.
- Dedupe within a directory by lowercase name, across directories by
  `ContextFileRealPath` (`EvalSymlinks`) — `AGENTS.md -> CLAUDE.md` loads
  once. Whitespace-only files skipped.
- `AgentContextFileCap` = 128 000 bytes per document (context file,
  rule, linked doc). A backstop against pathological files, not a budget
  — raise it before a real document loses its tail.
  `TruncateInstructionDoc` cuts on a line boundary, never splits a rune,
  appends a notice with file and byte counts.
  `TestAgentContextFileCap_FitsRealInstructionFiles` checks this repo's
  own files fit.
- `LoadedContextDoc.Links` are extracted from the FULL body before
  truncation. Seed link loading from `Links`, never by rescanning `Body`.

### `@`-links (prompt_links.go)

- `@path/to/file.md`, matched after `StripFencedCodeBlocks` — links in
  fences are never followed.
- `ResolveContextLink`: relative only (no `/`, `./`, `../` prefix), no
  `..`/`.` segments, `.md` case-insensitive, inside the root, regular
  file.
- `LoadContextLinksFrom(root, links)`: breadth-first, deduped by absolute
  path, each file scanned for nested links before truncation,
  whitespace-only skipped. `LoadContextLinks(root, bodies)` rescans
  bodies and is only for callers with no `Links` list.
- Rendered sorted by path. Lazy surfaces resolve their own links: JIT
  rules append `### Included from <path>`, skills append `<file path=…>`
  after `<loaded_skill>`, subagents append `## @-linked docs`.

## `.claude/rules/` (rules.go)

- `RuleSearchScopes`: `~/.claude/rules` (root `~`) then
  `<projectRoot>/.claude/rules`. Recursive, symlink-following,
  cycle-guarded, `.md` only, empty bodies skipped; keyed by path relative
  to the rules dir, so a project rule replaces a user rule of the same
  label.
- Frontmatter: only `paths:` — block list, inline `[a, b]`, or scalar,
  through `UnquoteYAML` so brace patterns survive. No `paths` ⇒ eager.
- `GlobMatch` (glob.go): `**` spans segments, `path.Match` per segment,
  one level of `{a,b}`; matched against the project-root-relative slash
  path.
- Eager rules → `<project_rules>`. Scoped rules ride
  `WrapContextAwareTools`, which wraps `read`, `glob`, `grep`, `ls`:
  - on `read`, each matching scoped rule fires once per session
    (`firedRules`), appended as `## Rule for <rel> (<label>)` plus its
    linked docs;
  - on any of the four, the target's directory and every parent up to
    the root are checked once (`seenCtx`) for instruction files the
    model has not seen (`seenCtxFile`, seeded from
    `AgentContextFiles(cwd)`), appended as `## Project instructions from
    <rel>`. A `CLAUDE.md` below cwd arrives this way, once. Keep the
    seeding — without it a read next to the root `CLAUDE.md` re-sends it.
  - `RelPath` is `""` outside the root; such paths match nothing.

## Skills, subagents, plugins

- Skill roots (later wins): `~/.config/ask/skills`, `~/.agents/skills`,
  `~/.claude/skills`, then `.agents/skills`, `.claude/skills`,
  `.ask/skills` under cwd and the project root; enabled plugins add
  `plugin:name`. Agent roots: `~/.config/ask/agents`, `~/.claude/agents`,
  then `.claude/agents`, `.ask/agents`.
- `NewSkillSource` is the ADK `skill.Source`; `BumpSkillsGeneration`
  makes every live source rescan on next use; `ExpandSkillInvocation`
  expands `/name args`.
- `SubagentToolNames`: empty or `*` ⇒ `AllSubagentTools`, else the
  listed names filtered to that set — never `task`, `ask_user_question`,
  `end_turn`.
- `skill_store.go` is the CRUD behind the tools and the browser; plugin
  items are read-only (`Origin.Editable`). Detail:
  `.claude/rules/skills-plugins.md`.

## Memory

`memory.IsOpen()` / `memory.SystemBlock` feed `<project_memory>`;
`pkgmemory.Default()` is the runner's `MemoryService` when open.
`memory_extract.go` is the write side: `MemoryExtractor` (one worker,
bounded drop-oldest queue, `Close` cancels in flight, `Drain` for tests)
turns a finished turn into concepts with one small model call on
`MemoryExtractModel` (config `memory.{provider,model}`, else the session's
provider and `providers.CheapestModel`). `EnsureMemoryExtractor` /
`CloseMemoryExtractor` own the process-wide instance;
`EnqueueMemoryTurn` is what `Run` (unless `RunOptions.SkipMemory` — set
for sub-agents) and the TUI call; `IngestWorkflowMemory(ctx, sessSvc,
sessionID, cwd)` reduces a finished workflow session to its last exchange
and enqueues it. `AppendTouchedFile` collects the read/write/edit paths a
turn touched. `DebugLog` is the engine's debug seam (the TUI points it at
`debugLog`). Detail in `pkg/memory/CLAUDE.md`.

## File map

- Runtime: `engine.go`, `run.go`, `session.go`, `coordinator.go`,
  `retry.go`, `usage.go`, `compact.go`, `plugins.go`, `workflow_run.go`.
- History archive: `history.go` (store, rendering, read, search),
  `history_tools.go`, `history_summarize.go`.
- Contracts: `interaction.go`, `events.go`, `types.go`.
- Prompt: `prompt.go`, `prompt_links.go`, `rules.go`, `glob.go`.
- Discovery: `skills.go`, `skill_store.go`, `subagents.go`.
- Persistence: `store.go`.
