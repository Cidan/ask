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

when working on the agent runtime, ALWAYS read the fantasy source, which you must checkout to /tmp:

* https://github.com/charmbracelet/fantasy

when working on MCP (client transports, OAuth, elicitation), ALWAYS read the official Go SDK source at the pinned version:

* https://github.com/modelcontextprotocol/go-sdk

and for skills/subagent file formats, the Agent Skills standard:

* https://agentskills.io

ALL OF THE ABOVE IS NOT OPTIONAL. YOU MUST ALWAYS USE THE ABOVE REFERENCES.

## General info

`ask` is a Bubble Tea v2 TUI coding agent. The agent loop runs
in-process on `charm.land/fantasy` against provider APIs (anthropic,
openai, deepseek) — there are NO CLI subprocesses and NO loopback MCP
server; every tool (coding core, linear/workflow bridge twins,
question modal, external MCP clients) is native Go. ask renders
markdown, images, and a custom question modal.

## Layout

One `package main`, one file per concern.

| File                   | Purpose                                                                 |
|------------------------|-------------------------------------------------------------------------|
| `main.go`              | Entry point. Builds `initialModel`, runs `tea.Program`, owns CLI arg parsing (`ask resume <vid>`). |
| `types.go`             | All type defs, model struct, style vars, slash command registry.        |
| `update.go`            | `Init`, `Update` dispatcher, input and session-picker key handlers.     |
| `view.go`              | `View`, layout math, viewport rendering, markdown cache, scrollbar, modal overlay. |
| `agent_provider.go`    | `agentProviderSpec` + the generic `agentAPIProvider` — ONE Provider implementation shared by every in-process API provider (deepseek/anthropic/openai): StartSession → `agentSession`, capability-gated image attachments, shared tool assembly (`agentSessionTools`), per-spec settings accessors. See "In-process API providers" below. |
| `deepseek.go`          | DeepSeek spec: model/effort options, `$DEEPSEEK_API_KEY` resolution, effort→wire mapping (thinking off / reasoning_effort), images rejected. |
| `anthropic.go`         | Anthropic spec: catwalk model list, effort→`output_config.effort` with catalog clamping, manual prompt-cache breakpoints (`anthropicPrepareStep`: system + last 2 messages; `anthropicDecorateTools`: last tool) — without these the API bills uncached every turn. |
| `openai.go`            | OpenAI spec: Responses API (prefix predicate `openaiUseResponsesAPI` so new gpt-5.x/codex ids never fall back to chat completions), reasoning summaries + encrypted reasoning content (stateless resume), effort clamping. |
| `catalog.go`           | catwalk embedded-catalog lookups: model ids (default first), context windows, image capability, effort-level clamping. No network — the snapshot ships with the module. |
| `agent_run.go`         | The in-process agent runtime: session goroutine, fantasy agent loop ↔ provider-protocol msgs, per-spec PrepareStep, image file parts, interrupt, loop detection, auto-compaction, dangling-tool-call repair. |
| `agent_prompt.go`      | Coder system prompt assembly: static head, env snapshot, CLAUDE.md/AGENTS.md inclusion, `askSteeringPrompt` tail. Byte-stable per session for DeepSeek's prefix cache. |
| `agent_session.go`     | `agentSessionStore` — fantasy-message transcripts under `~/.config/ask/agent-sessions/<provider>/`, backing /resume, LoadHistory replay, and Materialize. |
| `agent_diff.go`        | Pure-Go Myers unified diff (`unifiedDiff`) whose output round-trips `parseUnifiedDiff` so agent edits render through the existing diff pipeline. |
| `agent_tools.go`       | Shared tool infra: `agentToolEnv` (cwd, emit, approval gate), read-tracker, output caps/truncation. |
| `agent_tools_file.go`  | read / write / edit tools (exact-match edits, read-before-mutate + stale-mtime guards, CRLF preserve, diff emission). |
| `agent_tools_search.go`| glob (doublestar + `{a,b}`), grep (rg with pure-Go fallback), ls (capped tree). |
| `agent_tools_bash.go`  | bash (+`run_in_background`), job manager, job_output / job_kill, safe read-only command list. Exec layer behind `agentRunShell` for tests. |
| `agent_tools_web.go`   | fetch — HTTP GET with 100KB cap, HTML→text extraction via x/net/html. |
| `agent_tools_todos.go` | todos tool — full-list replace emitting `todoUpdatedMsg` into the existing todo surface. The description carries an explicit cadence contract (one call per status transition) and every ack appends a state-keyed nudge ("call again the moment the in_progress item is done" / "no item is in_progress") so models keep the list live instead of planning once and closing everything at the end. |
| `agent_tools_task.go`  | task tool v2: default read-only researcher on the parent model, OR a named subagent definition (`agent:` param) with its own instructions/tools/model — including a DIFFERENT in-process provider (cross-provider delegation). `run_in_background` rides the bash job manager (job_output/job_kill + bgTask UI signals). |
| `agent_subagents.go`   | Named subagent defs: `.claude/agents/*.md` + `~/.config/ask/agents` (frontmatter name/description/tools/model + ask's `provider` extension; body = system prompt), `<available_agents>` prompt block, tool grant sets (default read-only, `*` = coding core, never task/modal tools), claude model-alias mapping, cross-provider model resolution via `agentSpecByID`. |
| `skills.go`            | Agent Skills standard (agentskills.io): SKILL.md discovery (~/.config/ask/skills, ~/.agents/skills, ~/.claude/skills + project .agents/.claude/.ask skills dirs at cwd AND git root, project wins), name/description validation, `<available_skills>` trigger block (progressive disclosure — body loads via the read tool), `/skill-name` slash expansion (`expandSkillInvocation` in runTurn), user-invocable skills surfaced through `ProbeInit`. Generic `parseMarkdownFrontmatter` shared with subagent defs. |
| `agent_tools_ask.go`   | In-process twins of the bridge's `ask_user_question` / `end_turn` — same modal/workflow machinery, no HTTP loopback. |
| `agent_tools_bridge.go`| Native twins of every other bridge tool (12 `linear_*`, 7 `workflow_*` incl. `workflow_copy`): a generic `nativeBridgeTool` adapter generates fantasy schemas via the same jsonschema machinery the MCP SDK uses (field docs survive verbatim) and wraps the shared cwd-parameterized cores in mcp_linear.go/mcp_workflows.go. In-process sessions never attach the loopback bridge. |
| `agent_memory.go`      | Memory recall injection: session-start recall appended to the system prompt (once, byte-stable), per-prompt recall appended to the wire prompt, and `memoryAwareTool` wrapping read/edit/write with a per-file recall footer. All no-op when the memory service is closed. |
| `agent_tools_mcp.go`   | MCP client v2 (`mcpManager`/`mcpServerConn`): per-session manager over stdio/http/sse transports (official go-sdk v1.6.1), lazy ping-and-rebuild before every call + one renew-and-retry, `tools/list_changed` → live toolset refresh, MCP elicitation → ask's question modal (form mode: enum/boolean/free-form, typed answers; URL mode + headless decline), image tool-results as real media when the model has vision. Tools are `mcp__<server>__<tool>`. |
| `mcp_servers.go`       | User-facing MCP server config: `mcpServers` maps (user-global + per-project) merged over project-root `.mcp.json` (claude-code convention), `${VAR}`/`${VAR:-default}` expansion, per-server type inference, timeout, enabled/disabled tool filters, Disabled tombstones. |
| `mcp_oauth.go`         | OAuth for remote MCP servers (`oauth: true`): SDK authorization-code + PKCE + dynamic client registration, browser launch via swappable `mcpOAuthOpenBrowser`, one-shot loopback callback listener, tokens persisted 0600 under `~/.config/ask/mcp-oauth/` (valid stored tokens skip the browser; expiry re-runs the flow). |
| `model_picker.go`      | Ctrl+M unified model picker (crush-style): search input, “Recently used” group first (`cfg.RecentModels`, capped push-front), then one section per provider with human-friendly catwalk names; ↑/↓ skip headers, Enter applies the pick to the current tab and persists it as the provider default model (`applyProviderModelSwitch` + `applyVSProviderSwap`; cfg.Provider untouched). Picking a model whose provider has no key drops into an inline API-key prompt (`providerKeySpecs`) that saves to ask.json and proceeds; per-provider “Enter your own…” rows cover custom ids. Replaced the old Ctrl+B switcher AND the `/config` per-provider key sub-pickers AND the `/model` and `/provider` slash commands. |
| `sidebar.go`           | Sidebar tab mode (cfg.UI.TabMode == "sidebar"): right-hand column of per-tab task cards (title / provider·model / session $ spend / live activity + ⚠✓✗● badges), ~1/5 width clamped to [30,48], degrade to the bottom bar below 90 total cols. The list cursor IS `app.active` — zero view-local selection state. Focus model: `ActionSidebarFocus` (Tab) swaps input↔list when the tab has no local Tab use (`model.wantsTabKey`), Up/Down/j/k switch tabs live (no Enter), any printable rune bounces focus back into the input and types, Ctrl+Up/Down (`ActionTabPrevAlt`/`NextAlt`) switch from anywhere, click on a card switches. Activity line prefers the agent's in_progress todo over `m.status`. See "Sidebar tab mode" below. |
| `tab_title.go`         | Tab titles for the sidebar cards: seeded instantly from the first user prompt (`fallbackTabTitle`), refined async by a one-shot fantasy LLM call (`generateTabTitleText`, swappable; crush session-title pattern, 30s timeout) → `tabTitleMsg`, persisted on `VirtualSession.Title` (backfilled by `recordVirtualSession` when the title lands before the VS exists) and rehydrated on /resume. Generation is gated on sidebar mode so bar-mode users never pay for the call. |
| `commands.go`          | `cd` / `ls` handlers and `ls` formatting.                               |
| `paths.go`             | Path picker state, tilde expansion, completion.                         |
| `shell.go`             | Shell-mode execution: `$SHELL -c` fork, stdout/stderr pipe streaming, 100-line cap, cwd capture via `pwd > tmpfile`, pgroup SIGKILL on cancel. |
| `worktree.go`          | `inGitCheckout()` (cwd contains `.git`) and `ensureWorktreeGitignore()`. When worktree is enabled, the latter appends `.claude/worktrees/` to `./.gitignore` unless an existing rule already covers it. Both no-op outside a cwd-level git checkout — we do not walk upward. Called at startup when worktree is on in config, on the `/config` → Worktree toggle going true, and guarding the `--worktree` flag in `ensureProc`. Also exports `validateAskCwd(cwd)` — refuses to start an LLM session when ask is inside `.claude/worktrees/<name>` (with a `/resume` hint naming `<name>`) or in any subdirectory of a git/jj checkout. Plain checkout roots and non-checkout dirs pass. The chat-facing gate fires from `sendToProvider`, `handleCommand` slash dispatch, Ctrl+M, and silently from `Init`. `validateExecutorCwd(args, root)` is the executor-level defense in `prepareProviderSessionAt`: when worktree mode is on at a real checkout, `args.Cwd` must point inside `.claude/worktrees/`. |
| `clipboard.go`         | `wl-paste` integration, returns raw bytes + re-encoded PNG.             |
| `kitty.go`             | Kitty graphics protocol: detection, transmit over `/dev/tty`, Unicode placeholder rows. |
| `kitty_diacritics.go`  | The canonical 297-entry Kitty row/column diacritic table.               |
| `ask_question.go`      | Question modal state, rendering, navigation, submit/cancel flow.        |
| `workflows.go`         | Workflow runtime tracker singleton + persistence helpers + status broadcast. |
| `workflow_store.go`    | Two-scope workflow persistence: user (ask.json) + repo (`<root>/.ask/workflows/*.json`, committed), merged repo-first listing, ambiguity-strict name resolution, dir sync on save, cross-scope copy. |
| `workflows_screen.go`  | Workflows builder screen — list/steps/step editor levels with multi-line prompt textarea. |
| `workflows_picker.go`  | Small centred modal popped on `f` (issues) / `Ctrl+F` (chat) to pick which workflow to run. |
| `workflows_run.go`     | Step runner: prompt assembly, advance-on-turn-complete, finalise on done/failed. |
| `workflow_source.go`   | `workflowSource` tagged union (issue ref vs chat transcript) consumed by picker / runner / banner. |
| `chat_workflow.go`     | `Ctrl+F` dispatcher — snapshots `m.history` into a chat source, gates on busy/empty, opens the picker. |
| `keymap.go`            | Remappable global shortcuts — `Action` enum, `KeyBinding` parse/stringify, default keymap, `currentKeyMap()` cached accessor. Per-screen keys (kanban `j/k`, modal arrows, Ctrl+D close) stay inline; this only covers the global screen-switch + tab-nav surface. |
| `config_keybindings.go`| `/config → Keybindings...` sub-picker. Enter on a row arms capture mode; the next non-Esc keypress is persisted to `cfg.Keybindings` and the keymap cache is invalidated so the change takes effect immediately. |
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
| `provider_test.go`         | Provider registry lookup/fallback, providerProc kill + stream drain, user-bar text. |
| `worktree_test.go`         | `.claude/worktrees/` lifecycle against tmp git repos.            |
| `cwd_guard_test.go`        | `validateAskCwd` / `validateExecutorCwd` plus the entry-path gates (sendToProvider, /resume, Ctrl+M, Init). |
| `config_test.go`           | `loadConfig` / `saveConfig` / ollama validation.                 |
| `update_test.go`           | `model.Update` dispatcher behavior via `fakeProvider`.           |
| `tool_output_test.go`      | Tool-call rendering — phrase headline (short mode = phrase only, full = phrase + param rows, no duplicate description row), payload-description rejection heuristics (`toolCallPhrase`), `shortToolFields` native-lowercase-name coverage, tri-state cycling, result clamping. |
| `workflows_test.go`        | Workflow tracker (markWorking/markFinal/lookup/clear), schema round-trip (incl. loop steps), prompt assembly, glyph table, `effectiveMaxIterations`. |
| `workflows_screen_test.go` | Workflows builder state machine — add/rename/delete persistence + edit-while-running guard + loop tree (`stepRows`, add loop/inner, edit max-iters, delete loop/child) + scope copy/move (`c`/`s`, conflict auto-suffix, running guard, per-scope rename). |
| `workflow_store_test.go`   | Two-scope store — filename sanitization, user/repo round-trip (Scope never persisted), dir sync rename/delete, junk-file skip, repo-first resolution + ambiguity errors, cross-scope copy (conflict → new_name, deep clone), projectRoot anchoring. |
| `workflows_picker_test.go` | Picker open/navigate/Enter dispatches `spawnWorkflowTabMsg`. |
| `workflows_run_test.go`    | Step runner — advance (incl. linear no-`end_turn` re-prompt), finalise, fail, idempotent finalise, unknown-provider rejection; loop decision table (iterate / tail break / non-tail break / non-tail proceed / non-tail + tail no-`end_turn` re-prompt / tail no-decision re-prompt / max-iter soft-exit), enter-loop, bounded context, `stepSummaryLine`, `end_turn` signal handling. |
| `issues_workflow_test.go`  | `f` keybind dispatch on the issues screen — toast / picker / focus-existing-tab. |
| `chat_workflow_test.go`    | `Ctrl+F` chat-source flow — transcript filter, key uniqueness, prompt assembly, dispatcher gates (busy/empty/no-workflows/workflow-tab), end-to-end picker → spawn. |
| `keymap_test.go`           | `ParseKeyBinding` / `KeyBinding.String` round-trip, default keymap coverage, load-from-config (unknown/malformed entries skipped, empty-string unbinds), `currentKeyMap` invalidation. |
| `keymap_dispatch_test.go`  | End-to-end: overridden keymap rewires `tabs.go` tab navigation; `/config → Keybindings` capture persists to disk and re-binding to default deletes the entry. |
| `sidebar_test.go`          | Sidebar mode — tabMode parse, geometry (1/5 clamp, degrade threshold, zero bar height), scroll-window/card hit-testing, key routing (Tab focus + completion non-theft, Up/Down switch, type-to-return, Esc, Ctrl+Up/Down both modes), focus-steal suppression (sidebar) vs focus-steal (bar), card title/meta/cost/activity/badge derivation, view composition + `joinBodySidebar`/`clipText`, tabModeChangedMsg propagation, workflow supplant (snapshot, tracker, busy refusal, bar-mode fallback to dedicated tab) and Enter-restore (incl. still-running and dedicated-tab guards). |
| `tab_title_test.go`        | Tab titles — fallback/sanitize (think-tag strip, quote/period trim, clip), `maybeStartTabTitle` gating (bar mode / workflow tab / blank / already titled), swapped-generator cmd round-trip incl. error swallow, `tabTitleMsg` handler (foreign tab, empty title, stale-after-/new), VS persistence + `recordVirtualSession` backfill + /resume rehydration (Title, Preview fallback). |
| `deepseek_test.go`         | Provider metadata/registry/workflow validation, effort→wire mapping, no-key fail-fast, full session lifecycle against `fakeLM` (send, system prompt on wire, kill/exited, resume replays transcript), Materialize, `modelContextLimit`. |
| `agent_run_test.go`        | `fakeLM` (scripted `StreamPart`s) + runtime scenarios: text turn protocol order (done before complete), tool round-trip incl. wire history threading, interrupt = clean end, error turn, shutdown, loop-detection trip, compaction (summary head + auto-continuation), dangling-call repair, task sub-agent tool. |
| `agent_tools_test.go`      | Tool behaviors on `t.TempDir()`: read windows/caps/rejections, write/edit guards (read-before-mutate, stale mtime, uniqueness, replace_all, CRLF), glob/grep/ls, bash via swapped `agentRunShell` (output, exit codes, cancel-kills-pgroup, background jobs), approval denials (`StopTurn`), fetch via httptest, todos validation, required description-phrase schema across the coding core. |
| `agent_tools_ask_test.go`  | Native ask_user_question/end_turn — message shapes via swapped `agentSendToProgram`, cancelled/headless replies. |
| `agent_tools_mcp_test.go`  | MCP manager against in-process `mcp.Server`s over httptest — attach/skip/schema/IsError, image results (placeholder vs media by vision), unreachable-server skip, `tools/list_changed` live refresh, dead-server graceful error, elicitation schema mapping + accept/cancel/headless/url flows. |
| `mcp_servers_test.go`      | Server-config resolution — effectiveType inference, `${VAR}`/`${VAR:-default}` expansion (copy semantics), 3-layer merge (.mcp.json ← global ← project) incl. Disabled tombstones + junk drops + stable order, tool allow/deny filters. |
| `mcp_oauth_test.go`        | OAuth plumbing — token path/0600 round-trip, persisting token source saves on change, callback listener captures code/state via swapped browser opener, stored-valid-token served without a flow, fresh handler yields nil source (transport 401s into Authorize). |
| `agent_tools_bridge_test.go`| Native bridge twins — full 19-tool coverage check, jsonschema field-doc fidelity, description-phrase injection (+ payload-description non-clobber), linear gate error, malformed input, workflow CRUD round-trip against project config, workflow_run dispatch via swapped `mcpSpawnWorkflowTab`, loopback never in `agentSessionMCPServers`. |
| `skills_test.go`           | Skills — discovery validation (bad name / dir mismatch / no description skipped) + project-over-global precedence, trigger block (progressive disclosure, hidden skills), `/name args` expansion incl. user-invocable gating, frontmatter parser, ProbeInit → slash entries. |
| `agent_subagents_test.go`  | Subagents — def discovery/precedence/field parsing, tool grant sets, spec registry, claude model aliases, cross-provider model resolution (swapped LM var), task tool: named agent runs on the pinned provider w/ def prompt + report tail, background job lifecycle (bgTask signals, job_output), default researcher unchanged, `/skill` expansion reaches the wire. |
| `agent_session_test.go`    | Store round-trip (typed parts survive), CreatedAt preservation, list ordering, LoadHistory tool-output modes, Materialize. |
| `agent_prompt_test.go`     | Prompt assembly: env block, context-file discovery/dedupe/cap, determinism, steering tail, worktree clause. |
| `agent_diff_test.go`       | `unifiedDiff` — hunk headers/merging/context caps, no-EOF-newline markers, budget fallback, apply-the-patch property check via `parseUnifiedDiff`. |
| `model_picker_test.go`     | Ctrl+M picker — open/seed (incl. busy gate), header-skipping nav + wrap, fuzzy filter (provider name + friendly name + id), apply semantics (cross-provider clears session, same-provider keeps, no SaveSettings, tab-local), recents (record/cap/dedupe/resurface incl. synthesized custom ids), API-key gate (prompt/save/esc/env-or-config skip), custom-id editor, paste routing, friendly-name + fuzzy helpers, `missingAPIKeyError`. |
| `anthropic_test.go`        | Anthropic spec — metadata/registry, effort→wire incl. clamping, cache-breakpoint placement (`anthropicPrepareStep` marks system + last 2, strips stale, never mutates caller; `anthropicDecorateTools` marks last tool), no-key fail-fast, lifecycle w/ image attachment → wire FilePart, persisted transcript free of cache markers, context windows. |
| `openai_test.go`           | OpenAI spec — metadata/registry, Responses-API prefix predicate, encrypted-reasoning + summary options, effort mapping, no-key fail-fast, lifecycle (images accepted), context windows. |
| `catalog_test.go`          | catwalk lookups — model hit/miss, default-first id list, window/image fallbacks, effort clamping (down to nearest, up from below-range). |
| `cost_test.go`             | Session cost meter — `stepCostUSD` catalog pricing math + unpriceable fallbacks, `formatUSD`, usageMsg/costMsg/tabTitleMsg accumulation + foreign-proc/tab gating, resets (/new, /clear, cross-provider swap keeps same-provider), task-tool sub-agent cost emission, sidebar cost row derivation. |
| `util_test.go` / `paths_test.go` | Pure helpers, path completion, frontmatter parsing.       |

### Testing conventions

- **Every new piece of functionality ships with tests.** This is non-negotiable: when adding a feature, fixing a bug, or refactoring anything in the file table above, add or extend tests in the matching `_test.go` file. A PR that grows the codebase without growing the tests is incomplete.
- Tests must be **behavioral**, not rendering-based. Assert on `model` state, emitted `tea.Msg` values, serialized JSON bytes, file-system state, exec argv — never on styled output strings or view snapshots.
- **No subprocess spawning** except `git` in `worktree_test.go`. Everything else uses the `fakeProvider` from `testhelpers_test.go` or direct function calls. The agent harness keeps this rule via seams: `agentRunShell` (bash exec), `agentGitStatus` (prompt env), `deepseekLanguageModel` (the API client), and `agentSendToProgram` (modal routing) are all swappable vars; `fakeLM` in `agent_run_test.go` scripts whole fantasy streams with zero network.
- Worktree / git tests use `t.TempDir()` + `t.Chdir(...)` so they self-isolate and survive parallel runs.
- HOME-sensitive tests (`session`, `config`, `paths`) call `isolateHome(t)` to pin `$HOME` at a tmp dir so the user's real state is never touched.
- Prefer a few larger scenarios over dozens of trivial one-liners, but do cover each branch of complex functions (see the `agent_run_test.go` scenarios for the pattern).
- Keep the full suite under ~1 second — if you add something slow, figure out how to fake it.

## Bubble Tea wiring

- `Update` is a **value receiver** (`func (m model) Update(...) (tea.Model, tea.Cmd)`). Helpers that need to mutate (`layout`, `appendUser`, `killProc`, etc.) are pointer receivers — Go takes `&m` implicitly on the local copy and the returned `m` propagates back to the runtime.
- `View()` composes everything into one string. When an overlay is needed (slash popover, path picker, modal, scrollbar), we draw onto a `uv.ScreenBuffer` and return its rendered content; otherwise we return the plain body.
- The modal is drawn **on top** of the normal body so the user sees the history underneath — do not early-return a modal-only view.
- **Popups are wide-and-flat.** Design popups prefer a landscape rectangle over a tower: generous width (config modals 84×18, model picker 60–84 wide with a 12-row list cap, keybindings ≥56, workflow picker 72), tight visible-row counts, scroll windows instead of growing taller. New modals should follow this silhouette; `TestModelPicker_ViewIsWiderThanTall` pins the property for the picker.

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
pins its own provider (`anthropic` / `openai` / `deepseek`) + model +
prompt, so a single workflow can chain providers if the user wants. There's no default — an empty workflow list is a
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

### Schema & scopes

A workflow lives in one of two scopes (`workflow_store.go`):

- **user** — `projectConfig.Workflows` alongside `projectConfig.Issues`
  in `~/.config/ask/ask.json` (machine-local, the pre-scope default).
- **repo** — one JSON file per workflow under
  `<projectRoot>/.ask/workflows/` (committed, shared with the team).
  `projectRoot` walks to the main checkout, so every worktree/subdir
  sees the same files — consistent with projectConfig tenancy.

`workflowDef.Scope` is runtime-only (`json:"-"`) — the storage
location IS the scope, so both on-disk shapes are byte-identical to
pre-scope workflows. Every read merges repo-first (`listAllWorkflows`;
name-only lookup prefers repo, the same project-wins convention as
skills/subagents). Names are unique *within* a scope; the same name in
both scopes is legal — UI surfaces show a scope tag, and the mutating
tools refuse to guess (`resolveWorkflowByName` errors on ambiguity
without an explicit scope). All mutations funnel through
`mutateWorkflows`/`saveAllWorkflows`, which split the merged list by
scope and sync the repo dir (write changed files, remove stale ones —
rename = delete+add, VCS-friendly) under the config lock.
`copyWorkflowDef` copies across scopes (or duplicates within one);
target-name conflicts error and demand a `new_name` (the builder
auto-suffixes `-2`, `-3`… instead). Malformed/duplicate committed
files are skipped with a debugLog, never fatal.

The shape is intentionally generic — the runtime takes a
`workflowSource` for the prompt reference (issue or chat snapshot),
but nothing about the broader pipeline machinery is bound to issues.
Future surfaces (PRs, scheduled tasks, …) plug into the same builder
/ runner.

| Type | Purpose |
|------|---------|
| `workflowsConfig{Items, Sessions}` | Per-project block (user scope + run records). |
| `workflowDef{Name, Steps, Scope}` | One named pipeline; Scope is runtime-only. |
| `workflowStep{Name, Kind, …}` | Tagged union. `Kind==""` is an agent step (`Provider`/`Model`/`Prompt`, a fresh one-shot session at run time). `Kind=="loop"` is a loop container (`Steps` inner agent steps, `MaxIterations`, `ExitCondition`). Empty `Kind` keeps pre-loop workflows byte-identical on disk. |
| `workflowSession{Workflow, StepIndex, Status, StartedAt, UpdatedAt}` | Disk-persisted run record — terminal statuses only, always user-side (machine-local). |

Loops are exactly one layer deep — a loop's inner `Steps` must all be agent steps. The on-disk `workflowStep` is recursive, but the MCP wire views use a distinct non-recursive `workflowInnerStepView` for inner steps (the SDK's JSON-schema generator rejects self-referential types, and the separate type makes a nested loop structurally inexpressible). Enforced by `validateSteps` and by the builder never offering "+ New loop" inside a loop.

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
  (`▸ workflow "<name>" · step 2/3: review (openai/gpt-5.5)`),
  completion (`✓ workflow complete`), or failure (`✗ workflow
  failed`).
