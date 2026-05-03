# Repo notes for coding agents

## Documentation

Always look at the bubbletea 2.0 documentation in the main bubbletea readme

* https://github.com/charmbracelet/bubbletea

and in the godoc

* https://pkg.go.dev/charm.land/bubbletea/v2

and when at all possible, try to use bubbles for common widgets such as input, etc

* https://github.com/charmbracelet/bubbles

you must always, at all times, use crush as a reference, which you must checkout using
git to /tmp:

* https://github.com/charmbracelet/crush

as it is the cannonical interface in terms of implementation/bubbletea use, though it does not use Claude in the way we do.

when researching the claude code protocol, ALWAYS look at the python reference which you MUST clone to /tmp:

* https://github.com/anthropics/claude-agent-sdk-python

and the documentation here:

* https://code.claude.com/docs/en/agent-sdk/overview

and when looking at the codex protocol, you must always run the following in a temp dir:

* codex app-server generate-json-schema --out .

to generate the app server protocol used communicate with codex; use this to understand how to work with codex

ALL OF THE ABOVE IS NOT OPTIONAL. YOU MUST ALWAYS USE THE ABOVE REFERENCES.

## General info

`ask` is a Bubble Tea v2 TUI that wraps the `claude` CLI. It spawns
claude in `-p --input-format stream-json --output-format stream-json`
mode, streams JSON events back, and renders markdown, images, and a
custom question modal driven by an embedded MCP server.

## Layout

One `package main`, one file per concern.

| File                   | Purpose                                                                 |
|------------------------|-------------------------------------------------------------------------|
| `main.go`              | Entry point. Starts MCP bridge, builds `initialModel`, runs `tea.Program`. |
| `types.go`             | All type defs, model struct, style vars, slash command registry.        |
| `update.go`            | `Init`, `Update` dispatcher, input and session-picker key handlers.     |
| `view.go`              | `View`, layout math, viewport rendering, markdown cache, scrollbar, modal overlay. |
| `claude.go`            | Subprocess mgmt, stream-json reader, send/queue user messages, `--mcp-config`/`--settings` args. |
| `session.go`           | Session path helpers, history/session loading from `~/.claude/projects/`. |
| `commands.go`          | `cd` / `ls` handlers and `ls` formatting.                               |
| `paths.go`             | Path picker state, tilde expansion, completion.                         |
| `shell.go`             | Shell-mode execution: `$SHELL -c` fork, stdout/stderr pipe streaming, 100-line cap, cwd capture via `pwd > tmpfile`, pgroup SIGKILL on cancel. |
| `worktree.go`          | `inGitCheckout()` (cwd contains `.git`) and `ensureWorktreeGitignore()`. When worktree is enabled, the latter appends `.claude/worktrees/` to `./.gitignore` unless an existing rule already covers it. Both no-op outside a cwd-level git checkout — we do not walk upward. Called at startup when worktree is on in config, on the `/config` → Worktree toggle going true, and guarding the `--worktree` flag in `ensureProc`. Also exports `validateAskCwd(cwd)` — refuses to start an LLM session when ask is inside `.claude/worktrees/<name>` (with a `/resume` hint naming `<name>`) or in any subdirectory of a git/jj checkout. Plain checkout roots and non-checkout dirs pass. The chat-facing gate fires from `sendToProvider`, `handleCommand` slash dispatch, Ctrl+B, and silently from `Init`. `validateExecutorCwd(args, root)` is the executor-level defense in `prepareProviderSessionAt`: when worktree mode is on at a real checkout, `args.Cwd` must point inside `.claude/worktrees/`. |
| `clipboard.go`         | `wl-paste` integration, returns raw bytes + re-encoded PNG.             |
| `kitty.go`             | Kitty graphics protocol: detection, transmit over `/dev/tty`, Unicode placeholder rows. |
| `kitty_diacritics.go`  | The canonical 297-entry Kitty row/column diacritic table.               |
| `ask_question.go`      | Question modal state, rendering, navigation, submit/cancel flow.        |
| `mcp.go`               | MCP server bridge (Streamable HTTP), `ask_user_question` tool schema + handler. |
| `workflows.go`         | Workflow runtime tracker singleton + persistence helpers + status broadcast. |
| `workflows_screen.go`  | Workflows builder screen — list/steps/step editor levels with multi-line prompt textarea. |
| `workflows_picker.go`  | Small centred modal popped on `f` (issues) / `Ctrl+F` (chat) to pick which workflow to run. |
| `workflows_run.go`     | Step runner: prompt assembly, advance-on-turn-complete, finalise on done/failed. |
| `workflow_source.go`   | `workflowSource` tagged union (issue ref vs chat transcript) consumed by picker / runner / banner. |
| `chat_workflow.go`     | `Ctrl+F` dispatcher — snapshots `m.history` into a chat source, gates on busy/empty, opens the picker. |
| `util.go`              | Small helpers (`short`, `humanDuration`, `humanBytes`, `shortCwd`).     |
| `debug.go`             | `ASK_DEBUG=1` → `/tmp/ask.log`.                                         |
| `*_test.go`            | Fast, behavior-only tests. See "Test layout" below.                    |

