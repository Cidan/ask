---
paths:
  - "pkg/tools/**"
  - "cmd/ask/agent_tools*.go"
  - "cmd/ask/agent_run.go"
  - "cmd/ask/agent_provider.go"
  - "cmd/ask/tool_output.go"
---
# Agent tools

`pkg/tools` builds every tool the agent can call. `cmd/ask/aliases.go`
re-exports them as `agentXTool` names; `setupAgentSessionTools`
(`cmd/ask/agent_provider.go`) assembles the TUI session's surface, and
`BuildCoreTools` / `BuildSubagentTools` (`pkg/tools/core.go`) do the same
for the headless engine through `engine.RegisterToolFactory`. The
ADK-facing seams live in `pkg/engine`: `RunADKTool`, `ExtractToolInfo`,
`ToolResultText`.

## Contract (pinned by `pkg/tools/contract_test.go`)

- Build a tool with `NewTypedTool(name, description, handler)` over two
  real structs: `func(ctx agent.Context, p Params) (Result, error)`.
  ADK derives both the input and the output schema from them. No
  `map[string]any` params, no `any` as the result type.
- `agent.Context`, never `context.Context`.
- Failure is a Go error (`return Result{}, errors.New(...)`), not a flag
  on the result. ADK's `OnToolErrorCallback` keys on the error.
- One `Run` shape: `Run(agent.Context, any) (map[string]any, error)`.
  `invoke_tool` and the MCP tools implement it by hand; everything else
  gets it from `functiontool.New`.
- Every native tool takes a required `description` param: a
  model-authored phrase (under 10 words, `ToolPhraseFieldDoc`) saying
  what the call is doing. Declare it as a static field on the params
  struct. `NativeBridgeTool` adds it to the input schema automatically.
- `NativeBridgeTool[In, Out]` adapts an MCP-style handler
  (`func(ctx, In) (*mcp.CallToolResult, Out, error)`) to an ADK tool
  returning `BridgeResult[Out]{Content, Data}`; an `IsError` result
  becomes a Go error. The `workflow_*`, `linear_*`, and extension tools
  are built this way. `FlattenNullableTypes` strips `"null"` from
  schema type arrays for strict validators.
- A result's human-readable text lives in one of
  `toolResultTextFields` (`content`, `listing`, `output`, `body`,
  `results`, `memories`, `answers`, `report`, `outcome`, `note`,
  `notice`, `summary`, `pkg/engine/types.go`). `ToolResultText` is the
  one function the UI uses to read any result; give a new result struct
  one of those field names.

## Core (wire) vs deferred registry

The wire toolset is the set of definitions sent on every request. It is
built once per session and never changes mid-session (`refreshToolset`
only rebuilds the deferred list; `s.coreTools` is fixed).

Core today (`setupAgentSessionTools`): `read`, `write`, `edit`, `glob`,
`grep`, `ls`, `bash`, `job_output`, `job_kill`, `fetch`, `todos`,
`load_memory`, `preload_memory`, `task`, `ask_user_question`,
`end_turn`, `search_tools`, `invoke_tool`, `web_search`; plus
`finalized_plan` and the six `workflow_*` tools outside a workflow run,
and `finish_workflow` on a workflow's final step (`save_artifact` /
`load_artifacts` on every step, `WorkflowStepTools`). `task` exists only
in the TUI (`cmd/ask/agent_tools_task.go`); `pkg/tools.CoreTools` is the
same list without it.

Deferred registry (`s.deferredBase` + `s.mcp.Tools()`): the `linear_*`
twins, the memory set (`memory_index`, `memory_reinforce`,
`memory_demote`, `memory_forget` — `tools.MemoryTools`), the extension
tools (`skill_*`, `agent_*`,
`marketplace_*`, `plugin_*`, `skill_publish`, `skill_pull`), and every
MCP tool as `mcp__<server>__<tool>`. Registry tools are real and
callable but absent from the wire definitions: the model finds them
with `search_tools` (`*`, `prefix*`, or substring; each match carries
name, description, full `input_schema`) and calls them with
`invoke_tool`.

**New tools go in the registry.** Append to `s.deferredBase` (TUI) or
the registry func (engine). A core slot costs context on every call of
every session; the bar is "the agent cannot work without seeing it
unprompted". The documented exceptions are `web_search`, `fetch`,
`finalized_plan`, and the `workflow_*` tools (the workflow guard below
makes the model call `workflow_list` directly, so a `search_tools`
round-trip first would be pure overhead).

`invoke_tool` invariants (`pkg/tools/registry.go`):

- It checks the inner tool's `required` fields itself and errors on a
  missing one — ADK validates only wire tools.
- The invoke-level `description` phrase is copied into the inner params
  only when the inner tool requires `description` (the natives). MCP
  tools never receive keys they did not declare.
