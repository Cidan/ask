---
paths:
  - "pkg/watcher/**"
  - "pkg/tools/watcher*.go"
  - "cmd/ask/watcher*.go"
  - "cmd/ask/watchers*.go"
---
# Watchers

A watcher is a script ask runs on a schedule. Exit 0 with nothing on
stdout (whitespace counts as nothing) does nothing; anything printed goes,
whole and unparsed, to one model run with the watcher's instructions. A
non-zero exit or a timeout is a failed check and wakes no model. Deciding
what is worth printing is the script's job — ask never dedupes.
`pkg/watcher` owns definitions, the schedule grammar, shared state, the
lead lock, and the script runner; `cmd/ask` runs the model and draws it.

## Definitions and files (pkg/watcher/def.go, store.go)

- `Def{Project, Name, Description, Schedule, Command, Cwd, TimeoutSecs,
  Instructions, Agent, Workflow, Provider, Model, Paused, CreatedAt}`.
  Exactly one of `Command` (run with the agent shell, `-c`) or a stored
  script (`HasScript`, derived, never serialized; must start with `#!`).
  `Agent` and `Workflow` are exclusive targets; neither means a chat
  session, which requires `Instructions`. Provider/model never apply to a
  workflow target.
- Every watcher belongs to a project: `ProjectOf(Cwd)`, the git root of
  its directory (an ask worktree maps back to its checkout), else the
  directory itself. `Save` sets `Project`; names are unique within a
  project (`ValidName`). A `Ref{Project, Name}` names one; its `ID()`
  (`<project dir>/<name>`) keys the shared state, the hub, and the UI.
- Machine-local, never committed to a repo: whichever window runs
  watchers must see every project's, and a cloned repo must never start
  running scripts on a timer.

| Path | Holds |
|---|---|
| `~/.config/ask/watchers/<project dir>/<name>.json` | the definition |
| `~/.config/ask/watchers/<project dir>/<name>.script` | the stored script (0700) |
| `~/.config/ask/watchers/<project dir>/<name>.state/` | the script's own scratch dir (`$ASK_WATCHER_STATE_DIR`) |
| `~/.config/ask/watchers.state.json` | shared run state (`State`) |
| `~/.config/ask/watchers.lock` | the lead lock |
| `~/.config/ask/watchers.{defs,state}.lock` | flock around store and state writes |

- `<project dir>` is `ProjectDir`: the project's base name and a hash of
  its path, so two projects with one name never share it. `List` skips a
  file that disagrees with where it is stored.
- `Save(old Ref, def, script)` is create (zero `old`) and edit; a rename,
  or a `Cwd` in another project, moves the script, the scratch dir, and
  the state entry, and drops an emptied project dir. `Delete` removes all
  four. `Resolve(project, name)` prefers the asking project's watcher,
  else the one watcher of that name anywhere, and refuses to guess
  between several. Writes are temp-file + rename under the defs lock.
- The script gets `ASK_WATCHER_NAME`, `ASK_WATCHER_STATE_DIR`,
  `ASK_WATCHER_LAST_RUN` (RFC 3339 of the previous check, empty the first
  time), herdr's pane variables stripped. `RunScript` (swappable) runs it
  in its own process group, kills the group on timeout or stop, keeps
  stdout's head and tail up to `MaxOutputBytes`, stderr's tail, and uses
  `WaitDelay` so an orphaned child holding stdout cannot hang a check.

## Schedules (schedule.go)

`every 5m` · `every 10m weekdays 09:00-18:00` (an interval inside days and
hours; windows may cross midnight) · `daily 09:00` · `weekdays
08:30,13:00` · `mon,thu 14:30` · `mon-fri 9am` · `hourly :15,:45`. Times
are local; `String()` is the canonical spelling and round-trips.
`Next(lastEnd, anchor)` is the whole timing contract:

- An interval counts from the **end** of the last check — model run
  included — so checks are never back to back. Never ran: due at
  `anchor` (`CreatedAt`), i.e. a new interval watcher checks at once.
- A time of day is the first slot after the last end (or after creation).
- A due time in the past means run now; missed checks are never made up.

## Shared state and the lead (state.go, lead.go)

- One ask process leads: `TryLead` takes a non-blocking flock held for the
  process's life; the kernel drops it on any death, so there is no stale
  lead. Every window polls once a second (`watcherTickMsg` →
  `watcherPollCmd`, off the UI goroutine, single-flight): followers try
  for the lead and re-read the state file when its stamp changes; the
  leader prunes deleted watchers, refreshes `NextRun`, honors requests,
  and starts what is due.
