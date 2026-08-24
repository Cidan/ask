# cmd/ask — the TUI

`package main`: one Bubble Tea v2 program. `app` (tabs.go) owns the
tabs; each tab is a `model` (types.go). Everything agent-side lives in
`pkg/`; this package adapts it to Bubble Tea messages and draws it.
Contracts owned elsewhere: providers → `.claude/rules/providers.md`,
tool surface → `.claude/rules/tools.md`, workflow graph →
`.claude/rules/workflows.md`, issues/PR kanban →
`.claude/rules/issues.md`, skills/plugins →
`.claude/rules/skills-plugins.md`.

## Bubble Tea wiring

- `app.Update` handles tab keys and sidebar focus, routes
  tab-addressed messages with `dispatchByTabID` and everything else
  with `broadcast` (each tab filters with `matchesTabID`,
  coordinator.go). `app.View` joins the active body with the sidebar
  (`joinBodySidebar`) and composites the three full-frame overlays —
  model picker, skills browser, and the `/savings` overlay — with
  `drawOverlayCentered` so they cover the sidebar. Every other overlay is
  drawn inside the tab.
- `model.Update` is a value receiver; mutating helpers are pointer
  receivers (Go takes `&m` on the local copy). A deferred `layout()`
  runs after every Update.
- `model.View` builds the body (`viewBody` → session picker or
  `activeScreen().view`) and, only when an overlay is up (slash/path
  box, ask, approval, sudo, config, finalized plan, the confirms),
  draws body + overlay onto a `uv.ScreenBuffer`. Modals are drawn over
  the history, never returned alone.
- Screens (screens.go): `screenAsk`, `screenIssues`, `screenPRs`,
  `screenWorkflows`; handlers are stateless (`screen`), state lives on
  `model` fields. `ActionScreen*` keys run before the screen's
  `updateKey` and are blocked while `modalOpen()`.
- Modes (`viewMode`): input, session picker, ask question, approval,
  config, model picker, finalized plan, sudo password, skills browser.
  `model.Update` dispatches modals first, then `workflowTabHandleKey`,
  then the screen.
- Popups are wide-and-flat. New boxes use `lipgloss.NormalBorder()`
  (square corners; `modelPickerBoxStyle` is the reference). The
  `RoundedBorder` styles still in themes.go predate the move — do not
  add to them.

### Chat viewport

- `m.chat` is `chatView` (chatview.go): offset and dimensions only.
  view.go renders lazily: `viewportContent` wraps the visible entries
  plus `renderAheadEntries` (10) each side through
  `ensureEntryWrapped`; off-screen entries cost `estimateEntryLines`.
  The per-entry cache is `historyEntry.wrapped`/`wrappedFor`; a width
  change re-renders glamour for response/user entries, re-renders
  `histDiff` entries (their solid backgrounds must span the current
  width), and re-wraps prerendered ones. Never render in per-frame paths.
- The source of truth is `m.transcript []transcriptItem` (transcript.go):
  one typed, mode-independent item per user message, assistant block,
  tool call/result, diff, workflow summary, or prerendered banner.
  `m.history` is a *projection* of it: `projectItem`/`projectHistory`
  apply the view modes (quiet hides tool+diff items and shows assistant
  blocks live as separate entries; tool-output full/short/off; diffs
  on/off). Tool activity projects to three grouped kinds — `histToolCall`
  (per-tool header from the call input; see `renderToolCallBlock` /
  `toolPrimaryArg`), `histToolResult` (per-tool body from
  `renderToolResultBlock`: read → line count, bash → `exit N` + output,
  edit/write → suppressed since the diff carries it, generic → clamped),
  and `histDiff` (structured hunks, solid red/green backgrounds rendered
  at width in `renderDiffBlock`, hardcoded to bypass the theme). A tool
  call, its diff, and its result render as one tight block: `model.gapAfter`
  drops the blank separator row between adjacent members of a group. Arrival handlers `pushTranscript` (append + incremental
  project, preserving caches); mode toggles call `refreshHistory`
  (synchronous full re-project, so toggles are retroactive over the
  whole history — nothing is dropped at arrival time). `/resume` and
  cross-provider swap load the transcript and project through the same
  path, so replay can't drift from live. `m.history` is the persistent
  backing store view.go mutates in place; never rebuild it per frame.