## Build, verify, install

```
go build ./...
go vet ./...
go test ./...
go install .
```

The installed binary lives at `$(go env GOPATH)/bin/ask`. The test
suite is behavior-only (no UI rendering) and must stay fast — well
under a second end-to-end. TUI-level feature changes must still be
exercised by the user; code alone won't catch layout regressions.

### Test layout

| File                       | Scope                                                             |
|----------------------------|-------------------------------------------------------------------|
| `testhelpers_test.go`      | `fakeProvider`, `initGitRepo`, `isolateHome`, `newTestModel`, etc. |
| `provider_test.go`         | Provider registry + claudeProvider metadata + Send protocol.     |
| `claude_cli_test.go`       | `claudeCLIArgs` / `claudeEnv` flag construction.                 |
| `claude_stream_test.go`    | `readClaudeStream` stream-json → `tea.Msg` translation.          |
| `mcp_test.go`              | MCP bridge conversion + permission/approval wire shapes.         |
| `worktree_test.go`         | `.claude/worktrees/` lifecycle against tmp git repos.            |
| `cwd_guard_test.go`        | `validateAskCwd` / `validateExecutorCwd` plus the entry-path gates (sendToProvider, /resume, Ctrl+B, Init). |
| `session_test.go`          | `~/.claude/projects/` parsing + history loading.                 |
| `config_test.go`           | `loadConfig` / `saveConfig` / ollama validation.                 |
| `update_test.go`           | `model.Update` dispatcher behavior via `fakeProvider`.           |
| `workflows_test.go`        | Workflow tracker (markWorking/markFinal/lookup/clear), schema round-trip, prompt assembly, glyph table. |
| `workflows_screen_test.go` | Workflows builder state machine — add/rename/delete persistence + edit-while-running guard. |
| `workflows_picker_test.go` | Picker open/navigate/Enter dispatches `spawnWorkflowTabMsg`. |
| `workflows_run_test.go`    | Step runner — advance, finalise, fail, idempotent finalise, unknown-provider rejection. |
| `issues_workflow_test.go`  | `f` keybind dispatch on the issues screen — toast / picker / focus-existing-tab. |
| `chat_workflow_test.go`    | `Ctrl+F` chat-source flow — transcript filter, key uniqueness, prompt assembly, dispatcher gates (busy/empty/no-workflows/workflow-tab), end-to-end picker → spawn. |
| `util_test.go` / `paths_test.go` | Pure helpers, path completion, frontmatter parsing.       |

### Testing conventions

- **Every new piece of functionality ships with tests.** This is non-negotiable: when adding a feature, fixing a bug, or refactoring anything in the file table above, add or extend tests in the matching `_test.go` file. A PR that grows the codebase without growing the tests is incomplete.
- Tests must be **behavioral**, not rendering-based. Assert on `model` state, emitted `tea.Msg` values, serialized JSON bytes, file-system state, exec argv — never on styled output strings or view snapshots.
- **No subprocess spawning** except `git` in `worktree_test.go`. Everything else uses the `fakeProvider` from `testhelpers_test.go` or direct function calls.
- Worktree / git tests use `t.TempDir()` + `t.Chdir(...)` so they self-isolate and survive parallel runs.
- HOME-sensitive tests (`session`, `config`, `paths`) call `isolateHome(t)` to pin `$HOME` at a tmp dir so the user's real state is never touched.
- Prefer a few larger scenarios over dozens of trivial one-liners, but do cover each branch of complex functions (see `claudeCLIArgs` and `readClaudeStream` tests for the pattern).
- Keep the full suite under ~1 second — if you add something slow, figure out how to fake it.