- The raw chat transcript is **suppressed** on a workflow tab: the
  prompt user-bar (`sendToProvider`), the assistant response text
  (`assistantTextMsg`), tool calls/results (`shouldRenderToolCall` /
  `shouldRenderToolResult`), and diffs (`toolDiffMsg`) are all skipped
  when `m.workflowRun != nil`. Instead the log shows one clean entry
  per step — a `▸ name (provider/model)` header + the agent's
  `end_turn` summary (`stepSummaryLine`) — plus dim `⟳ loop …`
  transition and re-prompt notes (`loopNoteLine` / `workflowNoteLine`).
  Prompt-threading (`stepLog`, the `Previous step output:` block) is
  untouched — the agent still gets full context; only the *display*
  changes.
- `model.Update`'s key dispatch routes through `workflowTabHandleKey`
  before the screen handler runs: only `Ctrl+D` (close), `Ctrl+C`
  (cancel = mark failed), and viewport scroll keys (Up/Down/PgUp/
  PgDn/g/G/j/k/Home/End) are honoured. Everything else is absorbed.
- `askToolRequestMsg` and `approvalRequestMsg` arriving on a
  workflow tab are answered without a modal so the chain doesn't
  stall on a prompt that has no human to dismiss it. Ask replies
  with `askReply{headless: true}`, which `buildAskResult` turns into
  an `IsError` notice (`workflowHeadlessAskNotice`) telling the agent
  it is headless and to proceed on its own judgment — not the
  misleading "user cancelled the dialog" a real Esc produces.
  Approval auto-denies. Workflow tabs run with `skipAllPermissions =
  true` regardless of the global toggle.