- Stick-to-bottom: `layout()` reads `m.chat.AtBottom()` BEFORE
  `SetWidth`/`SetHeight`/`refreshChatTotals`, then `GotoBottom()` only
  if it was true. Reversing this leaves a resize off the bottom.
- `layout()` gives the chat `m.height-1-inputH-extra` rows, `extra =
  pendingBlockHeight + todoBlockHeight + spinnerBlockHeight +
  statusChipHeight`; `viewAskBody` emits exactly those blocks in that
  order (viewport, pending, todos, spinner, chip, input). Change both.
- Frame cache `frameCache{vpFP, vpView, vbFP, vbWithBar}` on `m.fc` is
  keyed by `contentFingerprint()` = `len(history) | width | shell
  output length | screen | issues cursor | loading frame`. A body
  change that touches none of those terms must clear
  `m.lastContentFP` and `fc.vpFP`/`fc.vbFP`. Shell mode appends to one
  entry in place — hence its length in the key.
- The scrollbar is column `m.width-1`; content is laid out at
  `m.width-1`. Mouse on the ask screen: left-drag selects
  (selection.go), right-click copies (`copySelectionAndClear`), drag on
  the scrollbar column scrolls.

### Reactive flow for data-bearing screens

One source of truth on the state struct; `view()` is a pure
projection; handlers mutate state and the next render derives the
view; single-flight guards live on state, never on the view; a rebuilt
view re-derives from the cache. Kanban does not follow this yet —
`kanbanIssueView.columns` mirrors the cache and carry mutates both in
lockstep (`.claude/rules/issues.md`).

## App layer and tabs

- sidebar.go is the only tab presentation (`tabBarHeight` is 0): width
  terminal/5 clamped to [30,48]; 5-row cards — title (`tabTitle`, else
  short cwd) with `▸` on the active card and a badge (`⚠` needs input,
  `✗`/`✓` workflow failed/done, `●` busy), provider/model, context %
  and session spend (`sidebarCost`), activity (workflow step,
  in_progress todo, stream status, idle).
- The list cursor IS `app.active`. `ActionSidebarFocus` (Tab) takes
  focus only when `model.wantsTabKey()` is false; while focused,
  Up/Down/j/k switch tabs immediately, a printable rune bounces focus
  back and types, Esc/Enter/Tab return, `ActionTabClose` closes.
  `ActionTabPrevAlt`/`NextAlt` switch from anywhere; clicking a card
  switches; wheel over the column is absorbed.
- No focus theft: `dispatchByTabID` parks ask/approval/sudo/plan
  requests on the target tab; a dead tab gets a cancelled/denied reply.
- `openTab` reloads config from disk (a `/config` change applies to the
  next Ctrl+T). `focusTab`/`closeTab` chdir to the tab's cwd and clear
  its frame cache. Closing the last tab prints `last session: <vsID>`
  inline (`quitting`); Ctrl+Z suspends the same way (`suspending`).
- Workflows supplant the origin tab (`app.supplantWorkflow`): the
  provider/session state goes into `workflowTabSnapshot`, the live
  session is killed, `workflowRun` is set, `skipAllPermissions` forced
  on. A busy tab defers through `pendingWorkflow`; a tab already
  running one, or streaming a shell command, refuses with a toast.
  `workflowTabHandleKey`: `ActionTabClose`/Enter after the run ends
  restore the chat (`restoreSupplantedTab`), scroll keys scroll,
  everything else reaches `updateInput` so typed text steers the
  running step. `askToolRequestMsg` on a workflow tab is answered
  `askReply{headless: true}`; approvals auto-deny; closing a running
  workflow tab marks the run failed.

## Provider adapter and session

- `agentAPIProvider` (agent_provider.go) is the one TUI `Provider`
  (provider.go) over every `providers.Provider`. `StartSession` builds
  the model through `engine.ModelBuilder` (a provider that cannot run
  fails here with its own message), creates an `agentSession`
  (agent_run.go), assembles tools (`setupAgentSessionTools`), attaches
  MCP, starts `session.run`. `providerProc.payload` is the session;
  `stdin.Close()` tears it down (`killProc`); `Interrupt` cancels the
  turn context.