## Bubble Tea wiring

- `Update` is a **value receiver** (`func (m model) Update(...) (tea.Model, tea.Cmd)`). Helpers that need to mutate (`layout`, `appendUser`, `killProc`, etc.) are pointer receivers — Go takes `&m` implicitly on the local copy and the returned `m` propagates back to the runtime.
- `View()` composes everything into one string. When an overlay is needed (slash popover, path picker, modal, scrollbar), we draw onto a `uv.ScreenBuffer` and return its rendered content; otherwise we return the plain body.
- The modal is drawn **on top** of the normal body so the user sees the history underneath — do not early-return a modal-only view.

### Stick-to-bottom rule

`layout()` captures `AtBottom()` **before** any `SetWidth` / `SetHeight` / `SetContent`, then calls `GotoBottom()` only if it was true. Reversing the order causes a 1-row resize to flip the viewport off the bottom and never snap back — a real bug we hit and fixed.

### Markdown cache

Glamour rendering is cached per `historyEntry` in `entry.rendered`. `viewportContent()` fills it lazily on first render; `WindowSizeMsg` invalidates every response entry so wrap recomputes at the new width. Don't re-render in `renderEntry` — that path runs on every spinner tick and every keystroke.

### State + view: reactive flow (data-bearing screens)

The issues screen has one user-facing view today — kanban. The flat
list (`listIssueView`) was removed in favour of the column picker
because it reads better at every terminal width and avoids the
table-widget UX foot-guns. The cycle infrastructure
(`issueViewLayers` + `cycleView()`) is preserved for future view
types — adding a per-assignee swimlane or milestone grid is one
new entry in the registry.

General reactive-flow discipline (forward-looking; kanban does NOT
yet follow it — see the bullet at the bottom):

- **One source of truth.** Cache lives on the screen-state struct (`issuesState.pageCache`). Everything else is derived. View structs hold ONLY view-local state: cursor position the widget needs, layout dimensions, widget refs.
- **`View()` is a pure projection.** Every render reads cache + state and projects them onto the bubbles widget. No hidden side state; two consecutive `view(s)` calls with no state mutation MUST return the same body.
- **One-way mutation.** `keypress / msg → handler → state → next render derives the view`. The handler MAY adjust state fields that affect rendering; the view picks them up on the next render. Don't add a "view-local sync" method.
- **Single-flight guards live on state, not view.** Bookkeeping that must survive a view rebuild (Tab cycle, search-box close) belongs on the state struct. Putting it on the view causes double-fires after rebuild — a real bug we already shipped a fix for.
- **View constructors derive their initial state from the cache.** A view that's reconstructed (cycle, Ctrl+R, search-box close) re-stitches its display from the cache so the user lands at the same virtual row.
- **Anti-pattern: dual state.** If your view has `chunks []X` or `rows []Y` fields that mirror the cache, you have two sources of truth. The handler mutates the cache, the view mutates its copy, and they drift. Symptoms in the issue tracker were duplicate-on-re-entry rows, error-Esc duplicates, and the pendingFetch-reset bug. Pick one source — and it should be the cache.
- **Kanban is NOT yet on this discipline.** `kanbanIssueView.columns` still holds per-column `loaded`/`nextCursor`/`hasMore`/`fetching` mirroring the cache, plus its own selection cursor. Carry-and-drop adds another dual-state surface (the `kanbanCarry` struct lives on the view, but `pickupCarry`/`dropCarry`/`cancelCarry` mutate both `column.loaded` AND `s.pageCache` in lockstep via the state-level `removeIssueFromCache` / `insertIssueIntoCache` helpers — keep them paired or columns and cache will drift). Earmarked as a follow-up refactor — don't read this section and assume kanban already follows the rule.

### Issues loader animation