- `closeTab` on a still-running workflow tab marks the run as
  failed before tearing down — the user closing the tab is the
  verdict, no graceful drain.

### Step runner

`workflows_run.go` is the chain driver. Each step is a fresh
session (one-shot — the chain doesn't share a provider session
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

The advance handler reads the step's `end_turn` report
(`pendingEndTurn`), appends the step's summary line to the visible
log, rolls the captured text into the appropriate context log,
kills the proc, mutates the cursor, and either dispatches
`workflowRunStartStepMsg` for the next step (deferred so the next
proc spawns at a clean Update boundary) or finalises (`done` on
chain end, `failed` on error). The cursor is `StepIdx` (top-level)
plus an optional `*loopRunFrame` while inside a loop — see "Loop
steps" below. Every step must call `end_turn`; a step that ends its
turn without it is re-prompted in place — see "Per-step `end_turn`
reporting" below.

### Per-step `end_turn` reporting

Every step (linear or loop-inner) must call the `end_turn` MCP tool
once per turn — it is the single source of the clean per-step output
*and* the loop control. The tool (`mcp_workflows.go`) takes a required
`summary` (1-3 sentences, rendered as the step's log line via
`stepSummaryLine`) and an optional `decision` (`continue`/`break`, only
meaningful in a loop). Like `ask_user_question` it blocks on an ack so
the report lands on `pendingEndTurn` before the turn ends; the runner
consumes it at `turnCompleteMsg` (see `handleEndTurnSignal`).

A step that ends its turn **without** calling `end_turn` is re-prompted
in place — "hammered" until it registers, Ctrl+C being the manual
escape. The re-prompt feeds the step's own prior output back so it
doesn't redo the work (`linearText` for a linear step,
`loopRunFrame.retryText` inside a loop) and sets a `remindKind`
(`remindNoSummary` / `remindNoDecision`) so the injected reminder
explains itself. The banner shows the re-prompt count (`re-prompt #N`).