- A core name passed to `invoke_tool` errors with "call it directly".
- The response is the inner tool's response, verbatim.
- `UnwrapInvokeToolCall` maps an invoke call back to the inner name +
  params. Both the live emit (`cmd/ask/agent_run.go`) and history
  replay (`cmd/ask/agent_session.go`) use it, so transcripts, the
  status line, and resumed sessions show the real registry tool.

## Display

- `toolCallPhrase(input)` (`cmd/ask/tool_output.go`) accepts the
  `description` value only when it is a single line of at most 120
  chars; payload fields named `description` on MCP/bridge tools never
  become a headline.
- Short mode (default) renders the phrase as the whole entry
  (`▸ bash — Looking for the latest files`). Calls without a phrase
  fall back to `shortToolFields`, an allowlist keyed by tool name; add a
  new built-in's highest-signal field there. Full mode adds every input
  as `key: value` rows. Off hides tool calls.
- The phrase also becomes the stream status while the call runs.

## Permissions and guards

- `env.ApprovalDenied(ctx, name, input)` (`pkg/tools/env.go`) is the
  one gate. It is a no-op when `SkipPermissions` is set (workflow tabs,
  sub-agents); otherwise it asks the `InteractionHandler` and returns
  the denial text, which the tool returns as its error. The text tells
  the model not to retry. Gated: `bash` outside `safeShellCommands`,
  `write`, `edit`, `fetch`, `web_search`, `memory_index`, `memory_forget`,
  `marketplace_add`, `plugin_install`, `skill_publish`, `skill_pull`.
  `invoke_tool` adds no gate of its own.
- `write` / `edit` on an existing file require a prior `read` in this
  session and refuse when the file changed on disk since
  (`CheckReadBeforeMutate`). `edit` needs a unique `old_string` unless
  `replace_all`; CRLF files are edited as LF and written back as CRLF.
  Both emit `ToolDiffEvent` (`EmitFileDiff`).
- `read` rejects image extensions (`ImageExts`), caps at `MaxReadLines`
  / `MaxReadBytes`; every tool output is cut middle-out at
  `MaxToolOutput` (`TruncateMiddle`).
- `GateTodosBeforeMutate` (config `UI.GateTodosBeforeMutate`, default
  off) turns on two things: `write` / `edit` refuse until a todos list
  has been applied this session (`RequireTodosNotice`), and the todos
  tool's workflow guard. The guard fires at most twice per session, only
  in a project that has workflows: first when `todos` is called before
  `workflow_list` (`WorkflowGuardTodosNotice`, list not applied), then
  when `todos` is called after checking but without a workflow being
  dispatched or inline execution being approved
  (`WorkflowDecisionGuardNotice`). `workflow_list` sets
  `WorkflowsChecked`; `finalized_plan` sets `WorkflowsChecked` +
  `WorkflowRunDispatched` on either approval path. With the flag off
  all of this is inert.
- `todos` replaces the whole list, allows one `in_progress` item, and
  appends a nudge to every ack telling the model when to call again.

## bash and jobs

- `RunShell` (swappable; `RunShellProcess` runs `$SHELL -c` with
  `Setpgid`) is the exec seam for tests. Default timeout 120s, max 600s;
  timeout kills the process group.
- `safeShellCommands` (`pkg/tools/bash.go`) is the exact-word /
  prefix allowlist that skips the approval prompt.
- `run_in_background` registers a `Job` in `env.Jobs`; `job_output`
  (optionally waiting up to 30s) and `job_kill` (SIGKILL to the group)
  manage it. `BgTaskStartedEvent` / `BgTaskEndedEvent` drive the UI.
  The `task` tool's background mode rides the same manager.