When `s.loading` is true the screen swaps the body for a centered
modal rendered by `renderIssuesOverlay`. The animation runs at
~30fps via `issueLoadingTickMsg` (interval `issueLoadingTickInterval`
= 33ms). Each tick increments `s.loadingFrame` and re-arms itself
with `issueLoadingTickCmd`; the handler stops re-arming when
`s.loading` flips false (first chunk arrival or error dismissal),
so a stale tick on the wire silently no-ops. Three entry paths
dispatch the initial tick alongside the fetch — Ctrl+I screen
entry, search-box submit, and `reloadCurrentQuery`.

The modal is a single line: a braille spinner glyph from
`issueLoadingSpinnerFrames` (10-frame ring, advances once per
tick for the high-FPS "still alive" cue), two spaces, then the
picked fun message (stable for the duration of the load).
`lipgloss.Place(width, height, Center, Center, box)` centers the
whole modal on screen.

### Kanban carry-and-drop

`Space` on a focused card enters carry mode: the issue is ripped
out of its origin column (both `column.loaded` AND the matching
cached chunk via `s.removeIssueFromCache`) and stashed on
`kanbanIssueView.carry`. Carry is a view-local affordance — it
lives on the kanban view because every other column-mutation
field already does, and bundling it onto the view prevents stale
carry state from outliving a screen-leave by accident. While
carrying, `←`/`→`/`Tab` cycle the focused column with the carry
following (rendered with a `warn`-background style pinned at row
0 above the destination column's loaded[] slice — no slot is
consumed from the column's data). `j`/`k`/`Up`/`Down`/`g`/`G`/`Enter`
are absorbed silently — the carry is the focus, rows underneath
don't matter. `Space` drops; `Esc` cancels.

The drop is **optimistic with rollback**: same-column drops are
short-circuited to `cancelCarry` (no provider call); cross-column
drops update the cache + `column.loaded` immediately and dispatch
`provider.MoveIssue` via a `tea.Cmd` whose context is *independent*
of `s.loadCtx` (`Ctrl+R` must not cancel an in-flight backend
mutation). The cmd resolves with an `issueMoveDoneMsg`; the
`update.go` handler is a silent no-op on success and a defensive
rollback on failure — it locates the issue *by number* in the
target column (not by blind index, so an intervening reload
between drop and rollback degrades to a no-op rather than
corrupting fresh state) and re-inserts at the recorded
`originRowIdx` before emitting a toast through `m.toast.show`.

Carry cancellation is wired into every screen-leave path:
`Ctrl+R`, `/` (search-box open), and `Ctrl+O` all call
`kv.cancelCarry(s)` *before* their normal effect, so the rolled-back
issue is back in the cache before `discardOnLeave` /
`reloadCurrentQuery` wipe it. Closing a tab (`Ctrl+D`, double
`Ctrl+C`) drops the entire issues state along with the tab —
no extra teardown needed, the in-memory carry evaporates with it.

The provider interface gained two methods to support this:
`MoveIssue(ctx, cfg, cwd, it, target)` (which github translates to
`issue_write` with `state` + `state_reason` per the four canonical
columns) and `KanbanIssueStatus(target)` (which keeps the kanban
view provider-agnostic by letting providers report the issue.status
string a card placed in target should carry). Both methods have
no-op implementations on `noneIssueProvider` and behavioral test
hooks on `fakeIssueProvider`.

## Workflows (issue → agent pipelines)

Two entry paths spawn a workflow run:

- `f` on a focused issue (kanban card or detail view) runs a
  pipeline against that issue.
- `Ctrl+F` on a chat tab runs a pipeline against the current
  conversation — the user/assistant turns from `m.history` are
  filtered (tool calls, shell output, system entries dropped) and
  appended to step 1's prompt under a `Reference (chat
  transcript):` block.

Either path opens the same picker (`workflows_picker.go`) and
spawns a workflow tab. Pipelines are per-project and built
through a dedicated screen (`Ctrl+W` or `/workflows`); each step
pins its own provider (`claude` / `codex` / …) + model + prompt,
so a single workflow can chain `claude → codex → claude` if the
user wants. There's no default — an empty workflow list is a
toast at trigger time pointing the user at the builder.

### Workflow source abstraction

The picker, the spawned tab, and the runner accept a
`workflowSource` value (defined in `workflow_source.go`). It is
a tagged union: `Kind == workflowSourceIssue` carries an
`issueRef`; `Kind == workflowSourceChat` carries a filtered
`[]chatTurn` plus a label and a unique key. The accessors
`Key() / Display() / RefBlock()` give the runtime everything it
needs without branching on the kind. Adding a third entry path
(PR review against a draft? scheduled run against a saved query?)
is one new constant + a switch arm per accessor.

### Schema

`projectConfig.Workflows` lives alongside `projectConfig.Issues` in
`~/.config/ask/ask.json`. The shape is intentionally generic — the
runtime takes a `workflowSource` for the prompt reference (issue or
chat snapshot), but nothing about the broader pipeline machinery is
bound to issues. Future surfaces (PRs, scheduled tasks, …) plug into
the same builder / runner.

| Type | Purpose |
|------|---------|
| `workflowsConfig{Items, Sessions}` | Per-project block. |
| `workflowDef{Name, Steps}` | One named pipeline. |
| `workflowStep{Name, Provider, Model, Prompt}` | One stage (a fresh subprocess at run time). |
| `workflowSession{Workflow, StepIndex, Status, StartedAt, UpdatedAt}` | Disk-persisted run record — terminal statuses only. |

### Runtime tracker

`workflowTracker()` is a process-wide singleton (`workflows.go`).
The in-memory map keys on `workflowSource.Key()` —
`<provider>:<owner/repo>#<n>` for issue sources,
`chat:<tabID>:<unix-nanos>` for chat sources (the timestamp suffix
makes two consecutive Ctrl+F runs from the same tab distinct so
they don't stomp each other). Only `done` / `failed` ever land on
disk; `working` is process-local because pipeline runs aren't
resumable across restarts (one-shots, no provider session pinning).
Three transitions matter:

- `markWorking(cwd, key, workflow, tabID)` — drops any stale disk
  record for `key`, broadcasts `workflowStatusChangedMsg` so the
  kanban repaints with the in-flight icon.
- `markStep(key, idx)` — bumps the step counter mid-run; the banner
  re-reads on next render.
- `markFinal(cwd, key, workflow, status, stepIdx)` — preserves
  StartedAt across the working→terminal transition, persists
  `done`/`failed` to disk, broadcasts.

Live UI updates flow through `broadcastWorkflowStatus` →
`teaProgramPtr.Send(workflowStatusChangedMsg)` → `app.broadcast` →
every tab's `model.Update` invalidates its frame cache. The kanban
renderer reads from the tracker on every render (cache-warm after
first hit), so no per-tab state mutation is needed for repaint.

### Read-only workflow tabs

A workflow tab is a regular `model` with a non-nil `workflowRun`
field. View consequences:

- `viewAskBody` swaps `m.input.View()` for `renderWorkflowBanner()`
  — a 3-line bordered status box showing the current step
  (`▸ workflow "<name>" · step 2/3: review (codex/gpt-5)`),
  completion (`✓ workflow complete`), or failure (`✗ workflow
  failed`).
- `model.Update`'s key dispatch routes through `workflowTabHandleKey`
  before the screen handler runs: only `Ctrl+D` (close), `Ctrl+C`
  (cancel = mark failed), and viewport scroll keys (Up/Down/PgUp/
  PgDn/g/G/j/k/Home/End) are honoured. Everything else is absorbed.
- `askToolRequestMsg` and `approvalRequestMsg` arriving on a
  workflow tab auto-cancel/auto-deny so the chain doesn't stall on
  a modal that has no human to dismiss it. Workflow tabs run with
  `skipAllPermissions = true` regardless of the global toggle.
- `closeTab` on a still-running workflow tab marks the run as
  failed before tearing down — the user closing the tab is the
  verdict, no graceful drain.

### Step runner

`workflows_run.go` is the chain driver. Each step is a fresh
subprocess (one-shot — the chain doesn't share a provider session
across steps; that's why workflow tabs don't pin a virtualSessionID
and why `providerDoneMsg.SessionID` is suppressed on workflow
tabs). The runner consumes the existing `sendToProvider` machinery
unchanged — it just sets `m.provider` / `m.providerModel` / clears
session state before the call.

Step transitions are signalled through three existing message
handlers, hooked at the **end** of their existing logic so the
runner doesn't need to know about provider-specific stream shapes:

- `turnCompleteMsg` (clean turn end) → `workflowAdvanceCmd(tabID, nil)`
- `providerDoneMsg` with `err != nil` or `IsError == true` →
  `workflowAdvanceCmd(tabID, errStepError(...))`
- `providerExitedMsg` with non-nil err on a still-running run →
  `workflowAdvanceCmd(tabID, errStepError(stderrTail))`

The advance handler kills the proc, rolls the captured per-step
text into `stepLog`, and either dispatches `workflowRunStartStepMsg`
for the next step or finalises (`done` on chain end, `failed` on
error).

### Step prompt assembly

`buildWorkflowStepPrompt(step, issue, log)` produces the full user
turn for step N:

```
<step.Prompt>

Reference: <owner/repo#N>

Previous step output:        (only when log is non-empty)
<log[0]>
---
<log[1]>
...
```

Reference format is `<project>#<number>` (no provider prefix, no
URL); the agent has the issue-tracker MCP wired in and resolves
the rest itself. Whitespace is trimmed at the head and tail; the
body stays as the user wrote it.

### Builder screen (`Ctrl+W` / `/workflows`)

`workflows_screen.go` is a top-level screen (`screenWorkflows`,
peer of ask/issues). State lives on `m.workflowsBuilder` and uses
the standard `renderLayeredConfigBox` chrome for visual parity
with `/config`. Three navigation levels:

| Level | Cursor over | Keys |
|-------|------------|------|
| List (workflowsLevelList) | workflows + "+ New" | enter open / r rename / d delete / esc back to ask |
| Steps (workflowsLevelSteps) | steps + "+ New step" | enter edit / r rename workflow / d delete step / esc list |
| Step (workflowsLevelStep) | Name / Provider / Model / Prompt | enter edits the field / esc back to steps |

The Prompt row opens a multi-line `textarea.Model` overlay
(`workflows_screen.go:newPromptTextarea`) with Ctrl+S to save and
Esc to cancel. Provider/Model rows pop tiny pickers populated from
`providerRegistry` / `modelPickerOptions(...)`. Every commit writes
to disk immediately, so navigating up/back is never a save action.

### Edit guards

A workflow that's currently running anywhere in the process
(`workflowTracker().activeWorkflowNames()`) is locked against
rename / delete / step edits — the builder shows a dim
"blocked: workflow is running" toast in the help row. Once the
run finalises (or the tab closes), the lock releases.

## Claude subprocess

- Always `-p --input-format stream-json --output-format stream-json --verbose --dangerously-skip-permissions`.
- Pass `--resume <id>` only when `m.sessionID != ""`.
- Always pass `--mcp-config` (HTTP URL to our bridge) and `--settings` (the `AskUserQuestion` redirect hook) when `m.mcpPort > 0`.
- `readClaudeStream` scans stdout and emits `streamStatusMsg`, `claudeDoneMsg`, and a final `claudeExitedMsg`. Stderr is captured into a ring buffer and surfaced on exit error.

### User message shape

Plain text → `content: string`. With attachments → `content: []block` using the Anthropic Messages API shape:

```jsonc
{"type": "text", "text": "…"}
{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "…"}}
```

`userContent` in `claude.go` builds this.

## Clipboard and thumbnails

- Only Wayland is supported. Don't add X11 / macOS fallbacks without asking.
- `wl-paste --list-types` picks the first `image/{png,jpeg,gif,webp}` entry; the raw bytes go straight to Claude (whatever mime), a PNG re-encode goes to Kitty.
- Kitty transmit writes APC sequences **directly to `/dev/tty`**, not stdout, so Bubble Tea's renderer can't interleave with the image upload.
- Placeholders are emitted inside `View()` via `kittyPlaceholderRows(id, cols, rows)`. Rows of `U+10EEEE` + diacritics encode `(row, col)` and the foreground color encodes the low 24 bits of the image ID.
- `kitty_diacritics.go` is the canonical Kitty lookup table — do not edit entries; if you need more than 297 indices, you've misdesigned the grid.

## Shell mode

- **Activation**: `updateInput` intercepts `msg.Text == "!"` on an empty prompt (not busy, no pending attachments) and flips `m.shellMode`. Subsequent keys route through `updateShellInput` until exit (Esc, Ctrl+C, or two backspaces on empty). On Enter the command is recorded into `m.shellHistory` (separate from `m.inputHistory`), the user text is rendered as a userBar entry, and `startShellCmd` dispatches.
- **Output pipeline**: `startShellCmd` forks `$SHELL -c '<input>\npwd > <tmpfile>'` with `Setpgid: true`. Two goroutines scan stdout and stderr into a channel as `shellLineMsg`; `nextShellStreamCmd` blocks on the first message then non-blockingly drains up to 500 more (and a trailing `shellDoneMsg`) into a single `shellBatchMsg` so large outputs render in chunks, not line-by-line.
- **100-line cap**: the two stream goroutines share a `shellStreamState` with an atomic counter and `marked` bool. Past the cap they stop forwarding lines and emit a one-shot `… output truncated at 100 lines` marker via `CompareAndSwap`. The pipe is kept draining so the child doesn't block on a full kernel buffer.
- **Cwd persistence**: the `pwd > tmpfile` suffix runs after the user's command (newline-separated — works in bash/zsh/fish). The done handler reads the tmpfile, `os.Chdir`s if it differs from the current cwd, then calls `refreshPrompt` and `refreshPathMatches`. Temp file is removed on both success and error paths.
- **Cancel**: `killShellProc` does `Kill(-pgid, SIGKILL)` so children (`sleep 100`, etc.) die with the wrapper. Do NOT combine `Setpgid: true` with `Setsid: true` on the same `SysProcAttr`: the child's `setpgid(2)` returns EPERM when called on a session leader, so exec fails with `operation not permitted`. This is the trap creack/pty falls into if you try to add PTY support naively.
- **Popups**: `View()`'s popup gate is `m.mode == modeInput && !m.busy && !m.shellMode`, so the path picker (from `cd `/`ls ` prefix) and slash popover both stay hidden in shell mode even though the input text might still prefix-match.
- **Curses apps are not supported** — output flows through pipes, so altscreen sequences from vim/htop/less render as raw text in history. Rollback artifact: there was a PTY-based path; removed because `Setpgid + Setsid` collision made non-curses commands fail with EPERM.

## MCP server

- `newMCPBridge()` binds `127.0.0.1:0`, stores the port, builds the `mcp.Server`, registers `ask_user_question`, then returns.
- `start(p *tea.Program)` is called after the program is constructed so the handler can call `p.Send(...)`. Uses `atomic.Pointer[tea.Program]` so the goroutine can read it safely.
- Tool handler packs input questions into the internal `question` type, `p.Send`s an `askToolRequestMsg` with a reply channel, then blocks on the channel.
- `submitAsk` / the Esc cancel path write to `m.askReply` if present; the `/qq` mock path (reply == nil) prints a summary to history instead.

## Conventions

- No new runtime dependencies without asking. We already carry Charm (bubbletea/bubbles/lipgloss/glamour/ultraviolet), the official MCP SDK, and stdlib.
- Only emojis that already exist in the codebase (`✓`, `✗`, `▸`, `›`, `▏`) — nothing new unless the user asks.
- Comments: default to none. Only add one when a reader cannot derive the reason from the code.
- Debug logging uses `debugLog(format, args...)` and is a no-op unless `ASK_DEBUG=1`. Add one when crossing an async boundary (paste command, MCP handler, claude stream, tool dispatch).

## Known-fragile areas

- `layout()` extra-row math: any change to what appears between viewport and input (chip, thumbnail strip, spacer row) needs the `extra` term in `layout()` and the emission order in `viewBody()` kept in sync.
- Scrollbar column is drawn over `m.width-1`. If any text-rendering style grows a margin or a user-bar width past `m.width-1`, the scrollbar will be overwritten or vice-versa.
- `askToolRequestMsg` is rejected if the modal is already open — only one MCP ask at a time. Double-calls from Claude return `cancelled: true` for the second one.
- `contentFingerprint` must mix in `len(m.history[m.shellOutIdx].text)` whenever a shell output entry is active. The frame cache is keyed on `len(m.history) | m.width`, and shell mode appends streamed lines in place to a single history entry, so without that extra term the cache returns a stale (first-line-only) view until something else (spinner row, window resize) perturbs the key.