### Loop steps

A loop step (`Kind=="loop"`) runs its inner agent steps repeatedly
until a step registers a **break** (or `MaxIterations` is reached).
The runtime (`workflows_run.go`):

- **Cursor.** `workflowRunState.loop` (`*loopRunFrame`) is non-nil
  while inside a loop; it tracks `innerIdx`, `iteration` (1-based),
  `retry`, and the bounded per-iteration context (`iterationLog`,
  `prevTail`, `retryText`). `startWorkflowStep` enters the loop
  (creates the frame) the first time `StepIdx` lands on a loop step;
  `exitLoop` commits the final iteration's outputs to `stepLog` and
  clears the frame.
- **Decision table** in `advanceWorkflowStep`, against the just-
  finished inner step's `end_turn` report: no report → re-prompt the
  same step; any step's `break` → exit the loop immediately (skipping
  the rest of the iteration — an exceptional early exit); a non-tail
  step with a summary and no break → next inner step; the **tail**
  step's `continue` → next iteration (or soft-exit at the cap); the
  tail with a summary but **no decision** → re-prompt the tail for one
  (`remindNoDecision`). Only the tail is *required* to decide — non-
  tail steps may break early but normally just summarise.
- **Bounded context** (`contextForDispatch`): linear steps see the
  full `stepLog` (a re-prompted linear step also sees its own prior
  output); inside a loop the linear log is frozen and the head inner
  step additionally sees the previous iteration's tail output
  (`prevTail`) while downstream steps see the current iteration's
  prior outputs. A re-prompted inner step also carries its own prior
  output (`retryText`).