- `Coordinator` (`globalCoordinator`, coordinator.go) owns sessions by
  tab: `Dispatch` starts one or queues a turn (`queueMidTurn` while
  busy), `Cancel`, `Kill`, `RunWorkflow` → `runWorkflowGraph`
  (workflow_graph.go). `injectTabID` stamps every outgoing message. On a
  first start `Dispatch` runs `prepareProviderSession` (proc.go) before
  `StartSession` — this is the one place the worktree is created and
  `args.Cwd` repointed into it (a no-op when worktree mode is off or the
  cwd is not a repo); it then emits `providerCwdMsg` so the tab sets
  `m.worktreeName` and shows the `[🌳 name]` chip. `WorktreeName` on
  `ProviderSessionArgs` carries the tab's chosen worktree so a re-dispatch
  reuses it. This is provider-agnostic — never move it into a provider's
  `StartSession`.
- The protocol every tab depends on (types.go; `emit` tags `proc` and
  tab): `streamStatusMsg`, `assistantTextMsg`,
  `toolCallMsg`/`toolResultMsg`, `toolDiffMsg`, `todoUpdatedMsg`,
  `usageMsg`/`costMsg`, `providerModelMsg`,
  `subagentStartedMsg`/`subagentEndedMsg`,
  `bgTaskStartedMsg`/`bgTaskEndedMsg`, `queuedMessageDrainedMsg`, then
  `providerDoneMsg` BEFORE `turnCompleteMsg` (workflow_graph.go waits
  on that order), and `providerExitedMsg` + channel close on shutdown.
- `EngineEventToTeaMsg` (event_adapter.go) turns `engine.EngineEvent`s
  into those messages; `TUIInteractionHandler` (interaction_adapter.go)
  turns ask question, approval, finalized plan, and sudo password into
  `*RequestMsg`s with reply channels (shapes in ask_wire.go). Those
  two, via the swappable `agentSendToProgram`, are the only bridges
  from tool goroutines into the tea loop.
- agent_run.go `runTurn` builds an `llmagent` (`InstructionProvider`,
  core tools, MCP and skill toolsets) or uses `workflowAgent`, runs it
  with `engine.RunnerBuilder` over the file session service, and
  translates ADK events (registry calls unwrapped with
  `unwrapInvokeToolCall`). Loop detection: `agentLoopWindow` 10,
  `agentLoopMaxRepeats` 5.
- agent_session.go `agentSessionStore` wraps the engine session store:
  `~/.config/ask/agent-sessions/<provider>/`, `list`, `loadTranscript`
  (replay events into the faithful, mode-independent `[]transcriptItem`
  that the tab projects), `materialize` (seed a transcript from
  `NeutralTurn`s on a cross-provider swap). See transcript.go for the
  transcript/projection model. virtual_session.go: a
  `VirtualSession` (`~/.config/ask/sessions.json`) is one conversation
  mapped to per-provider native ids; `/resume` lists them per workspace,
  `recordVirtualSession` upserts after a turn, `Title` is the tab
  title. proc.go: `sessionArgs` builds `ProviderSessionArgs`;
  `prepareProviderSessionAt` resolves the worktree cwd and runs
  `validateExecutorCwd`.

## Keys, input, overlays

- keymap.go defaults: Ctrl+I issues, Ctrl+R PRs, Ctrl+W workflows,
  Ctrl+O ask, Ctrl+M model picker, Ctrl+S skills browser, Ctrl+F run a
  workflow on the chat, Ctrl+X task-list toggle, Ctrl+T/Ctrl+D
  new/close tab, Ctrl+←/→ and Ctrl+↑/↓ tab nav, Tab sidebar focus,
  Ctrl+Z suspend; `ActionReload` ships unbound. `currentKeyMap()` is
  cached and invalidated when `/config → Keybindings`
  (config_keybindings.go) saves a capture. Per-screen keys stay inline.
