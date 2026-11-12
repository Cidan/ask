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
`load_memory`, `preload_memory`, `decide`, `render_design`, `task`,
`search_tools`, `invoke_tool`, `web_search`; plus the six `workflow_*`
tools outside a workflow run, and — only during a workflow run —
`end_turn` (the step's log-summary contract) on every step and
`finish_workflow` on the final step (`save_artifact` / `load_artifacts`
on every step, `WorkflowStepTools`). `end_turn` is therefore absent from
ordinary chat turns. `task` exists only in the TUI
(`cmd/ask/agent_tools_task.go`); `pkg/tools.CoreTools` is the same list
without it. The `ask_user_question` and `finalized_plan` tools still
exist in `pkg/tools/ask.go` (and their TUI aliases) but are no longer
wired into any session's toolset.

Deferred registry (`s.deferredBase` + `s.mcp.Tools()`): the `linear_*`
twins, the memory set (`memory_index`, `memory_reinforce`,
`memory_demote`, `memory_forget` — `tools.MemoryTools`), the extension
tools (`skill_*`, `agent_*`,
`marketplace_*`, `plugin_*`, `skill_publish`, `skill_pull`), the watcher
tools (`watcher_list/get/create/edit/delete/run` — `tools.WatcherTools`,
`.claude/rules/watchers.md`), and every MCP tool as
`mcp__<server>__<tool>`. Registry tools are real and
callable but absent from the wire definitions: the model finds them
with `search_tools` (`*`, `prefix*`, or substring; each match carries
name, description, full `input_schema`) and calls them with
`invoke_tool`.

**New tools go in the registry.** Append to `s.deferredBase` (TUI) or
the registry func (engine). A core slot costs context on every call of
every session; the bar is "the agent cannot work without seeing it
unprompted". The documented exceptions are `web_search`, `fetch`,
the `workflow_*` tools (kept on the wire so the
model can surface and run workflows without a `search_tools`
round-trip first), and `render_design` (kept on the wire so the model
reaches for the design-preview canvas while it is making visual
decisions rather than only after a `search_tools` round-trip; flip it to
`s.deferredBase` / the registry func to make it discovery-only), and
`decide` (the user asked for it on the wire, so the model reaches for a
cheap calibrated judgment while triaging instead of reading every item).

## decide (`pkg/tools/decide.go`)

Closed-set questions answered by the configured decision model
(`engine.DeciderBuilder`; contract in `.claude/rules/providers.md`).
Params: `input` (shared text), `files` (read from disk: `text/*`, JSON,
XML become text parts headed `File <path>:`; anything else inline data by
extension, then sniffed MIME; `DecideMaxFileBytes` 50 MB each), `calls`,
`scope`, `items` (each item is its own request after the shared input, up
to `DecideMaxItems` 100), and `questions` (`key`, `type`
`yes_no`|`choice`|`score`, `question`, `yes`/`no`, `options`
`[{key, when}]`, `levels`). Options are a list, not a map, so strict
schema validators accept it. No decision model is a non-error `notice`
(`DecideNoModelNotice`), like `web_search` without a key — and no call
runs. Not approval-gated itself: it sends content to the user's own
configured model provider, as every model call does.

**Inner calls** (`calls: [{tool, args, description}]`, up to
`DecideMaxCalls` 50, `decideConcurrency` 8 at once) gather what the
questions are about without the output entering the model's context.
There is one tool implementation, not a second: each call is looked up in
the session's own tool set (`env.Tools`, a `ToolLookup` —
`SessionTools{Core, Registry}` set by whatever assembles the session:
`setupAgentSessionTools`, `BuildCoreTools` / `BuildSubagentTools`, the
`task` tool's sub-agent env) — the exact wrapped tools the model calls —
prepared by `prepareCall` (shared with `invoke_tool`: description phrase
copied in, required params checked), and run with `engine.RunADKTool`.
So bash still asks for approval, output filters still apply, and the
rules, instruction files, and memory a read brings in are kept: they go
to the decision model with the result. What differs is carried by the
call's context (`engine.InnerCall`, `scope.ForCall().Context(ctx)`):
- the rules wrapper delivers against the decide's own `ruleDelivery`
  (every applicable rule and instruction file, the ones in the session's
  prompt included, once per decide) and never marks anything delivered to
  the session's model;
- `recordRead` skips the read tracker, so an inner read never satisfies
  read-before-edit;
- `queueImage` (read of an image, `render_design`, MCP image results)
  hands images to the decision model, as parts after that call's JSON,
  instead of the session's `ImageSink`.
Refused (`decideRefused`): `decide`, `task`, `invoke_tool`, `todos`,
`write`, `edit`, `job_kill`, the workflow-control and hook tools, and any
non-`mcp__` tool whose name has a changing verb (`create`, `edit`,
`delete`, `run`, `install`, `index`, …). MCP tools are allowed, as they
are through `invoke_tool`. Each call that runs emits `ToolCallEvent` /
`ToolResultEvent` with `Parent` = the decide's call id, which the TUI
nests under the decide.

The decision model sees each call as one JSON object
(`{"call", "tool", "args", "result" | "error"}`) plus its images. `scope`
`all` (default) asks every question once over every call; `each` asks
them of each call's result on its own (one request per call, labelled by
its address), alongside `items` units.