- **Cap.** `MaxIterations==0` ⇒ `workflowLoopDefaultMaxIterations`
  (10). Hitting the cap soft-exits (proceeds, never fails).
- **Instructions** are auto-injected by `buildWorkflowStepPrompt` via
  `endTurnInstructionBlock` (the `*stepPromptCtx` arg): the universal
  "call end_turn with a summary" contract, plus inside a loop the
  iteration/goal banner and a position-aware decision clause (tail:
  "you MUST also pass a decision"; non-tail: "omit decision unless
  breaking early").

The `end_turn` tool is a native fantasy tool on every session
(agent_tools_ask.go), so step agents on any provider can call it. Live loop progress (start / iteration / break /
limit) is logged to the tab history via `loopNoteLine`, and the
banner's running line shows `⟳ <loop> · iter N/max · <inner>`.

### Step prompt assembly

`buildWorkflowStepPrompt(step, source, prevOutputs, pc)` produces the
full user turn for a step. `pc *stepPromptCtx` carries the loop framing
(`pc.loop` nil for linear steps) and the re-prompt reason (`pc.remind`):

```
<step.Prompt>

Reference: <owner/repo#N>

Previous step output:        (only when log is non-empty)
<log[0]>
---
<log[1]>
...

<end_turn contract>          (ALWAYS; loop framing + tail decision clause inside a loop)

<re-prompt reminder>         (only when pc.remind != remindNone)
```

Reference format is `<project>#<number>` (no provider prefix, no
URL); the agent has the issue-tracker MCP wired in and resolves the
rest itself. The `end_turn` contract (`endTurnInstructionBlock`) is
appended to **every** step — that's what makes the clean per-step
output possible, so unlike pre-`end_turn` workflows a linear step's
prompt is no longer byte-identical to the bare user prompt.
Whitespace is trimmed at the head and tail; the body stays as the
user wrote it.

### Builder screen (`Ctrl+W` / `/workflows`)

`workflows_screen.go` is a top-level screen (`screenWorkflows`,
peer of ask/issues). State lives on `m.workflowsBuilder` and uses
the standard `renderLayeredConfigBox` chrome for visual parity
with `/config`. Three navigation levels:

| Level | Cursor over | Keys |
|-------|------------|------|
| List (left pane) | workflows (with repo/user scope tags) + "+ New" | enter open / r rename / c copy to other scope / s move scope / d delete / esc back to ask |
| Steps (right pane) | the step tree + affordances | enter edit/create / d delete / tab focus left / esc list |
| Step (right pane) | agent: Name/Provider/Model/Prompt · loop: Name/Max iters/Exit | enter edits the field / esc back to steps |

The steps pane is a **flat list of navigable rows** derived from the
step tree (`stepRows()` — `stepsCursor` indexes it, the single source
of truth). Row 0 is "+ New step"; loops render as a `⟳` header with
`▏`-railed indented inner steps and a trailing "+ add step"; the last
row is "+ New loop". Enter on an affordance creates a step/loop/inner
step and drops into its detail; Enter on a real step/loop opens its
detail. The detail pane branches on kind: an agent step shows
Provider/Model pickers + a Prompt `textarea`; a loop shows an inline
numeric Max-iterations editor (`renaming=="maxiter"`) and an Exit-
condition `textarea` (the textarea is shared, `promptTarget` says
which field it commits to). Every commit writes to disk immediately.

### Edit guards

A workflow that's currently running anywhere in the process
(`workflowTracker().activeWorkflowNames()`) is locked against
rename / delete / step edits — the builder shows a dim
"blocked: workflow is running" toast in the help row. Once the
run finalises (or the tab closes), the lock releases.


## Sidebar tab mode

`cfg.UI.TabMode` (`/config → Global Options → Tab Mode`) selects how
tabs present: `"bar"` (default, the bottom strip) or `"sidebar"` — a
permanent right-hand column owned by the app layer (`sidebar.go`).
Width is ~1/5 of the terminal clamped to [30,48] cols; below 90 total
cols rendering silently degrades to the bar (behaviour like workflow
supplanting still follows the *mode*, not the width). The column is a
pure projection of `a.tabs`: the selection cursor IS `app.active`, the
scroll offset is derived, and every card reads live model state
(title, provider/model, accumulated session spend in USD, in_progress
todo / stream status / workflow step, ⚠ needs-input · ✓ done · ✗
failed · ● busy badges) at render time. The cost row is fed by
`model.sessionCostUSD`: usage.go’s `stepCostUSD` prices every API call
against catwalk’s embedded per-1M rates (crush’s formula — cache
writes at the in-cached rate, cache reads at the out-cached rate) and
the meter counts main-loop steps (`usageMsg`), task sub-agents and the
compaction summarizer (`costMsg`), and the tab-title call
(`tabTitleMsg`). Unpriceable models (custom ids / no catalog) render
an empty row, never a fake $0.00; the meter resets with the
conversation (/new, /clear, /resume pick, cross-provider swap) and
survives same-provider model swaps. `app.tabMode` mirrors the config (seeded by `newApp` from the
first tab, refreshed on `openTab` reloads and `tabModeChangedMsg`
broadcasts from the /config toggle, which also refresh each tab's
`m.sidebarMode`).

Keyboard: `ActionSidebarFocus` (default Tab) swaps focus between the
typing area and the list — intercepted at the app layer only when the
active tab reports no local use for Tab (`model.wantsTabKey`: false on
an idle chat input, true for modals, non-ask screens, completion
popovers, inline confirms). While the list is focused, Up/Down (and
j/k) switch the active tab immediately — no Enter — typing any
printable rune bounces focus back into the input and types it, Esc /
Enter / Tab return, Ctrl+D closes the selected tab, everything else is
absorbed. `ActionTabPrevAlt`/`ActionTabNextAlt` (Ctrl+Up/Down) switch
tabs from anywhere in both modes. Mouse clicks on a card switch to it;
wheel events over the column are absorbed.

Behavioural changes while the mode is on:

- **No focus theft.** `dispatchByTabID` no longer force-focuses a tab
  whose ask/approval modal fires — the request parks on the tab's
  modal state and the card shows the ⚠ badge until the user switches.
- **Workflows supplant instead of spawning.** `spawnWorkflowTabMsg`
  routes to `app.supplantWorkflow`: the run attaches to the origin tab
  (busy/shell-streaming/already-running tabs refuse with a toast — no
  queue), a `workflowTabSnapshot` captures the provider/session state,
  the tab flips to the read-only banner, and when the chain finishes
  Enter restores the conversation (`restoreSupplantedTab`) with the
  step summaries left in the transcript. Closing the tab mid-run still
  marks the run failed. Bar mode keeps the dedicated-tab behaviour.
- **Tab titles.** The first prompt seeds `m.tabTitle` instantly and a
  one-shot LLM call (`tab_title.go`) refines it in the background;
  titles persist on the VirtualSession and rehydrate on /resume.

## In-process API providers (fantasy agents)

`deepseek`, `anthropic`, and `openai` run with **no CLI subprocess**:
the agent loop runs inside ask, built on `charm.land/fantasy`
(Apache-2.0, the agent runtime crush uses; v0.30+ repairs malformed
tool-call JSON by default — important for DeepSeek — so don't pass a
custom `WithRepairToolCall` that would shadow it). Crush itself is
FSL-licensed: design reference only, never copy its code.

### Provider seam

ONE generic implementation (`agentAPIProvider`, agent_provider.go)
satisfies the `Provider` interface for every spec, with
`providerProc.cmd == nil`: StartSession spawns a goroutine
(`agentSession.run`), `stdin` is an adapter whose `Close()` tears the
session down (that's what `killProc` calls), and `Interrupt` cancels
the in-flight turn's context cooperatively (handled=true, codex-style —
the session emits its own turn end). A provider is an
`agentProviderSpec` value (deepseek.go / anthropic.go / openai.go):
identity, model/effort options, `buildModel` (via a swappable
package-level var for tests), `callOptions` (effort→wire),
`prepareStep`/`decorateTools` hooks, image capability, context window,
and config-block accessors. Registration order is explicit in
provider.go's single `init()` — anthropic first (the default for an
empty config), then openai, then deepseek.
Everything else (tabs, workflows, banner, cancellation, the Ctrl+M picker)
works because the session emits the shared provider message protocol:
`streamStatusMsg`, `assistantTextMsg` (one per completed text block,
emitted at `OnTextEnd`), `toolCallMsg`/`toolResultMsg`, `toolDiffMsg`
(via `unifiedDiff` + `parseUnifiedDiff` after edit/write),
`todoUpdatedMsg`, `usageMsg` (input + cache tokens, codex-style
context footprint), then **`providerDoneMsg` before `turnCompleteMsg`**
(the workflow runner depends on that order),
and `providerExitedMsg` + channel close on shutdown.

