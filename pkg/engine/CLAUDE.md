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
  it; `InterruptTurn` cancels the turn's context; `Close` emits
  `ExitedEvent`. First turn emits `ModelInfoEvent`. Session id defaults
  to `ses-<modelID>`.
- Mid-turn input: `QueueMidTurn` → `MidTurnQueue` (types.go).
  `beforeModelCallback` drains it before the next model call, appends it
  as user content, persists it as an event, emits `MidTurnDrainedEvent`;
  leftovers at turn end become a new turn.
- `Coordinator` — tab id → `*Session` plus workflow cancel funcs
  (`Dispatch`, `InterruptSession`, `IsBusy`, `CancelWorkflow`). No UI
  types.
- Workflows: `Engine.CompileWorkflow`, `Engine.RunWorkflow`,
  `WorkflowGraphAgent`, `WorkflowCompileConfig` (shared by TUI and
  headless so both compile identical graphs). A run gets its own
  `session.InMemoryService()`. Detail in `.claude/rules/workflows.md`.

## Model and runner construction

- `ModelBuilder` (swappable) calls `p.BuildModel` and wraps it in
  `retryingModel` (retry.go). Every model in the process — tab titles,
  sub-agents, workflow steps — goes through it.
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
- Approval is deliberately NOT ADK's `toolconfirmation` flow (no
  suspend/resume for a chat turn): a mutating tool blocks on the handler
  and returns the denial inline. `IsConfirmationCall` /
  `UnwrapConfirmationCall` stay so an MCP server that declares
  confirmation renders the inner call.
- `EngineEvent` (`Kind()`, `GetTabID()`) + `EventListener`. Text deltas,
  `AssistantTextEvent` (one per completed text), status, tool call /
  result / diff, usage, cost, model info, todos, subagent and background
  task start/end, `DoneEvent`, `TurnCompleteEvent`, `ExitedEvent`,
  `MidTurnDrainedEvent`, `Workflow*Event`, `ExtensionsChangedEvent`,
  `MCPStatusChangedEvent`.
- Ordering contract: every turn ends with `DoneEvent` then
  `TurnCompleteEvent`, on success and on error. `cmd/ask/event_adapter.go`
  depends on it.
- Loop detection is `checkLoopDetection` in `cmd/ask/agent_run.go`, not
  here. There is no transcript compaction.

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
DisableSkillsPrompt, SystemPrompt})` emits, in order: `AgentCoderPrompt`
(static head; `InWorkflow` swaps three paragraphs for workflow-step
text) → `<env>` (cwd, git repo flag, platform, date, git status from
`AgentGitStatus`, 40-line cap) → `<project_instructions>` →
`<project_rules>` (eager) → `<included_docs>` (`@`-links, seeded from
each document's `Links`, resolved against its own `Root`) →
`<project_memory>` (the top-weighted concepts, when `memory.IsOpen()`,
2s timeout) →
`<available_skills>` (unless `DisableSkillsPrompt`) →
`<available_agents>` → `providers.SteeringPrompt`.

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
  `retry.go`, `plugins.go`, `workflow_run.go`.
- Contracts: `interaction.go`, `events.go`, `types.go`.
- Prompt: `prompt.go`, `prompt_links.go`, `rules.go`, `glob.go`.
- Discovery: `skills.go`, `skill_store.go`, `subagents.go`.
- Persistence: `store.go`.