- Chat input (`updateInput`, update.go): slash popover
  (`filterSlashCmds`, slashdescs.go), path picker on `cd `/`ls `/
  `/add-dir ` (paths.go, commands.go), prompt history, Ctrl+V image
  paste, `!` on an empty prompt enters shell mode. Ctrl+C or Esc while
  busy opens the cancel-turn confirm; Esc on an empty prompt opens the
  close-tab confirm; Ctrl+C/Ctrl+D twice on an empty prompt closes the
  tab. `sendToProvider`, slash dispatch, Ctrl+M and `Init` refuse when
  `validateAskCwd` fails.
- `/config` (config_modal.go): Global Options (Quiet Mode, Cursor
  Blink, Render Diffs, Tool Output, Skip All Permissions, Worktree,
  Gate Todos Before Mutate, Theme, Default Provider, Web Search…, one
  `<Provider>...` row per `providers.All()` entry, Keybindings…) and
  Project Options (config_project.go: GitHub MCP endpoint/PAT, Linear
  endpoint/key/team, Issue provider, Worktree). The project Worktree
  row cycles inherit → on → off and shadows the global flag;
  `config.WorktreeEnabled(cfg, cwd)` is the only resolver, and both
  toggles go through `applyEffectiveWorktree` so the session restarts
  only when the effective value moved. The Global row shows the
  on-disk global value (plus any project shadow), not `m.worktree`.
  `/cd` and shell-mode `cd` re-derive `m.worktree` for the new cwd;
  only `/cd` clears the worktree name. `fieldsPickerState`
  (config_fields.go) is the only field editor: opened with a
  `[]providers.SettingField` plus load/save closures; rows show stored
  / `from $ENV` / default; chrome is `renderLayeredConfigBox`.
- Model picker (model_picker.go, model_picker_view.go,
  model_catalog.go): one `NormalBorder` box, list ~38% / detail pane of
  `ModelMetaFor` facts; recents first, then per-provider natural sort;
  type to search, PgUp/PgDn scroll the detail, Ctrl+R forces a catalog
  refresh, Enter applies to the tab and persists the provider default.
  `providerNeedsAPIKey` drops into the inline key prompt; a custom-id
  row takes any id; `stepTarget` retargets Enter to the workflows
  builder. `modelCatalogRefreshCmd` is the single-flight catalog load.
- Skills browser (skills_browser.go, skills_browser_view.go): same box;
  Tab switches the Installed/Marketplace lens; plain keys type into the
  search; Enter acts on the row; Ctrl+G install, Ctrl+D or Delete
  remove (the one overlay where Ctrl+D is not close-tab), Ctrl+N/Ctrl+E
  hand create/edit to the agent, Ctrl+P publish, Ctrl+U pull, Ctrl+A
  add marketplace, Ctrl+X remove marketplace, Ctrl+R refresh. Ops are
  async cmds → `skillsBrowserOpDoneMsg`; `extensionsChangedMsg` is
  broadcast so every tab re-runs `ProbeInit`.
- Question modal (ask_question.go): `askToolRequestMsg` → `startAsk`;
  pick_one / pick_many / pick_diagram, an "Enter your own" row, a note.
  One at a time — a second request while `modeAskQuestion` is up is
  answered `cancelled: true`. Approval (approval.go): deny / allow /
  always (`permissionRuleFor`); Ctrl+C denies and kills the session.
  Finalized plan (finalized_plan.go): `finalizedPlanRequestMsg` renders
  the plan and offers execute-in-workflow, pick another workflow,
  execute inline, or talk more. Sudo (sudo_password.go): masked
  `textinput`, retried on a wrong password.
- toast.go (`m.toast.show`), themes.go (16 themes in `themeRegistry`;
  `applyTheme` fills every style var), selection.go, list_nav.go
  (↑/↓ and Ctrl+P/N for popovers), tab_title.go (`fallbackTabTitle`
  seeds, `generateTabTitleCmd` refines through the swappable
  `generateTabTitleText`, persisted on the VS), usage.go
  (`stepCostUSD` → `providers.StepCostUSD`; `usageMsg`/`costMsg`/
  `tabTitleMsg` feed `sessionCostUSD`; unpriceable models show no
  cost; the meter resets with the conversation).

## Shell mode (shell.go)