### Wire mechanics

**DeepSeek** rides fantasy's `openaicompat` (reasoning_content
echo-back, index-keyed tool-call delta merge, retry-after backoff).
Options keyed by provider name ("deepseek" via `openaicompat.WithName`):
`off` → `extra_body: {thinking: {type: disabled}}` + temperature 0.0,
`high` → `reasoning_effort: high`, `max` → `xhigh`. Models
`deepseek-v4-pro` (default) / `deepseek-v4-flash`, 1M context; the
deprecated `deepseek-chat`/`deepseek-reasoner` aliases (retire
2026-07-24) are deliberately absent. Images rejected.

**Anthropic** rides fantasy's `anthropic` provider. Effort maps to
`output_config.effort` (adaptive thinking on current models), clamped
to the model's published levels via catwalk. **Prompt caching is
manual on the raw API**: `anthropicPrepareStep` clones the step
messages, strips stale markers, and marks the system message + last
two messages; `anthropicDecorateTools` marks the last tool definition.
That's ≤4 ephemeral breakpoints (the API max). Never set sampling
params — thinking-enabled requests reject them.

**OpenAI** rides fantasy's `openai` provider with
`WithUseResponsesAPI()` + a prefix predicate (`openaiUseResponsesAPI`)
so gpt-5.x/codex/o-series ids always take the Responses API even when
fantasy's exact-id list lags. Reasoning summaries on; encrypted
reasoning content always requested (Store defaults false → stateless
replay needs the blobs round-tripped in persisted messages).