**Records and peeking.** Every decide gets an id (`d1`, `d2`, … —
`env.nextDecisionID`, continuing from a resumed session's records) and
saves an `engine.DecisionRecord` (call id, model, answers, every call's
args and raw result) in session state (`engine.SaveDecision`), which the
model never sees. The reply's `answers` is one line per question in the
order asked (`yes — p(yes) 0.94`, options by probability, `1.12 on 0-2 ·
most likely 1 "label" (0.82)`; per call or item under `[n] label` when
asked separately), then an index of `decision:<id>/<n>` addresses with
each call's size or error; `id` and `model` ride alongside. `read` and
`grep` take those addresses wherever they take a path (`decision:d4` for
every call, `decision:d4/3` for one) — `openReadSource` /
`resolveSearchTarget` feed the same line-range and matching code
(`readSpans`, `matchText`) the files go through.

Each decision call's usage is reported with `env.RecordSpend(SpendDecision,
u)` — an `engine.SpendEvent` the TUI's session env routes to
`recordSpend` (cost meter and usage ledger) and `engine.Run` adds to the
run's spend; the memory recall hook reports its filter call the same way.

**History tools are neither.** `history_summary`, `history_read`, and
`history_search` (`pkg/engine/history_tools.go`) are not in any tool list:
the auto-compactor packs them onto the request itself once its archive
holds a chunk, each bound to that agent's own archive (detail in
`pkg/engine/CLAUDE.md`). The registry would be wrong for them — sub-agents
and workflow steps reach the coder's registry, so a registry copy would
search the parent's archive — and a session that never compacts never
carries them. They first appear on the call that makes the first cut,
which already misses the prompt cache. The elision notice names them.

## Images from tools (ImageSink)

Tool results are text/JSON on every provider — ADK stuffs a tool's
`map[string]any` into `FunctionResponse.Response`, and OpenRouter can
only carry a string in a `role:"tool"` message — so bytes returned from a
tool never reach the model as an image. The only cross-provider path is
an `InlineData` part in a *user* content, which is exactly what pasted
images use. `ImageSink` (`pkg/tools/toolimage.go`) is the per-session
handoff: a tool queues a `PendingImage`, and `ImageInjectionHook` — a
request processor (like `preload_memory`, it implements `ProcessRequest`
and registers no declaration, so it is invisible to the model) — drains
the sink before each model call and appends the images as user image
parts, gated by `env.SupportsImages`. The sink lives on `ToolEnv`
(`env.ImageSink`) so `render_design`, `read` of an image file
(`pkg/tools/read_image.go`, decoded and normalized to PNG), and MCP image
results (`mcp.go` `convertResult`) all feed the one sink. `inject_tool_images`
is present in the core list as that request processor, not as a callable
tool.

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
  `marketplace_add`, `plugin_install`, `skill_publish`, `skill_pull`,
  `watcher_create`, `watcher_edit`.
  `invoke_tool` adds no gate of its own.
- `write` / `edit` on an existing file require a prior `read` in this
  session and refuse when the file changed on disk since
  (`CheckReadBeforeMutate`). `edit` needs a unique `old_string` unless
  `replace_all`; CRLF files are edited as LF and written back as CRLF.
  Both emit `ToolDiffEvent` (`EmitFileDiff`).
- `read` rejects image extensions (`ImageExts`), caps at `MaxReadLines`
  / `MaxReadBytes`; every tool output is cut middle-out at
  `MaxToolOutput` (`TruncateMiddle`).
- `todos` replaces the whole list, allows one `in_progress` item, and
  appends a nudge to every ack telling the model when to call again.
  There is no todos-before-mutate gate and no workflow guard: `write` /
  `edit` never require a prior todos list, and `todos` is applied on the
  first call. Workflows are opt-in — the steering prompt no longer forces
  a `workflow_list` check.

## Batched read and grep

Every tool call resends the whole conversation, so `read` and `grep`
take many targets per call (transcripts showed bash `grep` and `sed -n`
as most of all model calls).

- `read`: `file_path` (+ `offset`/`limit`) for one file, `files` (each a
  `file_path` with any number of `ranges`) for many, and `grep` (a list
  of `GrepSearch`) to search in the same call. One file with at most one
  range and no search renders exactly as a plain read (bare `cat -n`
  lines, `next_offset`). Anything more is a batch (`readBatch`): ranges
  of a file are sorted and merged so no line repeats, the file is
  scanned once, and every range and search comes under its own
  `==> <path> lines a-b of n <==` / `==> grep "<pat>" … <==` heading. A
  file or search that fails reports `error: …` under its heading; only a
  batch where everything failed is a Go error. The whole batch shares
  one `MaxReadBytes` budget: the range that crosses it says where to
  continue, and what follows is listed as not read. Every text file read
  is recorded in the tracker.
- `grep`: `GrepParams` embeds `GrepSearch` (`pattern`, `patterns`,
  `path`, `include`, `literal_text`, `context`). One pattern renders
  bare, as before; several each get a heading, run separately (each
  capped at `MaxSearchHits` matches), and a bad one errors only under its
  heading. `context` (max `MaxGrepContext`) adds the lines around each
  match as `Line N- text` with `--` between groups; ripgrep runs with
  `-C` (`parseRgLine` reads its `NUL` / `:` / `-` format) and the Go
  fallback builds the same groups. The cap keeps the context of kept
  matches and drops a dropped match's leading context.
- A path of the form `decision:<id>[/<n>]` reads or searches a decide's
  stored raw results instead of the file system (see decide above); it is
  not a file, so the wrappers add no rules or memory for it and the read
  tracker records nothing.
- Consumers read the call through `engine.ReadCallFiles` /
  `ReadCallSearchPaths`: the context-aware wrapper (rules per file),
  `MemoryAwareTool` (a recall per file), `AppendTouchedFile`, and the
  TUI header (`toolPrimaryArg`: first file `(+N more)`, patterns joined
  by ` | `).

## bash and jobs

- `RunShell` (swappable) is the exec seam for tests. `RunShellProcess`
  runs the agent shell (`ResolveAgentShell`: bash, zsh, `$SHELL`,
  `/bin/sh`) with `-c` under `Setsid`: the command leads its own session,
  so its process group is the shell's pid and it has no controlling
  terminal — a prompt on `/dev/tty` (ssh, git credentials, pinentry)
  fails at once instead of stopping on ask's terminal. Default timeout
  120s, max 600s; timeout, Esc, and `job_kill` SIGKILL the process group.
- Output is read for at most `shellOutputGrace` (2s) after the shell
  exits or is killed. A process the command started that still holds
  stdout/stderr then (`cmd &` in the group, or one that left it with
  `setsid` / daemonizing) would otherwise hold the call open for its
  whole life: ask closes its read ends, SIGKILLs the group, and appends
  `shellOutputHeldNote` pointing the model at `run_in_background`. The
  result keeps the shell's own exit code. Lines stream through a 64 KiB
  `bufio.Reader`, so a longer line arrives in pieces rather than
  stopping the read.
- `safeShellCommands` (`pkg/tools/bash.go`) is the exact-word /
  prefix allowlist that skips the approval prompt.
- `run_in_background` registers a `Job` in `env.Jobs`; `job_output`
  and `job_kill` (SIGKILL to the group) manage it. `job_output` waits
  (`wait`, or implied by `until` / `timeout`) through `waitForJob` until
  the job exits, its raw output matches the `until` regexp (re-checked
  every `jobUntilPoll` from the start of the last line seen, so a line
  split across chunks still matches), `timeout` passes (`jobWaitTimeout`:
  `JobWaitDefault` 600s, `JobWaitMax` 3600s), or `env.Steering` fires
  because the user wrote to the running turn; `matched` and `note` say
  which. One waiting call replaces a poll loop. `job_ids` names several
  jobs: `waitForJobs` waits for all of them, or the first one ready with
  `first`, and each comes back under its own `==> job-N (…): status <==`
  heading (`reportJob` reads, filters, and delivers each), with `status`
  counting how many exited.
- **The user writing ends every long wait.** `env.Steering` fires when
  input is queued for the running turn. `job_output` returns at once
  with a `note`. A foreground `bash` command that is still running
  `SteerYieldGrace` (2s) later is not killed: it becomes a job
  (`env.Jobs.Add` with the output so far, `finishDetachedShell` reading
  the rest and still killing it at its own timeout, `BgTaskStartedEvent`
  / `BgTaskEndedEvent`), and the call returns that output with `job_id`
  and a `note` telling the model to answer, then `job_output` or
  `job_kill`. A result carrying `job_id` shows no exit code in the TUI
  (`extractExitCode`). Sub-agents get no `Steering`, so their tools never
  hand off.
- Foreground `bash` results carry a `notice` (`bashNotice`) when the
  command reads or searches files through the shell
  (`filters.FileReadCommands`: grep/rg/…, `sed -n`, cat/head/tail/nl on a
  file, not a pipe-fed filter, a `-q`/`-c`/`-l` grep, an in-place sed, a
  `tail -f`, a redirected write, or a heredoc body) or sleeps for
  `bashSleepNudgeSeconds` or more (`filters.LongestSleep`). Each kind is
  nudged on the first three calls of a session, then every tenth
  (`ToolEnv.shouldNudge`), so the reminder corrects a habit without
  filling the context. `BgTaskStartedEvent` / `BgTaskEndedEvent` drive the UI.
  The `task` tool's background mode rides the same manager as an agent
  job (`AddAgent`, labelled `agent <name>: <description>`). A job is
  `Delivered` once the model has its output — `job_output` on a finished
  job, `job_kill`, or `MarkDelivered`; `UndeliveredAgents` lists the
  agent jobs still owed (what a watcher run waits for).
- Output passes through `ApplyBashFilter(command, raw, exitCode)`
  unless `disable_token_savings`; savings are tallied in
  `~/.config/ask/savings.json` (`RecordSavings(base, rawTokens,
  savedTokens)`, keyed by `ExtractBaseCommand` → `LedgerKey`). Every
  non-trivial command is recorded — a zero-saving run is a coverage data
  point, so the overlay reflects modeled commands that had nothing to
  compress, not only the wins — while pagers, text transformers, and tiny
  builtins (cat/grep/head/cd/echo…) are skipped via `IsTrivialCommand` so
  the ledger stays about real build/test/tooling. `LoadSavings` reads it
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
  success-collapse + failures-only, npm-family install noise, `kubectl`
  managedFields-block + deprecation-warning strip) first, then
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
  is the one quote-aware command parser both the registry and the ledger
  key agree on: it tokenizes honoring quotes / `$()` / backticks (so an
  operator inside a string never invents a garbage key) and selects a
  pipeline's *primary* stage — the last filter-claimed or non-pager
  producer, not the trailing `| head` / `| tail` — so `go test ./... |
  tail` both filters and keys as `go test`. Global flags are skipped when
  resolving a subcommand (`subcommandAfter`: `git --no-pager diff` → `git
  diff`, `go -C dir test` → `go test`); `LedgerKey` is the derived ledger
  key. New long-tail commands are a new `Rule` in `rules.go`; complex
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
- Bash tool commands (`agentShellEnv`) and stdio MCP servers start
  without herdr's pane variables (`herdr.WithoutPaneEnv`), so a nested
  agent cannot report over or release ask's status in its herdr pane.
- `sudo` must get `-A` (`ValidateSudoCommand`, `sudo_validate.go`).
  The validator follows sudo's own option grammar (`-A` anywhere among
  the options; `-n`, `-V`, `-K`, help, and a bare `-k` never prompt) and
  looks through leading assignments, wrappers (`env`, `nice`, `timeout`,
  `xargs`, `nohup`, `command`, `exec`, `time`, `stdbuf`, `setsid`,
  `ionice`, `taskset`), `sh -c` / `bash -c` scripts, and `find -exec`.
- Sudo askpass (`sudo_ipc.go`, `sudo_proc*.go`; Linux only, other
  platforms export nothing). The bash tool exports only
  `SUDO_ASKPASS=<running ask image>` (`/proc/<pid>/exe` once a reinstall
  replaced the file) and `ASK_SUDO_SOCKET` (a 0700 temp dir); neither is
  a secret. It registers each command's shell pid with the server
  (`RegisterShell`, with the env's interaction handler and tab) for the
  command's lifetime. Authorization is the process tree, read through
  the swappable `SudoProcInspector`: the peer must be this ask
  executable, its parent a sudo front end of this user (real uid the
  user, effective uid 0), its stdout a pipe that is not one of ask's
  own (so the password cannot flow into tool output), and the sudo must
  descend from a registered shell. A request from the same sudo after a
  delivered password carries `Retry`. A hang-up cancels the prompt's
  context. `cmd/ask/main.go` enters helper mode only when
  `IsAskPassInvocation` holds (argv is `SUDO_ASKPASS` plus the prompt,
  parent is that sudo front end); every other launch with the variable
  set is a normal ask that unsets it. The protocol is one JSON line each
  way. Handlers implementing `engine.SudoPasswordPrompter` receive the
  full request; others get `RequestSudoPassword`.

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

These constructors still exist but are no longer wired into any
session's toolset:

- `ask_user_question` → `Interaction.AskQuestion`; a headless reply
  (workflow tab) returns `WorkflowHeadlessAskNotice` so the model
  proceeds on its own instead of reading "cancelled". Not registered.
- `finalized_plan` presents a self-contained markdown plan through
  `Interaction.ConfirmPlan`. Not registered.

Still wired, workflow-only:

- `end_turn` records `PendingEndTurn{Summary, Decision}`. Nothing reads
  it: the workflow progress adapter takes the summary from the event
  stream, and `decision` has no effect (loop control is `exit_loop`).
- `finish_workflow` records `PendingFinishData{Description, Artifacts}`;
  final step only.

## Sub-agents (`cmd/ask/agent_tools_task.go`)

`task` runs a research sub-agent on the parent model, or a named
definition from `<available_agents>` (`agent:` param) on its own
provider/model and tool grants. A foreground run blocks the call until
the sub-agent returns, on a context of its own that the turn's
cancellation stops (`context.AfterFunc`); when the user writes and it is
still running `tools.SteerYieldGrace` later, it is handed off to an agent
job like a background run (`startJob` / `finishJob`) and the call
returns the job id with `agentTaskYieldNote`. A background run (`run_in_background`) is an agent
job; when it finishes it signals its session (`agentFinished`), and a
chat tab's run loop, between turns only, asks `pendingAgentReports`: once
no undelivered background agent is still running, one turn carries every
report the model never collected (`agentReportsTurn` in `watcher_hub.go`,
which marks them delivered) behind an `agentReportsMsg` notice. Inside a
turn the model collects reports with `job_output`, so a report goes one
way or the other, never both. Watcher runs (their own wait in
`watcher_hub.go`) and workflow runs do not wake this way.
`NewSubagentToolEnv` shares the parent's
file tracker, jobs, and emit, with permissions skipped. Either kind works
in the project like a tab: the project's context in its prompt
(`engine.ProjectContext`; a named agent's own instructions head it via
`RunOptions.AgentInstructions`), and `subagentTools` adds what every
session gets whatever the grant — memory-aware file tools, `load_memory`,
per-turn memory recall, and the image hook for its reads.

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