- `startShellCmd` forks `$SHELL -c '<input>\npwd > <tmpfile>'` with
  `Setpgid: true`; stdout/stderr scan into `shellLineMsg`s and
  `nextShellStreamCmd` drains up to 500 queued lines into one
  `shellBatchMsg` per render. Past `shellOutputCap` (100) the scanners
  keep draining so the child never blocks and emit one truncation
  marker through `CompareAndSwap`.
- The done handler reads the tmpfile and `os.Chdir`s when it differs,
  so `cd` persists. `killShellProc` SIGKILLs the process group.
- Never combine `Setpgid` with `Setsid` on one `SysProcAttr`: the
  child's `setpgid(2)` fails with EPERM on a session leader. Output is
  piped, so curses programs are not supported. The slash and path
  popovers are gated on `!m.shellMode`.

## Clipboard and images (clipboard.go, kitty.go)

- Text copy: OSC 52 to `/dev/tty` (tmux passthrough aware) plus a
  binary — `pbcopy` on macOS; `wl-copy`, `xclip`, `xsel` on Linux.
- Image paste: Linux reads `wl-paste --list-types` and the first
  accepted mime (png/jpeg/gif/webp), no X11 path; macOS coerces the
  pasteboard with `osascript`. Raw bytes go to the provider; a PNG
  thumbnail re-encode goes to Kitty.
- Kitty: `isKitty` (TERM kitty/ghostty or `KITTY_WINDOW_ID`);
  `kittyTransmitPNG` writes APC chunks straight to `/dev/tty` so the
  renderer cannot interleave; `kittyPlaceholderRows` emits `U+10EEEE`
  plus row/column diacritics with the image id in the foreground
  color. `kittyDiacritics` is the canonical 297-entry table — do not
  edit it.

## Worktrees and cwd guards (worktree.go)

- Detection, `validateAskCwd`, and `ensureWorktreeGitignore` alias
  pkg/config; lock/prune/create/jj-workspace machinery is TUI-only.
  Worktrees live in `.claude/worktrees/`, named from worktree_words.go
  (`ask-<provider>-<adjective>-<verb>-<noun>`, three lists of exactly
  50).
- `validateAskCwd` refuses to run inside `.claude/worktrees/<name>` or
  any subdirectory of a checkout. `validateExecutorCwd`
  (`prepareProviderSessionAt`) is the last-line guard: with worktree
  mode on at a checkout, `args.Cwd` must be inside `.claude/worktrees/`
  (a resumed session recorded at the project root is honored).

## File map

- App, tabs, layout: main.go, tabs.go, types.go, update.go, view.go,
  transcript.go, chatview.go, screens.go, sidebar.go, keymap.go,
  themes.go, toast.go, selection.go, list_nav.go, util.go, debug.go,
  aliases.go.
- Chat and input: commands.go, paths.go, slashdescs.go, shell.go,
  clipboard.go, kitty.go, kitty_diacritics.go, tool_output.go,
  tab_title.go, usage.go.
- Overlays: ask_question.go, ask_wire.go, approval.go,
  finalized_plan.go, sudo_password.go, config.go, config_modal.go,
  config_fields.go, config_project.go, config_keybindings.go,
  model_picker.go, model_picker_view.go, model_catalog.go,
  skills_browser.go, skills_browser_view.go.
- Provider and session: provider.go, agent_provider.go, agent_run.go,
  agent_session.go, proc.go, proc_stream.go, coordinator.go,
  event_adapter.go, interaction_adapter.go, virtual_session.go,
  worktree.go, worktree_words.go.
- Tool glue: agent_tools.go, agent_tools_task.go, agent_tools_bridge.go.
- Issues and PRs (`.claude/rules/issues.md`): issues.go,
  issue_search.go, issue_provider.go, issue_provider_github.go,
  issue_provider_linear.go, pr_provider_github.go, mcp_linear.go,
  mcp_linear_images.go.
- Workflows (`.claude/rules/workflows.md`): workflows.go,
  workflow_graph.go, workflow_store.go, workflows_screen.go,
  workflows_picker.go, chat_workflow.go.

## Debugging

`ASK_DEBUG=1` appends to `/tmp/ask.log` through `debugLog` /
`debugTrace` (no-ops otherwise). Add a `debugLog` when crossing an
async boundary: paste cmd, MCP handler, provider stream, tool dispatch.