Model metadata (context windows, image capability, reasoning levels)
comes from `charm.land/catwalk`'s embedded catalog (catalog.go) with
conservative 200k fallbacks for unknown ids. The system prompt is
built once per session and reused verbatim so prefix caching
(automatic on DeepSeek, explicit breakpoints on Anthropic) hits;
volatile env (git status) is a labeled session-start snapshot.

### Tools

Coding core (read/write/edit/glob/grep/ls/bash+jobs/fetch/todos/task)
plus native twins of EVERY bridge tool: `ask_user_question`/`end_turn`
(agent_tools_ask.go) and the full `linear_*`/`workflow_*` set
(agent_tools_bridge.go, same cores as the bridge handlers). The
loopback bridge is never attached in-process — only the project
GitHub MCP and user-configured servers (mcp_servers.go) ride the MCP
client as `mcp__<server>__<tool>` tools. Memory recall is injected
natively (agent_memory.go) at three points: the system prompt at
session start, the wire prompt per user turn, and a per-file footer
on read/edit/write results. Permissions: when `SkipAllPermissions`
is off, mutating
tools (bash beyond the safe read-only list, edit/write, fetch) block
on the existing approval modal; denial returns an error result with
`StopTurn` so the model ends its turn instead of retrying.

**Description phrases.** Every native tool — the coding core, the
modal pair, and the bridge twins (injected generically by
`nativeBridgeTool`, skipping inputs whose own `description` is real
payload like linear_create_issue's Markdown body) — takes a
*required* `description` param: a model-authored phrase (under 10
words) saying what the call is doing. The claude-code/crush trick:
the model writes the headline itself in the same tool call, no
second summarization pass. The UI renders it as the call headline
(`▸ bash — Looking for the latest files`, tool_output.go) — in
short mode (default) the phrase IS the whole entry; full mode adds
the param rows — and as the streaming status (`bash: Looking for
the latest files`, agent_run.go). `toolCallPhrase` gates what
qualifies (single line, ≤120 chars) so payload `description` fields
on MCP/bridge tools never masquerade as the headline; calls without
a phrase (old transcripts, MCP tools) fall back to the
`shortToolFields` allowlist, keyed by the native lowercase names.

### Sessions & turn hygiene

Transcripts persist as fantasy message arrays (typed parts survive
JSON round-trip) under `~/.config/ask/agent-sessions/<provider>/`,
keyed by a claude-code-style cwd encoding for the project dirs. Resume
replays the stored messages into the next wire call; Materialize
seeds a fresh transcript from NeutralTurns for cross-provider
Ctrl+M provider swaps. Before persisting, `repairDanglingToolCalls`
synthesizes error results for any unanswered tool call so a resumed
transcript never violates the strict call/result pairing. Loop
detection (identical tool-step signatures, >5 repeats in a 10-step
window) stops runaway turns; context pressure (≤20K headroom of the
model's window) stops the turn, summarizes the transcript into a new
user-role head message, and auto-queues a continuation turn when the
model was mid-tool-loop. A cancelled turn is not persisted — resume
lands at the last completed turn.

## Clipboard and thumbnails

- Only Wayland is supported. Don't add X11 / macOS fallbacks without asking.
- `wl-paste --list-types` picks the first `image/{png,jpeg,gif,webp}` entry; the raw bytes go straight to the provider (whatever mime), a PNG re-encode goes to Kitty.
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

## MCP (client only)

ask is an MCP CLIENT, never a server. `agent_tools_mcp.go` owns the
per-session manager (stdio/http/sse transports, official go-sdk
v1.6.1, lazy ping-and-rebuild, tools/list_changed refresh, elicitation
→ question modal, OAuth via mcp_oauth.go). Servers come from
`mcpServers` maps (user-global + per-project) merged over the
project-root `.mcp.json` (mcp_servers.go). The old loopback bridge is
gone — its tools are native fantasy tools (agent_tools_ask.go,
agent_tools_bridge.go) and the question modal is driven by
`askToolRequestMsg` (ask_wire.go) directly.

## Conventions

- No new runtime dependencies without asking. We already carry Charm (bubbletea/bubbles/lipgloss/glamour/ultraviolet), `charm.land/fantasy` (the agent runtime behind the deepseek provider — user-approved), the official MCP SDK, golang.org/x/net (fetch tool's HTML→text), and stdlib.
- Only emojis that already exist in the codebase (`✓`, `✗`, `▸`, `›`, `▏`) — nothing new unless the user asks.
- Comments: default to none. Only add one when a reader cannot derive the reason from the code.
- Debug logging uses `debugLog(format, args...)` and is a no-op unless `ASK_DEBUG=1`. Add one when crossing an async boundary (paste command, MCP handler, provider stream, tool dispatch).

## Known-fragile areas

- `layout()` extra-row math: any change to what appears between viewport and input (chip, thumbnail strip, spacer row) needs the `extra` term in `layout()` and the emission order in `viewBody()` kept in sync.
- Scrollbar column is drawn over `m.width-1`. If any text-rendering style grows a margin or a user-bar width past `m.width-1`, the scrollbar will be overwritten or vice-versa.
- `askToolRequestMsg` is rejected if the modal is already open — only one MCP ask at a time. Double-calls from Claude return `cancelled: true` for the second one.
- `contentFingerprint` must mix in `len(m.history[m.shellOutIdx].text)` whenever a shell output entry is active. The frame cache is keyed on `len(m.history) | m.width`, and shell mode appends streamed lines in place to a single history entry, so without that extra term the cache returns a stale (first-line-only) view until something else (spinner row, window resize) perturbs the key.