- Output passes through `ApplyBashFilter(command, raw, exitCode)`
  unless `disable_token_savings`; savings are tallied in
  `~/.config/ask/savings.json` (`RecordSavings(base, rawTokens,
  savedTokens)`, keyed by `ExtractBaseCommand`; `LoadSavings` reads it
  back, `/savings` opens an interactive overlay — a two-level tree grouped
  by base command (go, git, make…) with a RUNS/SAVED/%/IMPACT column
  grid and a fixed-width rtk-gain-style impact bar,
  Enter/→ to expand a base into its subcommands, ← to collapse,
  type-to-filter (auto-expands matches), Tab to sort by saved/runs/name
  (`modeSavings`, `cmd/ask/savings_screen.go`, composited in `tabs.go`
  like the model picker; bars scale to the largest base's saving;
  `savings_view.go` holds the token/percent formatters). It delegates to
  the **filter registry**
  (`pkg/tools/filters`): an ordered list of command-aware compressors —
  hand-written aggregators (`go test` NDJSON→summary + text RUN/PASS strip
  — `GoFilter` claims only `go test`; `git` status→porcelain /
  log→short-sha / large-diff→per-file stats / transport noise, `pytest`
  success-collapse + failures-only, npm-family install noise) first, then
  the declarative `ruleTable` (`rules.go`: `make`, `go build`/`vet`
  (pass/fail) + `go mod`/`get` (strip) + `go run` (passthrough),
  `cargo build`/`cargo test`, `jstest` (vitest/jest), `gradle`, `pip`,
  `uv`, `poetry`, `bundle`, `mypy`, `ruff`, `cmake`, `bazel`/`bazel build`,
  `dotnet build`, `terraform`, `docker build` — each a `Rule` of
  strip/keep/replace/summarize/cap patterns, RTK's TOML model in Go; a
  `Rule` may set `PassFail` so a success collapses to one "ok" line and a
  failure still shows the filtered error. Granularity is just the `Command`
  regex — `^make\b` is whole-program pass/fail, `^go\s+build\b` is one
  subcommand; a hand-written filter must claim only its specialty (e.g.
  `go test`) so subcommand rules aren't shadowed),
  then the universal fallback (ANSI strip, blank squeeze,
  consecutive-run dedup `(xN)`, middle-out cap at `filters.MaxLines`).
  `exitCode` steers verbosity: a success may collapse to a summary line,
  a failure preserves detail, and an unmodeled failure passes through
  raw (a running background job reads as exit `-1` so a partial stream
  is never collapsed); `Summarize` fires only on exit 0. `BaseCommand`
  is the one command parser both the registry and the ledger key agree
  on. New long-tail commands are a new `Rule` in `rules.go`; complex
  aggregation is a new `Filter` in its own file, registered ahead of the
  rules.
- **Raw-output recovery** (`pkg/tools/spill.go`): when filtering is lossy
  and the raw exceeds `SpillThreshold` (30 KB) it spills to
  `$TMPDIR/ask-spill/` (`SpillRaw`; `ASK_SPILL_DIR` overrides for tests;
  pruned after 7 days). `BashResult`/`JobOutputResult` carry
  `raw_path`/`raw_lines` (`MaybeSpill` / `spillIfLossy`), and the read
  tool's offset/limit continuation pages the untouched bytes back. It is
  the recovery path, so it runs even under `disable_token_savings` when
  the middle-out cap dropped content.
- `sudo` must be invoked as `sudo -A` (`ValidateSudoCommand`). The
  tool exports `SUDO_ASKPASS=<ask binary>` plus `ASK_SUDO_SOCKET` /
  `ASK_SUDO_TOKEN` / `ASK_SUDO_TABID`; `ask` run as the askpass helper
  (`RunAskPassHelper`, dispatched from `cmd/ask/main.go`) connects to
  the per-process unix socket (`EnsureSudoIPCServer`, peer-uid checked)
  and the TUI answers with the password modal
  (`cmd/ask/sudo_password.go`).

## fetch and web_search

- `fetch`: HTTP GET, `FetchMaxBytes` (100 KB) cap, HTML→text via
  `HTMLToText`, `FetchClient` swappable.
- `web_search` is always on the wire and always Brave-backed
  (`BraveSearch`, `BraveSearchClient` swappable). The key comes from
  `config.ResolveBraveAPIKey` (config `webSearch`, else
  `BRAVE_API_KEY`). Without a key the tool returns a non-error
  `WebSearchNoKeyNotice` telling the model to finish the task and point
  the user at `/config → Web Search`.

## Modal tools (`pkg/tools/ask.go`)

- `ask_user_question` → `Interaction.AskQuestion`; a headless reply
  (workflow tab) returns `WorkflowHeadlessAskNotice` so the model
  proceeds on its own instead of reading "cancelled".
- `end_turn` records `PendingEndTurn{Summary, Decision}`. Nothing reads
  it: the workflow progress adapter takes the summary from the event
  stream, and `decision` has no effect (loop control is `exit_loop`).
- `finish_workflow` records `PendingFinishData{Description, Artifacts}`;
  final step only.
- `finalized_plan` presents a self-contained markdown plan through
  `Interaction.ConfirmPlan`. The TUI modal (`cmd/ask/finalized_plan.go`)
  offers: execute in the suggested workflow, pick another workflow,
  execute inline, or keep talking. A workflow pick runs
  `env.WorkflowRunner` (the coordinator) and returns its outcome as the
  tool result; inline approval disarms the todos guards.

## Sub-agents (`cmd/ask/agent_tools_task.go`)

`task` runs a research sub-agent on the parent model, or a named
definition from `<available_agents>` (`agent:` param) on its own
provider/model and tool grants. `NewSubagentToolEnv` shares the parent's
file tracker, jobs, and emit, with permissions skipped and the todos
gate off.

## MCP client (`pkg/tools/mcp*.go`)

ask is an MCP client only (go-sdk v1.7.0). `MCPManager` owns one
`mcpServerConn` per server; transports are stdio (`CommandTransport`),
`sse`, and streamable `http` (default), chosen by `EffectiveType`. Every
call goes through `ensure`: ping the session, rebuild it on failure,
retry the call once. `tools/list_changed` refreshes that server's tool
list and calls `onToolsChanged` → `refreshToolset`, which only touches
the deferred registry. Per-server `enabledTools` / `disabledTools`
filter (`MCPToolAllowed`). Image results become media when the model has
vision, else a placeholder line. Elicitation maps the requested schema
to question-modal prompts (`handleElicitation`); URL-mode and headless
requests are declined. The manager also tracks per-server live state
(`Statuses()` → connected · needs-auth · error) and fires
`onStatusChanged` (→ `engine.MCPStatusChangedEvent`); `Reconcile` /
`Detach` add and drop servers live so the Ctrl+S browser can turn a
server on/off without restarting the session.

Server config (`pkg/tools/mcp_servers.go`): `ListMCPServers` is the one
all-sources resolver — enabled plugins (`PluginMCPServers`: a plugin's
`.mcp.json` / `mcps/*.json`, `${CLAUDE_PLUGIN_ROOT}` expanded) ← project-root
`.mcp.json` ← global `mcpServers` ← per-project `mcpServers`, later layers
replacing by name; it returns every server (incl. disabled) with its
`Origin` and effective `Disabled`. `ResolveMCPServers` (the attach path)
filters to enabled, applies `${VAR}` / `${VAR:-default}` expansion, and
drops empties. Enable/disable overrides live in `config.MCPDisabled` at
user scope and `ProjectConfig.MCPDisabled` at project scope (project wins
over user, both over the server's own `disabled`).

OAuth is just-in-time for all http/sse servers (`oauthWanted`: any http/sse
server without header auth, or explicit `oauth: true`). A 401 challenge
triggers the SDK flow (authorization-code + PKCE + dynamic client
registration); startup connects non-interactively so a server that needs
auth surfaces as `needs-auth` (`ErrMCPInteractiveAuthRequired`) instead of
opening a browser. The browser's authorize action runs `AuthorizeMCPServer`
(interactive), passing a `MCPAuthPrompter`: the interactive fetch opens the
local browser best-effort **and** races the loopback callback against a modal
(`modeMCPAuth`) that shows the auth URL, copies it over OSC 52 (works via SSH),
and accepts the pasted redirect URL / code back — so authorization completes
over SSH without port-forwarding (`parseManualAuth` recovers `state` from the
outgoing auth URL for a bare code). `MCPOAuthHandler` persists the token **and** the resolved
client registration + endpoints (v1.7.0 `NewTokenSource` /
`InitialTokenSource`) 0600 under `~/.config/ask/mcp-oauth/`, so refreshes
and restarts stay headless; SSE OAuth rides an `oauthRoundTripper` (the SSE
transport has no native handler). `MCPServerAuthorized` /
`ForgetMCPServerAuth` back the browser's auth indicator and sign-out.

## linear_* twins

`cmd/ask/agent_tools_bridge.go` registers twelve `linear_*` registry
tools over the cores in `cmd/ask/mcp_linear.go` through
`nativeBridgeTool`. They require a configured Linear issue provider and
error otherwise.

## Memory touch points

`load_memory` (query or `id`) and `preload_memory` are core
(`pkg/tools/memory.go`). `preload_memory` is ask's own `MemoryRecallHook`,
an ADK request processor with no declaration: once per invocation it
recalls on the turn's user text and appends a `<memory>` part to a copy
of the turn's user message (never the system instruction), reporting the
inferred topic to the session. The contract tests skip it by name. The
registry set is `tools.MemoryTools`: `memory_index` (approval-gated),
`memory_reinforce`, `memory_demote`, `memory_forget` (approval-gated).
`WrapFileToolsWithMemory` decorates `read` / `edit` / `write` so a result
carries a silent recall for the touched path. All of it is a no-op when
`memory.IsOpen()` is false. Detail in `pkg/memory/CLAUDE.md`.

## Runtime notes (`cmd/ask/agent_run.go`)

Tool calls and results are emitted as `toolCallMsg` / `toolResultMsg`
after unwrapping `invoke_tool` and ADK confirmation calls. Loop
detection hashes each step's tool calls; 5 identical signatures inside
a 10-step window (`agentLoopMaxRepeats`, `agentLoopWindow`) stop the
turn. Provider and model construction is `.claude/rules/providers.md`;
the workflow graph is `.claude/rules/workflows.md`.