- `State.Watchers[id]` is a `Status`: `Phase` (idle, script, model,
  approval), `Activity`, `Approval`, `LastCheck`, `LastEnded` (the
  schedule's base), `LastNote` ("no output"), `NextRun`, the last
  `MaxRuns` runs newest first, and requests any window may file —
  `RunRequested`, `StopRequested`, `SeenRun` (only ever the newest run;
  opening an older one changes nothing). A check that printed nothing is
  not a run.
- A new leader resets every phase and marks unfinished runs stopped
  ("the ask window running it closed"). Ctrl+Z (`suspendApp`) and exit
  (`shutdown`) resign: stop live checks, wait up to `resignWait` for them
  to record their end, clear `Leader`, release. A check that outlives the
  lead writes nothing.

## Running a check (cmd/ask/watcher_hub.go)

- `watcherHub` (`globalWatcherHub`) owns the lead, the live checks (one
  per watcher; a live watcher is never started again), and parked
  approvals. A check leaves the live set only after its outcome is
  written, so "not live" always means "settled".
- The model run (`watcherModelRun`, swappable) is a **background agent
  session**: `ProviderSessionArgs{Background: true}` on a synthetic
  negative tab id. Its messages stay on its channel (never
  `agentSendToProgram`), its turns skip memory extraction and deslop, and
  it records its session id on the run as soon as it starts, so the
  transcript is readable live. It works in the watcher's directory like
  a tab there: that project's instructions, rules, and memory in its
  prompt (`engine.ProjectContext`). `Agent` set runs it as that named
  agent (its instructions heading the same prompt, its name,
  `subagentTools`); provider/model: the watcher's, else the agent's, else
  configured. A workflow target goes through `runWorkflowGraph` with
  `workflowGraphOpts{background: true}` (no tracker, no memory ingest),
  every step carrying the project's context the same way.
- Permissions follow the configured Skip All Permissions. Runs work in the
  watcher's directory, never a fresh worktree (a chatty watcher would
  leave a worktree and branch per check).
- Approvals: `app.Update` hands an `approvalRequestMsg` for a watcher tab
  to `parkApproval` instead of a tab. `/watchers` opens it with
  `openApproval` — the modal answers on a relay channel; the first answer
  from any modal reaches the run; `watcherApprovalClosedMsg` closes
  modals whose request is gone. Only the leader holds the reply channel:
  a follower says so. herdr reports the wait (`blockedReason`).
- **Never `Program.Send` from the hub synchronously** — it is called from
  inside `Update` (parking, keys). Every hub → program message goes
  through `postToProgram` (its own goroutine).

## UI

- Sidebar block (`watcher_sidebar.go`): pinned above the Claude usage
  footer, reserved out of the cards, `watchers` header (`· other window`
  on followers), one line per watcher **of the active tab's project** —
  glyph, name, what it is doing — by `watcherHeadlineFor` priority: `⚠`
  approval > `●` running > `✓` unread result > paused > `✗` failing > `⟳`
  next (relative within the hour). Past five it keeps the ones that need
  the user and adds `+N more`. Other projects add one line, `N in other
  projects need you`, only for an approval, a failure, or an unread
  result (`watcherNeedsUser`) — never for running or idle. No watchers
  here and nothing needing the user elsewhere: no block. Clicking it
  opens `/watchers`.
- `/watchers` (Ctrl+G, `screenWatchers`, `watchers_screen.go`): the
  workflows builder's two-pane silhouette with square panes. The list is
  every project's watchers under a heading each, the tab's project first
  (`ordered`; "none yet" when it has none); ↑/↓ move through all of them.
  List keys:
  enter runs (straight into the run waiting on approval), r run now, x
  stop, p pause/resume, n/e hand create/edit to the agent (pre-filled
  input), d delete (confirm). Runs: enter opens one. A run: ↑/↓/pgup/pgdn
  scroll; enter answers its approval while one is pending, else continues
  it as a chat; a answers, x stops. Script output, stderr, errors, then
  the model's tool calls and markdown answers from its session file
  (`watcherTranscriptLoadedMsg`, reloaded as the run moves).
- Continue as chat (`continueWatcherRunMsg` → `app.continueWatcherRun`):
  upserts a virtual session for the run's native session (reused when one
  exists), titled `watcher <name> · <time>`, opens a tab in the watcher's
  directory on the run's provider, and resumes it. Workflow runs cannot
  be continued.

## Tools (pkg/tools/watcher.go)

`watcher_list/get/create/edit/delete/run`, registry tools (TUI
`deferredBase`, engine registry). `create`/`edit` are approval-gated
(they schedule code to run unattended) and verify the directory, agent,
workflow, and provider exist from the watcher's own directory. `create`
defaults `cwd` to the session's project root (`ProjectOf`, so a worktree
session targets the checkout). `list` shows the session's project and
counts the rest (`all: true` lists every project); the others take an
optional `project` (any directory in it) and otherwise `Resolve` the
name. `watcher_run` files a request; the leader starts it.

## Tests

`pkg/watcher` tests cover the grammar and `Next`, the store, state, the
lead, and the buffers — never a real process. `cmd/ask` swaps
`watcher.RunScript` and `watcherModelRun` (`stubScript`, `stubModelRun`),
installs a fresh hub per test (`useWatcherHub`), keeps each test's
watchers in one project (`testWatcherProject`, ids via `wid`), and drives
real background sessions with `windowProvider` + `engine.ModelBuilder`.
