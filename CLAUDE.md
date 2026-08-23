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

when working on the agent runtime, ALWAYS read the Google ADK Go documentation and source:

* https://google.github.io/adk-docs/
* https://github.com/google/adk-go

when working on MCP (client transports, OAuth, elicitation), ALWAYS read the official Go SDK source at the pinned version:

* https://github.com/modelcontextprotocol/go-sdk

and for skills/subagent file formats, the Agent Skills standard:

* https://agentskills.io

ALL OF THE ABOVE IS NOT OPTIONAL. YOU MUST ALWAYS USE THE ABOVE REFERENCES.

## General info

`ask` is a Bubble Tea v2 TUI coding agent. The agent loop runs
in-process on Google ADK 2.0 (`google.golang.org/adk/v2`) and the GenAI SDK
against Vertex AI Gemini — there are NO CLI subprocesses and NO loopback MCP
server; every tool (coding core, linear/workflow bridge twins,
question modal, external MCP clients) is native Go. The tool surface
is two-tier: a small fixed CORE rides the wire tool definitions,
everything else lives in a deferred registry reached through
`search_tools` + `invoke_tool` (see "Tool registry vs core tools"
below). ask renders markdown, images, and a custom question modal.

## Layout

One `package main`, one file per concern.

| File                   | Purpose                                                                 |
|------------------------|-------------------------------------------------------------------------|
| `main.go`              | Entry point. Builds `initialModel`, runs `tea.Program`, owns CLI arg parsing (`ask resume <vid>`). |
| `types.go`             | All type defs, model struct, style vars, slash command registry.        |
| `update.go`            | `Init`, `Update` dispatcher, input and session-picker key handlers.     |
| `view.go`              | `View`, layout math, viewport rendering, markdown cache, scrollbar, modal overlay. |
| `agent_provider.go`    | `agentProviderSpec` + the generic `agentAPIProvider` — ONE Provider implementation for the in-process API provider (Vertex AI): StartSession → `agentSession`, capability-gated image attachments, shared tool assembly (`agentSessionTools`), per-spec settings accessors. `ModelPicker()` is synchronous and network-free (cached live listing from model_catalog.go, else the static catalog ids); `ListModels` (the `modelLister` interface) is the network path the catalog load calls. |
| `vertex.go`           | Vertex AI spec: project + location + ADC auth (`google.New(google.WithVertex)`), filters Claude ids out of the catwalk model list, `vertexPrepareCredentials` env-mutation seam for the optional SA-key path. |
| `config_vertex.go`    | `/config → Vertex AI...` sub-picker. Three free-text rows (Project, Location, Service Account Key path) with regex validation + `expandTilde` for the SA-key row. |
| `pkg/providers/catalog.go` | Static catalog (`ModelInfo`): the built-in Vertex/OpenRouter ids (default first), context windows, max output, image capability, reasoning levels, and the published list price for the Gemini models. No network. `CatalogModel` is strict on provider id — an unknown provider misses instead of borrowing Vertex's table. |
| `pkg/providers/model_meta.go` | `ModelMeta` — the merged, display-ready description of a model (name, description, context, max output, per-1M pricing, modalities, reasoning levels, knowledge cutoff, release date, status) and `ModelMetaFor`, the layered lookup: static catalog → models.dev → the provider's live listing (OpenRouter), higher layers overwriting only the fields they actually know. Precedence policy — see "Model metadata precedence" below. Never touches the network. `StepCostUSD` (via the swappable `ModelMetaLookup`) prices token usage against the same data so the cost meter and the picker never disagree. |
| `pkg/providers/modelsdev.go` | models.dev client (MIT community model database, `https://models.dev/api.json`): `LoadModelsDev` = memory → disk cache (`~/.config/ask/cache/models-dev.json`, 24h TTL) → network, with a stale-cache fallback on a failed fetch; only the mapped providers (`vertex`→`google-vertex`, `openrouter`) are parsed out of the multi-MB payload. Vertex's publisher-model API returns nothing beyond an id, so for Vertex this is the only source of descriptions, limits, and prices. Seams: `ModelsDevURL`, `ModelsDevHTTPClient`, `ModelsDevCachePath`. |
| `agent_run.go`         | The in-process agent runtime: session goroutine, ADK agent loop, per-spec PrepareStep, image file parts, interrupt, loop detection, auto-compaction. `refreshToolset` splits the surface: core → wire tool definitions, bridge twins + MCP → the deferred registry. |
| `agent_prompt.go`      | Coder system prompt assembly: static head, env snapshot, CLAUDE.md/AGENTS.md inclusion, `askSteeringPrompt` tail. Byte-stable per session for DeepSeek's prefix cache. Context files are discovered by `AgentContextSearchDirs` — the user-global `~/.claude/` scope first, then every directory from the project root down to cwd (general before specific), matching what `.claude/rules`, skills, and subagents already do. Each distinct file loads **once**: dedupe is by resolved path (`ContextFileRealPath` → `EvalSymlinks`), so the common `AGENTS.md -> CLAUDE.md` symlink contributes one body, not two. |
| `agent_session.go`     | `agentSessionStore` — ADK FileSessionService transcripts under ~/.config/ask/agent-sessions/<provider>/, backing /resume, LoadHistory replay, and Materialize. |
| `pkg/diff`             | Modular subpackage implementing pure-Go Myers unified diffing (`Unified`) and parsing (`Parse`) algorithms. |
| `agent_tools.go`       | Shared tool infra: `agentToolEnv` (cwd, emit, approval gate), read-tracker, output caps/truncation. |
| `agent_tools_file.go`  | read / write / edit tools (exact-match edits, read-before-mutate + stale-mtime guards, CRLF preserve, diff emission). write/edit refuse to mutate until a todos list has applied this session (`requireTodosNotice`, see `agent_tools_todos.go`) only when the opt-in `Gate Todos Before Mutate` config flag is on (default off); otherwise they proceed normally. |
| `agent_tools_search.go`| glob (doublestar + `{a,b}`), grep (rg with pure-Go fallback), ls (capped tree). |
| `agent_tools_bash.go`  | bash (+`run_in_background`), job manager, job_output / job_kill, safe read-only command list. Exec layer behind `agentRunShell` for tests. Implements `SUDO_ASKPASS` IPC modal support: subprocesses prompting for sudo are intercepted via the `ask` binary as an IPC askpass provider, piping the request into the TUI's question modal. |
| `agent_tools_web.go`   | fetch — HTTP GET with 100KB cap, HTML→text extraction via x/net/html. ALSO the Brave-backed `web_search` core tool (the second documented core exception alongside fetch): `https://api.search.brave.com/res/v1/web/search` with `X-Subscription-Token` (`braveSearchClient` swappable), formats `web.results[]`, and on a missing Brave key returns a NON-error notice telling the model to finish its turn and tell the user to add a key in `/config → Web Search`. Only attached for providers WITHOUT first-party search (DeepSeek/Kimi) — anthropic/openai run native search via the spec's `nativeWebSearch`. |
| `agent_tools_todos.go` | todos tool — full-list replace emitting `todoUpdatedMsg` into the existing todo surface. The description carries an explicit cadence contract (one call per status transition) and every ack appends a state-keyed nudge ("call again the moment the in_progress item is done" / "no item is in_progress") so models keep the list live instead of planning once and closing everything at the end. ALSO the two-stage **workflow guard**: in a project that defines workflows (`env.workflowsAvailable`, set once at session start) the todos tool gates multi-step work in two checkpoints, each firing at most once per session, but only when the opt-in `Gate Todos Before Mutate` config flag is on (default off). STAGE 1 (`workflowGuardShouldFire`): the first todos call before the model has looked at the workflows is REJECTED without applying the list — `workflowGuardTodosNotice` steers it to call the `workflow_list` core tool directly (agent_tools_workflow.go), forces a one-line fit verdict judged against each workflow's **Description** (its stated purpose — NOT the step names, since inferring scope from step structure is how a fitting workflow gets wrongly declined), and names the exact mechanic (a fitting workflow + user approval ⇒ the next action MUST be calling `finalized_plan` with the workflow suggested as `default_workflow`). STAGE 2 (`workflowDecisionGuardShouldFire`): once the model has looked (`workflowsChecked`) but is now re-sending a list to start inline work WITHOUT ever proposing a workflow, it is rejected once more — `workflowDecisionGuardNotice` makes it reconcile that decision (run the workflow, or confirm with the user it's declining the workflow). This catches the real DeepSeek failure where the model asks, gets a yes, then proceeds inline anyway. Stage 1 disarms when `workflow_list` is invoked (`env.markWorkflowsChecked`); stage 2 disarms when inline execution or a workflow run is proposed/approved via `finalized_plan` — both hooks live in the tool closures themselves, agent_tools_workflow.go. Each stage latches after one fire so a model legitimately proceeding inline is never blocked past these two checkpoints. Inert when the project has no workflows, the gate is false, or the two-stage check lives in `env.workflowGuardNotice()` (agent_tools.go). A successful todos call sets `env.todosApplied` (`markTodosApplied`). When the gate is on, `write`/`edit` refuse to mutate until that flag is set (`requireTodosNotice` → `requireTodosBeforeMutateNotice`, agent_tools_file.go), making the todos call a mandatory chokepoint before any code change. When the gate is off (default), the workflow guard and require-todos gate are both inert. |
| `agent_tools_task.go`  | task tool v2: default read-only researcher on the parent model, OR a named subagent definition (`agent:` param) with its own instructions/tools/model — including a DIFFERENT in-process provider (cross-provider delegation). `run_in_background` rides the bash job manager (job_output/job_kill + bgTask UI signals). |
| `agent_subagents.go`   | Named subagent defs: `.claude/agents/*.md` + `~/.config/ask/agents` (frontmatter name/description/tools/model + ask's `provider` extension; body = system prompt), `<available_agents>` prompt block, tool grant sets (default read-only, `*` = coding core, never task/modal tools), claude model-alias mapping, cross-provider model resolution via `agentSpecByID`. |
| `skills.go`            | Agent Skills standard (agentskills.io) using native ADK `skilltoolset` and `skill.Source`: SKILL.md discovery, progressive disclosure (body loads dynamically), `/skill-name` slash expansion, user-invocable skills surfaced through `ProbeInit`. |
| `pkg/plugin/`          | Claude Code plugin-marketplace format and ask's operations over it: `manifest.go` (marketplace.json / plugin.json / the `source` union: path, `github`, `url`, `git-subdir`), `source.go` (what `/skills add marketplace` accepts), `store.go` (state files under `~/.config/ask/plugins/` in Claude Code's shapes + `<root>/.ask/plugins.json` for project scope), `marketplace.go` (add / remove / refresh / init), `install.go` (install / uninstall / `EnabledPlugins` / `SyncProject`), `contents.go` (`ResolveContents`: strict merge vs `strict:false` entry, default dirs, single-skill shorthand), `publish.go` (land a skill/agent/workflow as a plugin, lossless marketplace.json upsert, git commit/push), `claude.go` (explicit "Import from Claude" over `~/.claude/plugins` + `settings.json`). `RunGit` / `HTTPClient` / `ClaudeHome` / `Now` are the seams. |
| `pkg/engine/skill_store.go` | Skill/agent CRUD behind the tools and the browser: `CreateSkill`/`UpdateSkill`/`DeleteSkill`, `CreateAgent`/`UpdateAgent`/`DeleteAgent`, `RenderSkillFile`/`RenderAgentFile`. New definitions land in ask's own dirs (`~/.config/ask/{skills,agents}` user, `<root>/.ask/{skills,agents}` project); edits rewrite the discovered file in place keeping unknown frontmatter keys; plugin copies are read-only (`Origin.Editable`). Every mutation calls `BumpSkillsGeneration`. |
| `pkg/tools/extensions.go` | The registry (never wire) tool set over skills/agents/plugins/marketplaces: `skill_list`/`skill_get`/`skill_create`/`skill_edit`/`skill_delete`, `agent_create`/`agent_edit`/`agent_delete`, `marketplace_list`/`marketplace_search`/`marketplace_add`, `plugin_install`/`plugin_uninstall`, `skill_publish`, `skill_pull`. Built with `NativeBridgeTool` over shared cores the browser also calls (`MarketplaceSearch`, `PublishTargetFor`/`PublishItem`/`PullItem`, `WorkflowProviderWarnings`); items carry a `published` view with the sync status. `marketplace_add`, `plugin_install`, and `skill_publish` go through the approval gate; every mutation emits `ExtensionsChangedEvent`. |
| `skills_browser.go`    | `/skills` / Ctrl+S browser state + keys (`modeSkillsBrowser`): two lenses switched by Tab — Installed (Project / User / one group per enabled plugin; every row tagged skill · agent · wf, ⚠ on workflows whose step provider is not configured) and Marketplace (one group per registered marketplace, plugins with ✓ when enabled, Enter drills into a plugin's contents, tail rows `+ Add marketplace` / `Import from Claude Code` / `+ New marketplace`). Every plain key types into the search, so actions are Ctrl+letter (never Ctrl+I = Tab, Ctrl+M = Enter, Ctrl+C = close) or Delete, and ↑/↓ are the only list-nav keys (no Ctrl+P/N aliases here): Enter (skill → `/name ` into the input, agent → a delegation prompt, plugin row → drill, item inside a drill → install), Ctrl+G install (scope prompt), Ctrl+D / Delete remove whatever the row is — the one overlay where Ctrl+D is NOT close-tab (Esc closes) (user/project skill or agent → delete the file after a confirm; anything plugin-backed → uninstall the plugin), Ctrl+N / Ctrl+E hand creation/editing to the agent by pre-filling a `skill_create`/`skill_edit` prompt, Ctrl+P publish (writable-marketplace chooser the first time; an update to the same plugin after that, confirming when the marketplace copy changed), Ctrl+U pull the marketplace copy back (confirm), Ctrl+A add marketplace, Ctrl+X remove marketplace, Ctrl+R refresh + `SyncProject`; Import from Claude is the tail row (and `/skills import claude`). Both footers list the keys that apply to the selected row. Mutations are async cmds resolving to `skillsBrowserOpDoneMsg`; the browser rebuilds from disk and broadcasts `extensionsChangedMsg` so every tab re-runs `ProbeInit` (new slash commands register mid-session). `/skills add marketplace <src> [project]`, `/skills remove marketplace <name>`, `/skills import claude`, `/skills refresh` run without the browser. |
| `skills_browser_view.go` | The browser's look: the model picker's box/geometry/divider/scroll window (`modelPickerGeometry`, `modelPickerBoxStyle`, `modelPickerWindow`) with a lens tab line, the row renderer, and the detail pane (skill: slash, model-invoked, support files, path, glamour body; agent: invoke, model, tools, prompt; workflow: scope, steps, provider warnings; plugin: status, category, author, source, listed skills; inline editors in the same pane). Composited at the app layer next to the model picker overlay. |
| `rules.go`             | Claude Code `.claude/rules/` standard: `*.md` rule files discovered recursively (symlink-following, cycle-guarded) under project `.claude/rules/` (git root) and user `~/.claude/rules/` (user loads first, project wins on same relative label). YAML `paths` frontmatter (block + inline list forms, brace patterns survive verbatim) splits rules two ways — no `paths` ⇒ EAGER (`rulesPromptBlock` → `<project_rules>` system-prompt block, byte-stable for prefix caching), with `paths` ⇒ JIT (`ruleAwareTool` decorates the read tool; reading a file whose project-root-relative path matches a glob appends the rule body to that tool result, once per rule per session, project-scope only). Globs reuse `agentGlobMatch` (doublestar + `{a,b}`). The same decorator ALSO injects project instruction files it walks past — but only ones the model has not already been given: `WrapContextAwareTools` seeds `seenCtxFile` with everything `AgentContextFiles(cwd)` already put in `<project_instructions>`, so a read next to CLAUDE.md no longer re-sends CLAUDE.md. Only genuinely unseen instructions (a `CLAUDE.md` in a subdirectory *below* cwd) arrive JIT, once each. Do not remove that seeding — without it, one `read` of a 3KB file returned 99KB. |
| `agent_tools_ask.go`   | In-process twins of the bridge's `ask_user_question` / `end_turn` — same modal/workflow machinery, no HTTP loopback. |
| `agent_tools_bridge.go`| Native twins of the `linear_*` bridge tools: a generic `nativeBridgeTool` adapter generates ADK schemas via jsonschema-go. In-process sessions never attach the loopback bridge. These tools live in the deferred registry, never on the wire. |
| `agent_tools_workflow.go`| The ask-built-in workflow tools on the CORE wire toolset: `workflow_list`, `workflow_get`, `workflow_create`, `workflow_edit`, `workflow_delete`, and `workflow_copy`. Built with the same `nativeBridgeTool` adapter (so wire schemas are byte-identical to the prior registry shape) and the shared cwd-parameterized cores in mcp_workflows.go. Deliberate, documented core exception: the two-stage workflow guard (agent_tools_todos.go) forces the model to call `workflow_list` as a precondition for any multi-step work, and an extra `search_tools` round-trip on every guard interaction is pure overhead. The disarm hook (`env.markWorkflowsChecked`) lives in the `workflow_list` closure itself so the guard clears on the direct call — they are NOT in `invoke_tool` anymore. Don't bypass the bridge adapter for a new workflow tool; the adapter's `flattenNullableTypes` pass is what keeps `workflow_create` / `workflow_edit` from emitting the type-arrays Moonshot's strict validator rejects. |
| `agent_tools_registry.go`| The deferred tool registry surface: `search_tools` (query the registry — `*` / prefix-`*` / substring — returning name + description + full input_schema per match) and `invoke_tool` (dispatch a registry tool by name via its `.Run`, with replicated required-field validation, phrase injection for natives, and verbatim response pass-through). `unwrapInvokeToolCall` maps invoke calls back to the inner tool for display. See "Tool registry vs core tools" below — **new tools go here, never into the core list**, unless a deliberate, documented exception is in play (the two today are `web_search` and the workflow_* tools). |
| `agent_memory.go`      | Memory recall injection: ADK memory.Service implementation (sqlite-vec) and native loadmemorytool/preloadmemorytool. All no-op when the memory service is closed. |
| `agent_tools_mcp.go`   | MCP client v2 (`mcpManager`/`mcpServerConn`): per-session manager over stdio/http/sse transports (official go-sdk v1.6.1), lazy ping-and-rebuild before every call + one renew-and-retry, `tools/list_changed` → live deferred-registry refresh (the wire toolset never changes mid-session), MCP elicitation → ask's question modal (form mode: enum/boolean/free-form, typed answers; URL mode + headless decline), image tool-results as real media when the model has vision. Tools are `mcp__<server>__<tool>`. |
| `mcp_servers.go`       | User-facing MCP server config: `mcpServers` maps (user-global + per-project) merged over project-root `.mcp.json` (claude-code convention), `${VAR}`/`${VAR:-default}` expansion, per-server type inference, timeout, enabled/disabled tool filters, Disabled tombstones. |
| `mcp_oauth.go`         | OAuth for remote MCP servers (`oauth: true`): SDK authorization-code + PKCE + dynamic client registration, browser launch via swappable `mcpOAuthOpenBrowser`, one-shot loopback callback listener, tokens persisted 0600 under `~/.config/ask/mcp-oauth/` (valid stored tokens skip the browser; expiry re-runs the flow). |
| `model_picker.go`      | Ctrl+M model picker state + keys: “Recently used” group first (`cfg.RecentModels`, capped push-front, recency order), then one section per provider with models natural-sorted by display name (`naturalLess`, util.go); type-to-search from anywhere, ↑/↓ skip headers, PgUp/PgDn scroll the detail pane, Ctrl+R forces a catalog refresh, Enter applies the pick to the current tab and persists it as the provider default model. Opening dispatches `modelPickerLoadCmd` (model_catalog.go) and `rebuild` re-reads the cache when `modelCatalogLoadedMsg` lands, keeping query + cursor. Picking a model whose provider has no key drops into an inline API-key prompt (`providerKeySpecs`) that saves to ask.json and proceeds. |
| `model_picker_view.go` | The picker's look: ONE square-cornered box (`modelPickerBoxStyle`, `NormalBorder` — the first surface on the square look every overlay is moving to) sized to the terminal minus a margin (4 cols / 2 rows, collapsing to 1 on small terminals; `modelPickerGeometry`), a thin `│` divider, list column ~38% / detail column the rest. The detail pane is the selected model's `ModelMetaFor` data: title, provider · id, glamour-rendered description (cached per model+width), then label/value facts (`modelDetailFacts`: Context, Max output, Input/Output/Cached input/Cache write per 1M, Modalities, Reasoning, Knowledge, Released, Status) — or “no information available”. The key-prompt and custom-id editors render in the same box. `modelPickerOverlay` is composited at the APP layer (`drawOverlayCentered` in `app.View`, tabs.go) so it covers the sidebar — it is the one overlay not drawn inside the tab body. |
| `model_catalog.go`     | Process-wide model catalog cache behind the picker and the workflows builder: `modelCatalogRefreshCmd` (single-flight; re-runs only on force or while models.dev has never loaded) → `loadModelCatalogCmd` fetches models.dev (`loadModelsDev` alias) and every `modelLister`'s ids concurrently, installs them (`cacheModelOptions`, per-provider error notes), then broadcasts `modelCatalogLoadedMsg`. Triggered by Ctrl+M, Ctrl+R in the picker, and entering the workflows screen. |
| `sidebar.go`           | Right-hand column of per-tab task cards (title / provider·model / session $ spend / live activity + ⚠✓✗● badges), ~1/5 width clamped to [30,48]. The sidebar is the only tab mode (the bottom bar has been removed). The list cursor IS `app.active` — zero view-local selection state. Focus model: `ActionSidebarFocus` (Tab) swaps input↔list when the tab has no local Tab use (`model.wantsTabKey`), Up/Down/j/k switch tabs live (no Enter), any printable rune bounces focus back into the input and types, Ctrl+Up/Down (`ActionTabPrevAlt`/`NextAlt`) switch from anywhere, click on a card switches. Activity line prefers the agent's in_progress todo over `m.status`. |
| `tab_title.go`         | Tab titles for the sidebar cards: seeded instantly from the first user prompt (`fallbackTabTitle`), refined async by a one-shot fantasy LLM call (`generateTabTitleText`, swappable; crush session-title pattern, 30s timeout) → `tabTitleMsg`, persisted on `VirtualSession.Title` (backfilled by `recordVirtualSession` when the title lands before the VS exists) and rehydrated on /resume. Generation always runs since the sidebar is the only tab mode. |
| `commands.go`          | `cd` / `ls` handlers and `ls` formatting.                               |
| `paths.go`             | Path picker state, tilde expansion, completion.                         |
| `shell.go`             | Shell-mode execution: `$SHELL -c` fork, stdout/stderr pipe streaming, 100-line cap, cwd capture via `pwd > tmpfile`, pgroup SIGKILL on cancel. |
| `worktree.go`          | `inGitCheckout()` (cwd contains `.git`) and `ensureWorktreeGitignore()`. When worktree is enabled, the latter appends `.claude/worktrees/` to `./.gitignore` unless an existing rule already covers it. Both no-op outside a cwd-level git checkout — we do not walk upward. Called at startup when worktree is on in config, on the `/config` → Worktree toggle going true, and guarding the `--worktree` flag in `ensureProc`. Also exports `validateAskCwd(cwd)` — refuses to start an LLM session when ask is inside `.claude/worktrees/<name>` (with a `/resume` hint naming `<name>`) or in any subdirectory of a git/jj checkout. Plain checkout roots and non-checkout dirs pass. The chat-facing gate fires from `sendToProvider`, `handleCommand` slash dispatch, Ctrl+M, and silently from `Init`. `validateExecutorCwd(args, root)` is the executor-level defense in `prepareProviderSessionAt`: when worktree mode is on at a real checkout, `args.Cwd` must point inside `.claude/worktrees/`. |
| `clipboard.go`         | `wl-paste` integration, returns raw bytes + re-encoded PNG.             |
| `kitty.go`             | Kitty graphics protocol: detection, transmit over `/dev/tty`, Unicode placeholder rows. |
| `kitty_diacritics.go`  | The canonical 297-entry Kitty row/column diacritic table.               |
| `ask_question.go`      | Question modal state, rendering, navigation, submit/cancel flow.        |
| `workflows.go`         | Workflow runtime tracker singleton + persistence helpers + status broadcast. |
| `workflow_store.go`    | Three-scope workflow persistence: user (ask.json) + repo (`<root>/.ask/workflows/*.json`, committed) + global (`~/.config/ask/workflows/*.json`, machine-local, visible from every project); merged global-first listing (personal-wins), ambiguity-strict name resolution, dir sync on save, cross-scope copy. |
| `workflows_screen.go`  | Workflows builder screen — list/steps/step editor levels with multi-line prompt textarea. `e` on a selected workflow opens that same textarea (workflow-scoped `promptTarget=="description"`) to edit the workflow's free-text Description; it commits to `workflowDef.Description` and shows as the steps-pane subtitle. |
| `workflows_picker.go`  | Small centred modal popped on `f` (issues) / `Ctrl+F` (chat) to pick which workflow to run. |
| `pkg/workflow/compile.go` | `CompileWorkflow` — a `Def` becomes an ADK graph: `AgentNode` per step, `loopagent`+`exitlooptool` (tail step only) per loop, `IncludeContentsNone` + `InstructionProvider` per step agent, per-node `RetryConfig`. Hands each step a `StepRole{InLoop, IsTail, IsFinal}` so the `ToolsBuilder` attaches position-dependent tools. Returns `Compiled` with the agent-name → step-index map the progress adapter needs. |
| `pkg/tools/artifact.go` | `SaveArtifactTool` (writes through `ctx.Artifacts().Save`; ADK ships only `load_artifacts`) and `WorkflowStepTools(env, isFinal)` — save/load on every step for data handoff, `finish_workflow` on the final step for the user-facing outcome report. |
| `pkg/workflow/progress.go` | `Progress` — ADK events → `RunnerListener` callbacks, driven only by events that actually arrived. |
| `cmd/ask/workflow_graph.go` | TUI runner: one session for the whole graph, agent swapped for the compiled workflow. |
| `pkg/engine/workflow_run.go` | Headless runner + `WorkflowGraphAgent` + `IngestWorkflowMemory`. |
| `workflow_source.go`   | `workflowSource` tagged union (issue ref vs chat transcript) consumed by picker / runner / banner. |
| `chat_workflow.go`     | `Ctrl+F` dispatcher — snapshots `m.history` into a chat source, gates on busy/empty, opens the picker. |
| `keymap.go`            | Remappable global shortcuts — `Action` enum, `KeyBinding` parse/stringify, default keymap, `currentKeyMap()` cached accessor. Per-screen keys (kanban `j/k`, modal arrows, Ctrl+D close) stay inline; this only covers the global screen-switch + tab-nav surface. |
| `config_keybindings.go`| `/config → Keybindings...` sub-picker. Enter on a row arms capture mode; the next non-Esc keypress is persisted to `cfg.Keybindings` and the keymap cache is invalidated so the change takes effect immediately. |
| `config_websearch.go`  | `/config → Web Search...` sub-picker. One masked field — the Brave Search API key (`cfg.WebSearch.BraveAPIKey`, env fallback `BRAVE_API_KEY`). Enter on the row opens an inline editor; the draft accepts keystrokes + paste, Enter persists. Same chrome/state-machine shape as the Memory picker. The key feeds the Brave-backed `web_search` tool (DeepSeek/Kimi); anthropic/openai never consult it. |
| `util.go`              | Small helpers (`short`, `humanDuration`, `humanBytes`, `shortCwd`).     |
| `debug.go`             | `ASK_DEBUG=1` → `/tmp/ask.log`.                                         |
| `*_test.go`            | Fast, behavior-only tests. See "Test layout" below.                    |

## Build, verify, install

Use the Makefile for building, testing, and installing ask:

```
make build      # Build the ask binary to bin/ask (includes llama.cpp setup)
make test       # Run all behavioral tests
make install    # Install ask to GOPATH/bin
make clean      # Clean up build artifacts
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
| `workflows_screen_test.go` | Workflows builder state machine — add/rename/delete persistence + edit-while-running guard + description edit (`e` → prompt textarea → Ctrl+S persists `Description`) + loop tree (`stepRows`, add loop/inner, edit max-iters, delete loop/child) + scope copy/move (`c`/`s`, conflict auto-suffix, running guard, per-scope rename). |
| `workflow_store_test.go`   | Three-scope store — filename sanitization, user/repo/global round-trip (Scope never persisted), Description round-trip (persisted under `json:description`, absent ⇒ key omitted), dir sync rename/delete (both repo and global dirs tidy on empty), junk-file skip, global-first resolution + ambiguity errors, cross-scope copy (conflict → new_name, deep clone), `workflowsGlobalDir_NonHome` for empty-home fallback. |
| `workflows_picker_test.go` | Picker open/navigate/Enter dispatches `spawnWorkflowTabMsg`. |
| `workflows_run_test.go`    | Step runner — advance (incl. linear no-`end_turn` re-prompt), finalise, fail, idempotent finalise, unknown-provider rejection; loop decision table (iterate / tail break / non-tail break / non-tail proceed / non-tail + tail no-`end_turn` re-prompt / tail no-decision re-prompt / max-iter soft-exit), enter-loop, bounded context, `stepSummaryLine`, `end_turn` signal handling. |
| `issues_workflow_test.go`  | `f` keybind dispatch on the issues screen — toast / picker / focus-existing-tab. |
| `chat_workflow_test.go`    | `Ctrl+F` chat-source flow — transcript filter, key uniqueness, prompt assembly, dispatcher gates (busy/empty/no-workflows/workflow-tab), end-to-end picker → spawn. |
| `keymap_test.go`           | `ParseKeyBinding` / `KeyBinding.String` round-trip, default keymap coverage, load-from-config (unknown/malformed entries skipped, empty-string unbinds), `currentKeyMap` invalidation. |
| `keymap_dispatch_test.go`  | End-to-end: overridden keymap rewires `tabs.go` tab navigation; `/config → Keybindings` capture persists to disk and re-binding to default deletes the entry. |
| `config_websearch_test.go` | `/config → Web Search` picker — Global row presence, open/edit/commit (Brave key persisted to `cfg.WebSearch.BraveAPIKey`), paste accumulation, masked summary, clear-to-empty, Esc close. |
| `sidebar_test.go`          | Sidebar — geometry (1/5 clamp), scroll-window/card hit-testing, key routing (Tab focus + completion non-theft, Up/Down switch, type-to-return, Esc, Ctrl+Up/Down both modes), focus-steal suppression, card title/meta/cost/activity/badge derivation, view composition + `joinBodySidebar`/`clipText`, workflow supplant (snapshot, tracker, busy refusal) and Enter-restore. |
| `tab_title_test.go`        | Tab titles — fallback/sanitize (think-tag strip, quote/period trim, clip), `maybeStartTabTitle` gating (workflow tab / blank / already titled), swapped-generator cmd round-trip incl. error swallow, `tabTitleMsg` handler (foreign tab, empty title, stale-after-/new), VS persistence + `recordVirtualSession` backfill + /resume rehydration (Title, Preview fallback). |
| `agent_run_test.go`        | ADK runner loop tests + runtime scenarios: tool round-trip incl. wire history threading, interrupt = clean end, error turn, shutdown, loop-detection trip, compaction. |
| `agent_tools_test.go`      | Tool behaviors on `t.TempDir()`: read windows/caps/rejections, write/edit guards (read-before-mutate, stale mtime, uniqueness, replace_all, CRLF), glob/grep/ls, bash via swapped `agentRunShell` (output, exit codes, cancel-kills-pgroup, background jobs), approval denials (`StopTurn`), fetch via httptest, Brave `web_search` (no-key notice, empty-query reject, token/query/count headers + config-beats-env via swapped `braveSearchClient`, HTML-stripped results, HTTP-error result, approval `StopTurn`), todos validation, the two-stage workflow guard (todos-only: fire/disarm-by-check/disarm-by-run/inert), the require-todos-before-mutate gate (edit + write refused until a todos list applies, then it lands; fires in workflow-less projects too), required description-phrase schema across the coding core. |
| `agent_tools_ask_test.go`  | Native ask_user_question/end_turn — message shapes via swapped `agentSendToProgram`, cancelled/headless replies. |
| `agent_tools_mcp_test.go`  | MCP manager against in-process `mcp.Server`s over httptest — attach/skip/schema/IsError, image results (placeholder vs media by vision), unreachable-server skip, `tools/list_changed` live refresh, dead-server graceful error, elicitation schema mapping + accept/cancel/headless/url flows. |
| `mcp_servers_test.go`      | Server-config resolution — effectiveType inference, `${VAR}`/`${VAR:-default}` expansion (copy semantics), 3-layer merge (.mcp.json ← global ← project) incl. Disabled tombstones + junk drops + stable order, tool allow/deny filters. |
| `mcp_oauth_test.go`        | OAuth plumbing — token path/0600 round-trip, persisting token source saves on change, callback listener captures code/state via swapped browser opener, stored-valid-token served without a flow, fresh handler yields nil source (transport 401s into Authorize). |
| `agent_tools_bridge_test.go`| Native linear twins — 12-tool coverage check, jsonschema field-doc fidelity, description-phrase injection (+ payload-description non-clobber), linear gate error, malformed input, loopback never in `agentSessionMCPServers`, every linear tool's wire schema is free of `anyOf`+`items` conflicts (Normalize-shape regression check). |
| `agent_tools_workflow_test.go`| Native workflow core tools — 6-tool coverage check, workflow CRUD round-trip against project config, workflow Description round-trip (create sets it, list/get surface it, edit replaces it, omitted leaves it unchanged across a rename), the workflow-guard disarm hooks (calling `workflow_list` sets `workflowsChecked`) so the two-stage todos guard clears on the direct path, plus the wire-schema shape tests (`flattenNullableTypes` table + Normalize-shape check) that pin `workflow_create.steps` / `workflow_edit.steps` / `workflow_edit.description` to the single-type shape strict validators accept. |
| `agent_tools_registry_test.go`| Tool registry — search query forms (`*`/prefix/substring, schema fidelity, sorted, no-match name list, empty registry), invoke dispatch (identity + params JSON), replicated required-field check, phrase injection (natives yes, MCP no), unknown/core-name errors, response pass-through (IsError/StopTurn/image/hard error), `unwrapInvokeToolCall`, `refreshToolset` wire/registry split (decorateTools sees core only), session surface (linear_* in the registry, `workflow_*` on the wire), `web_search` backend selection (no native spec → Brave on the wire + nil `providerWebSearch`; native spec → off the wire + `providerWebSearch` set), end-to-end fakeLM unwrap (toolCallMsg/toolResultMsg/status), loadHistory replay unwrap. |
| `skills_test.go`           | Skills — discovery validation (bad name / dir mismatch / no description skipped) + project-over-global precedence, trigger block (progressive disclosure, hidden skills), `/name args` expansion incl. user-invocable gating, frontmatter parser, ProbeInit → slash entries. |
| `pkg/plugin/plugin_test.go` | Marketplace format — manifest shapes (string/object sources, `PathList`, `Author` string), `ParseMarketplaceSource` forms, directory marketplace add/install/uninstall/remove (cache copy, `installed_plugins.json` record, `strict:false` bare-skill entry, disable, installed-vs-enabled lenses), project scope (`.ask/plugins.json`, Missing on another HOME, `SyncProject`), git-backed marketplaces + remote `git-subdir` installs via swapped `RunGit` (clone/pull/checkout, sha version, no temp left behind, clone removed with the last registration), publish (files, plugin.json, lossless entry upsert, round-trip install, version bump, git add/commit/push, read-only url marketplaces), `InitMarketplace`, `ImportFromClaude` (state read, skip-present, unknown-marketplace plugin reported), `ResolveContents` shapes and path-escape rejection, `ParseRef`. |
| `pkg/engine/skills_plugin_test.go` | Plugin origins — namespaced `plugin:skill` names, command files as skills (with and without frontmatter), `$ARGUMENTS` substitution, bare names resolve local over plugin, generation-bump rescans a live source, skill/agent store CRUD (quoted descriptions, scope ambiguity, flag set/clear, delete), plugin items read-only, plugin agents. |
| `pkg/tools/extensions_test.go` | Extension tools — 14-tool coverage + off-the-wire check, skill CRUD round-trip with `ExtensionsChangedEvent` per mutation, agent CRUD, marketplace add/search/install (project scope writes `.ask/plugins.json`, installed plugin skills/agents discoverable, plugin workflow carries provider warnings) / uninstall, approval denial on the gated tools, publish of a skill, an agent, and a workflow, `WorkflowProviderWarnings`. |
| `pkg/workflow/store_plugin_test.go` | Plugin-scope workflows — listed read-only from installed plugins, resolvable by name/scope, never written back by `SaveAll`, copyable into an editable scope; `ExportFile` shape. |
| `skills_browser_test.go`   | Browser — open/close via Ctrl+S, `/skills`, Esc/Ctrl+C, modal gate, slash-menu entry; installed lens grouping + kind tags + provider warnings + type-to-search; Enter inserts `/name ` (agents: delegation prompt); marketplace lens rows, filter, drill/undrill; install flow (Ctrl+G scope prompt → async op → rebuilt rows, `.ask/plugins.json`, `extensionsChangedMsg` re-probes slash commands) and Delete-uninstall with confirm; Ctrl+A add-marketplace editor + paste + `/skills add|remove marketplace`; Delete confirm and Ctrl+P publish (agent + workflow) into the writable marketplace; Ctrl+N/Ctrl+E hand-off prompts, a plain letter only types into the search, plugin items refuse edit and Delete offers uninstall, Ctrl+P is never list-nav; overlay geometry (picker footprint, wide-and-flat, square corners) + paste + PgDn; Import from Claude (state summary, registers + installs, never writes `~/.claude`); workflow launch refused with a toast when a step's provider is not configured; `extensionsChangedMsg` broadcast re-probes every tab. |
| `rules_test.go`            | `.claude/rules/` — `paths` frontmatter parsing (no-frontmatter/no-paths eager, block + inline list, brace verbatim, key-terminated list), eager/match split, recursive discovery + project-over-user precedence + non-md/empty-body skip, `rulesPromptBlock` (eager only, path attr), `ruleAwareTool` JIT injection + once-per-session dedup + non-match miss + eager exclusion, no-scoped-rules passthrough, `relPath` outside-root rejection. End-to-end eager block in `agent_prompt_test.go`. |
| `agent_subagents_test.go`  | Subagents — def discovery/precedence/field parsing, tool grant sets, spec registry, claude model aliases, cross-provider model resolution (swapped LM var), task tool: named agent runs on the pinned provider w/ def prompt + report tail, background job lifecycle (bgTask signals, job_output), default researcher unchanged, `/skill` expansion reaches the wire. |
| `agent_session_test.go`    | Store round-trip (typed parts survive), CreatedAt preservation, list ordering, LoadHistory tool-output modes, Materialize via ADK FileSessionService. |
| `agent_prompt_test.go`     | Prompt assembly: env block, context-file discovery/dedupe/cap, determinism, steering tail, worktree clause. |
| `pkg/diff/diff_test.go`    | `Unified` diff and `Parse` tests, including Myers budget fallback, interleaved edits, and EOF newline handling. |
| `model_picker_test.go`     | Ctrl+M picker — open/seed (incl. busy gate), header-skipping nav + wrap, fuzzy filter (provider name + friendly name + id), natural sort within a provider, recents first in recency order, catalog-load rebuild keeps the cursor, open dispatches the load once (+ Ctrl+R force), apply semantics (cross-provider clears session, same-provider keeps, tab-local), API-key gate (prompt/save/esc/env-or-config skip), custom-id editor, paste routing, overlay geometry (margins, square corners, divider, editors keep the footprint), scroll window, app-level compositing over the sidebar, detail pane (catalog hit vs “no information available”, scroll clamp), `modelDetailFacts`, `formatUSDPer1M`, friendly-name + fuzzy helpers, `missingAPIKeyError`. |
| `model_catalog_test.go`    | Catalog load — listings + errors collected per provider (non-listers skipped), cache + notes installed before the msg, single-flight + retry-while-models.dev-fails, `agentAPIProvider.ModelPicker` serves the cached listing and falls back to the static ids. |
| `vertex_test.go`           | Vertex AI spec — metadata/registry (incl. Claude-filtered picker + no key-prompt), effort mapping, no-key fail-fast (no project), SA-key auth via `vertexPrepareCredentials` seam (path validation, env mutation, env fallback, ADC discovery), full session lifecycle, Materialize, store root, `vertexModelOptions_FiltersClaude`. |
| `config_vertex_test.go`    | `/config → Vertex AI` picker — global row presence, project/location regex validation, SA-key tilde + readable-file validation, persistence, paste accumulation, invalid input keeps editor open, summary state ("off" / "<project>/global"). |
| `pkg/providers/catalog_test.go` | Static catalog lookups — model hit/miss, default-first id list, window/image fallbacks, effort clamping (down to nearest, up from below-range). |
| `pkg/providers/model_meta_test.go` | `ModelMetaFor` layering — static only (list price, no invented description), models.dev overrides static but a sparse layer never erases lower data, OpenRouter live wins over models.dev, live-only ids resolve, unknown everywhere misses, `StepCostUSD` catalog vs live pricing, `perTokenToPer1M`. |
| `pkg/providers/modelsdev_test.go` | models.dev client against httptest — fetch + parse + disk cache write, fresh cache skips the network, stale cache refetches and rewrites, stale fallback on a failed fetch, no-cache failure errors, payload without mapped providers rejected and not cached, not-loaded misses. |
| `cost_test.go`             | Session cost meter — `stepCostUSD` per-model list pricing (Flash cheaper than Pro, unpriced catalog model stays unpriceable, live/models.dev pricing via the `ModelMetaLookup` seam), `formatUSD`, usageMsg/costMsg/tabTitleMsg accumulation + foreign-proc/tab gating, resets (/new, /clear, cross-provider swap keeps same-provider), task-tool sub-agent cost emission, sidebar cost row derivation. |
| `util_test.go` / `paths_test.go` | Pure helpers, path completion, frontmatter parsing.       |

### Testing conventions

- **Every new feature must include comprehensive tests.** Do not write or merge features without end-to-end or unit tests that verify their behavior. A PR without comprehensive tests is incomplete.
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

A workflow lives in one of three scopes (`workflow_store.go`):

- **user** — `projectConfig.Workflows` alongside `projectConfig.Issues`
  in `~/.config/ask/ask.json` (machine-local, the pre-scope default).
- **repo** — one JSON file per workflow under
  `<projectRoot>/.ask/workflows/` (committed, shared with the team).
  `projectRoot` walks to the main checkout, so every worktree/subdir
  sees the same files — consistent with projectConfig tenancy.
- **global** — one JSON file per workflow under
  `~/.config/ask/workflows/` (machine-local, visible from every
  project — a personal toolbox that follows the user between
  checkout roots).

`workflowDef.Scope` is runtime-only (`json:"-"`) — the storage
location IS the scope, so every on-disk shape is byte-identical to
pre-scope workflows. Every read merges in the order global → repo →
user (`listAllWorkflows`; name-only lookup prefers global, the
personal-wins convention). Names are unique *within* a scope; the
same name across multiple scopes is legal — UI surfaces show a scope
tag, and the mutating tools refuse to guess (`resolveWorkflowByName`
errors on ambiguity without an explicit scope). All mutations funnel
through `mutateWorkflows`/`saveAllWorkflows`, which split the merged
list by scope and sync both dirs (write changed files, remove stale
ones — rename = delete+add, VCS-friendly) under the config lock.
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
| `workflowDef{Name, Description, Steps, Scope}` | One named pipeline; Scope is runtime-only. Description is the author's free-text statement of what the workflow is FOR and when to use it — surfaced verbatim in `workflow_list` so the agent judges fit against stated intent instead of guessing purpose from step names (the real failure that motivated it: a `ship` workflow misread as feature-only and wrongly declined for a deletion refactor). `omitempty`, so pre-description workflows stay byte-identical on disk; editable in the builder via `e` and through `workflow_create`/`workflow_edit`. |
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

### Execution: the ADK workflow graph

A `workflowDef` compiles to an ADK workflow graph
(`pkg/workflow/compile.go`, `CompileWorkflow`). There is exactly ONE
workflow engine — the handwritten `Runner.Run` state machine, plus the
two dead parallel implementations (`RunGraph`, `BuildWorkflowAgent`),
were deleted. Don't add a second one.

- A top-level agent step becomes an `AgentNode` wrapping an `llmagent`;
  nodes are chained `Start -> n0 -> n1 -> …`.
- A `kind: "loop"` step becomes an `AgentNode` wrapping ADK's
  `loopagent`, whose sub-agents are the inner steps. **Only the tail
  (last) inner step carries `exitlooptool`**, so only it can break the
  loop early: `exit_loop` sets `Actions.Escalate` (what `loopagent`
  watches), no other ask tool touches `Actions`, so withholding the tool
  from the non-tail steps makes an early break structurally impossible
  for them — this replaces the old `end_turn` decision guard, which
  caught the violation after the fact and re-prompted. Otherwise the loop
  runs to `MaxIterations`. There is no `decision` argument — loop control
  is ADK's tool, not an `end_turn` field.
- `NodeConfig.RetryConfig` gives per-node retry, replacing the runner's
  hand-rolled `stepErrorRetry` loop.

`CompileWorkflow` hands each step a `StepRole{InLoop, IsTail, IsFinal}`
via its `ToolsBuilder`, which is what decides the position-dependent
tools: `IsTail` gates `exit_loop`, `IsFinal` gates `finish_workflow`.

Two `llmagent` settings carry the semantics and MUST NOT be dropped:

- **`IncludeContents: IncludeContentsNone`** is what isolates a step.
  Without it a step inherits the whole session, and ADK's
  `ConvertForeignEvent` renders every prior step's events as prose —
  every tool call and every full tool result — so step 3 would carry
  steps 1 and 2 in full. With it, a step sees the handoff from the step
  before it plus its own work.
- **`InstructionProvider`, never `llmagent.Config.Instruction`.** Step
  prompts are user-authored and routinely contain braces; ADK
  interpolates the static `Instruction` field against session state and
  hard-fails the invocation on the first unknown `{name}`.

Agent names are sanitised and de-duplicated by the compiler
(`agentNamer`): ADK requires them unique within a graph and rejects
`"user"`, while step names are free text and never checked.

`*workflow.Workflow` is NOT an `agent.Agent` — the interface has an
unexported method — so `engine.WorkflowGraphAgent` wraps it via
`agent.New(agent.Config{Run: wf.Run})`.

### Running a workflow

One agent session runs the whole graph, not one session per step.

- TUI: `cmd/ask/workflow_graph.go` starts a single session, swaps its
  agent for the compiled graph (`agentSession.workflowAgent`), and
  queues one turn. Tool execution, approvals, cost accounting, and
  cancellation therefore behave exactly as in a chat turn.
- Headless: `engine.RunWorkflow` (`pkg/engine/workflow_run.go`) does the
  same against its own `session.InMemoryService`.

### Progress reporting

`pkg/workflow/progress.go` turns the ADK event stream into
`RunnerListener` callbacks, and `agentSession.workflowProgress` feeds it
from inside the runner loop. Every callback is driven by something that
actually happened: a step starts when an event authored by its agent
arrives, and finishes when its successor starts or the run ends cleanly.
`Compiled.StepIndexByAgent` maps an event author back to the top-level
step, so a loop's inner agents report against the loop step the user
wrote.

**Never fabricate step events.** The deleted `RunGraph` closed out every
step that never ran as both started AND done and hardcoded a successful
`FinishData`, so a chain that died at step 1 of 5 rendered 5/5 green.
`TestProgress_FailureDoesNotFabricateRemainingSteps` pins this.

### Per-step `end_turn` reporting

Every step should call `end_turn` once with a `summary` (1-3 sentences);
it becomes the step's line in the workflow log via `stepSummaryLine`.
A step that ends without it is NOT re-prompted any more — the whole
remind/re-prompt machinery is gone — its log line falls back to the first
line of its own output.

`finish_workflow` reports the run's outcome and the artifacts it produced
(PR links, tickets) to the user. It is attached to the **final step
only** (`WorkflowStepTools(env, isFinal)` in pkg/tools/artifact.go);
`Progress` captures the call straight from the event stream
(progress.go), and the TUI also reads `env.PendingFinishData` as a
backup. Nothing attaches it otherwise — an earlier regression left it
attached to no step at all once the graph became one session.

### Passing data between steps (artifacts)

A node's text output is the implicit handoff to the next node. For
structured data — a plan, a diff, notes — a step calls the native
`save_artifact` tool; a later step loads it by name with ADK's
`load_artifacts`. Both are attached to every workflow step by
`WorkflowStepTools`. This works because the whole graph runs as ONE
runner invocation, so the runner's `ArtifactService` (set in
`RunnerBuilder`) spans every step — a save in step 1 is visible to a load
in step 3. ADK ships `load_artifacts` but no save tool; `SaveArtifactTool`
(pkg/tools/artifact.go) is the missing half, writing through
`ctx.Artifacts().Save`. This is the ADK-native replacement for the old
`ask/plans/` notes directories.

### Step instruction assembly

`workflow.BuildStepInstruction(step, source, pc)` produces a step's
system instruction:

```
<step.Prompt>

Reference: <owner/repo#N>        (or the chat transcript block)

<end_turn contract>              (+ loop framing and exit_loop guidance inside a loop)
```

Previous-step output is deliberately absent. Threading it into the prompt
was the old runner's job; the graph passes a node's output as the next
node's input, and `IncludeContentsNone` is what lets the step see it.

### No notes directories

`ask/plans/` is gone, along with `plans.go`, `clear_plans`, and the
`RemindFixPlanDir` re-prompt. Step-to-step handoff is the graph's node
output plus ADK artifacts (see above). Durable "what we learned" goes to
`pkg/memory` — `RunWorkflow` calls `engine.IngestWorkflowMemory` on a
clean finish, which is what the notes directories were badly
approximating.


### Builder screen (`Ctrl+W` / `/workflows`)

`workflows_screen.go` is a top-level screen (`screenWorkflows`,
peer of ask/issues). State lives on `m.workflowsBuilder` and uses
the standard `renderLayeredConfigBox` chrome for visual parity
with `/config`. Three navigation levels:

| Level | Cursor over | Keys |
|-------|------------|------|
| List (left pane) | workflows (with user/repo/global scope tags) + "+ New" | enter open / r rename / c copy / s move (rotates user→repo→global→user) / d delete / esc back to ask |
| Steps (right pane) | the step tree + affordances | enter edit/create / d delete / tab focus left / esc list |
| Step (right pane) | agent: Name/Provider/Model/Prompt · loop: Name/Max iters/Exit | enter edits the field / esc back to steps |

The steps pane is a **flat list of navigable rows** derived from the
step tree (`stepRows()` — `stepsCursor` indexes it, the single source
of truth). Row 0 is "+ New step"; loops render as a `⟳` header with
`▏`-railed indented inner steps and a trailing "+ add step"; the last
row is "+ New loop". Enter on an affordance creates a step/loop/inner
step and drops into its detail; Enter on a real step/loop opens its
detail. The detail pane branches on kind: an agent step shows a
builder-local Provider picker, the shared full-frame model picker for
the Model field, and a Prompt `textarea`; a loop shows an inline
numeric Max-iterations editor (`renaming=="maxiter"`) and an Exit-
condition `textarea` (the textarea is shared, `promptTarget` says
which field it commits to). Every commit writes to disk immediately.
The Model field opens the **same** Ctrl+M model picker
(`openModelPickerForStep` → `modeModelPicker`), not a builder-local
overlay: `modelPickerState.stepTarget` retargets its terminal action
from the live-tab switch to `applyModelPickerToStep`, which writes
BOTH `step.Provider` and `step.Model` (a picker entry is
provider-grouped, so it always carries both) and returns to the
builder. This subsumes the old "Enter your own" custom-model path —
the shared picker's own custom-id entry handles it.

### Edit guards

A workflow that's currently running anywhere in the process
(`workflowTracker().activeWorkflowNames()`) is locked against
rename / delete / step edits — the builder shows a dim
"blocked: workflow is running" toast in the help row. Once the
run finalises (or the tab closes), the lock releases.


## Skills, agents, plugins, and marketplaces

One distribution format: the **Claude Code plugin marketplace**
(`pkg/plugin`). A marketplace is a git repo (or directory, or a bare
`marketplace.json` URL) with `.claude-plugin/marketplace.json`; a
plugin is a directory with `.claude-plugin/plugin.json` plus
`skills/`, `agents/`, `commands/` (single-file skills), and ask's
`workflows/*.json`. `strict:false` entries let a bare SKILL.md
directory be distributed without a plugin.json (how
`anthropics/skills` ships). A plugin ask publishes installs in Claude
Code unchanged; Claude Code ignores the `workflows/` dir and the
`provider:` key on agents.

Scopes and state (ask-private — nothing here overlaps Claude Code's
own state, which is only ever read by the explicit *Import from
Claude*):

| What | user | project |
|------|------|---------|
| new skills / agents | `~/.config/ask/skills`, `~/.config/ask/agents` | `<root>/.ask/skills`, `<root>/.ask/agents` (committed) |
| marketplaces | `~/.config/ask/plugins/known_marketplaces.json` | `<root>/.ask/plugins.json` `marketplaces` |
| enabled plugins | `~/.config/ask/plugins/installed_plugins.json` (scope `user`) | `<root>/.ask/plugins.json` `enabled` |
| plugin copies | `~/.config/ask/plugins/cache/<mkt>/<plugin>/<version>/` (machine-local; `SyncProject` fetches what the project file names) | |

Discovery (`engine.SkillSearchRoots` / `SubagentSearchRoots` +
`plugin.EnabledPlugins`): user dirs, then project dirs (project wins
on a bare-name clash), then every enabled plugin. Plugin items are
namespaced `plugin:name` (Claude Code's rule) so they never clash;
`Skill.Origin` / `SubagentDef.Origin` / `workflow.Def.Plugin` carry
provenance, and plugin copies are read-only (`Origin.Editable`,
`workflow.ScopePlugin`; the builder's `runningGuard` blocks edits,
`c` copies into an editable scope). The ADK skill source rescans
whenever `BumpSkillsGeneration` fires, so a skill created mid-session
is on the model's next request; `extensionsChangedMsg` re-runs
`ProbeInit` so its slash command registers at the same moment.

Model-facing: the `skill_*` / `agent_*` / `marketplace_*` /
`plugin_*` / `skill_publish` tools live in the deferred registry
(`pkg/tools/extensions.go`) — "make a skill from this conversation"
is `skill_create` + iteration with `skill_edit`; the file is the
draft. `marketplace_search` is the on-the-fly path: the model finds
a fitting plugin, proposes `plugin_install`, and the approval modal
gates the install. Catalogs never go on the wire.

**Publishing is a link, not an install.** `Publish` copies the local
item into `<mkt>/plugins/<name>/` and records a `Publication` (kind,
name, scope, marketplace, plugin, file, version, content hash) — in
`.ask/plugins.json` for project items, `~/.config/ask/plugins/published.json`
for user items. The local copy stays the source of truth; the
marketplace row reads `✓ yours` and refuses install. `plugin.Status`
compares the local hash and the marketplace copy's hash against the
recorded one: in sync / local changes (Ctrl+P publishes an update,
patch version bumps) / marketplace newer (Ctrl+U pulls it, `skill_pull`)
/ diverged (either, after a confirm) / missing. Git-backed
marketplaces are pulled (ff-only), committed, and pushed on every
publish unless `NoPush`.

Workflows shipped by plugins run like any other (`f` / Ctrl+F), but
the launch path (`supplantWorkflow`) refuses a workflow whose step
provider has no credentials (`providers.ProviderConfigured`, the
`Configured` hook on `AgentProviderSpec`) with a toast telling the
user to switch that step's model; the browser shows the same ⚠ on
the item.

## Sidebar tab mode

The sidebar is the only tab mode (the bottom bar has been removed).
It is a permanent right-hand column owned by the app layer (`sidebar.go`).
Width is ~1/5 of the terminal clamped to [30,48] cols. The column is a
pure projection of `a.tabs`: the selection cursor IS `app.active`, the
scroll offset is derived, and every card reads live model state
(title, provider/model, accumulated session spend in USD, in_progress
todo / stream status / workflow step, ⚠ needs-input · ✓ done · ✗
failed · ● busy badges) at render time. The cost row is fed by
`model.sessionCostUSD`: usage.go’s `stepCostUSD` prices every API call
through `providers.StepCostUSD` against the same layered model metadata
the picker shows (`ModelMetaFor`: static list price → models.dev → the
provider's live listing; crush’s formula — cache writes at the
cache-write rate, cache reads at the cached-input rate) and
the meter counts main-loop steps (`usageMsg`), task sub-agents and the
compaction summarizer (`costMsg`), and the tab-title call
(`tabTitleMsg`). Unpriceable models (custom ids / no catalog) render
an empty row, never a fake $0.00; the meter resets with the
conversation (/new, /clear, /resume pick, cross-provider swap) and
survives same-provider model swaps.

Keyboard: `ActionSidebarFocus` (default Tab) swaps focus between the
typing area and the list — intercepted at the app layer only when the
active tab reports no local use for Tab (`model.wantsTabKey`: false on
an idle chat input, true for modals, non-ask screens, completion
popovers, inline confirms). While the list is focused, Up/Down (and
j/k) switch the active tab immediately — no Enter — typing any
printable rune bounces focus back into the input and types it, Esc /
Enter / Tab return, Ctrl+D closes the selected tab, everything else is
absorbed. `ActionTabPrevAlt`/`ActionTabNextAlt` (Ctrl+Up/Down) switch
tabs from anywhere. Mouse clicks on a card switch to it; wheel events
over the column are absorbed.

- **No focus theft.** `dispatchByTabID` never force-focuses a tab
  whose ask/approval modal fires — the request parks on the tab's
  modal state and the card shows the ⚠ badge until the user switches.
- **Workflows supplant instead of spawning.** `spawnWorkflowTabMsg`
  routes to `app.supplantWorkflow`: the run attaches to the origin tab
  (busy/shell-streaming/already-running tabs refuse with a toast — no
  queue), a `workflowTabSnapshot` captures the provider/session state,
  the tab flips to the read-only banner, and when the chain finishes
  Enter restores the conversation (`restoreSupplantedTab`) with the
  step summaries left in the transcript. Closing the tab mid-run still
  marks the run failed.
- **Tab titles.** The first prompt seeds `m.tabTitle` instantly and a
  one-shot LLM call (`tab_title.go`) refines it in the background;
  titles persist on the VirtualSession and rehydrate on /resume.


## In-process API providers (ADK 2.0 & Vertex AI)

`vertex` runs with **no CLI subprocess**:
the agent loop runs inside ask, built on Google ADK 2.0 (`google.golang.org/adk/v2`)
and `google.golang.org/genai`.

### Provider seam

ONE generic implementation (`agentAPIProvider`, agent_provider.go)
satisfies the `Provider` interface for every spec, with
`providerProc.cmd == nil`: StartSession spawns a goroutine
(`agentSession.run`), `stdin` is an adapter whose `Close()` tears the
session down (that's what `killProc` calls), and `Interrupt` cancels
the in-flight turn's context cooperatively (handled=true, codex-style —
the session emits its own turn end). A provider is an
`agentProviderSpec` value (`pkg/providers/vertex.go`):
identity, model/effort options, `BuildModel` (via ADK's `gemini.NewModel`),
`CallOptions` (effort→wire), image capability, context window,
and config-block accessors. Registration order is explicit in
provider.go's single `init()`.
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

**Vertex AI** rides the `google` provider via `google.New(google.WithVertex(project, location))` through ADK 2.0. Auth uses Google Cloud ADC: explicit `cfg.Vertex.ServiceAccountKey` (or `$GOOGLE_APPLICATION_CREDENTIALS` env) → `gcloud auth application-default login` creds → GCE metadata. The `vertexPrepareCredentials` helper validates the SA-key path and uses `genai.ClientConfig{Credentials: ...}` directly instead of mutating the process environment. Project is required (fail-fast at session start); Location defaults to `global`. Default model is `gemini-3.7-flash`.

Model metadata (context windows, max output, image capability, reasoning levels, pricing, descriptions) comes from the layered lookup in `pkg/providers/model_meta.go` — the static catalog (catalog.go), models.dev (modelsdev.go), and the provider's live listing — never from the Vertex API itself, which publishes nothing beyond the model id.

### Model metadata precedence

This is the standing policy for EVERY provider, current and future
(`ModelMetaFor` + `mergeProviderNative`, pkg/providers/model_meta.go):

- **Descriptions: models.dev first, the provider's own text as the
  fallback.** models.dev's descriptions are complete one-liners written
  for this purpose; provider text is either truncated server-side
  (OpenRouter — 368 of its 421 descriptions end in "...") or absent
  (Vertex). A provider's description is used only for a model models.dev
  does not describe.
- **Every other fact — limits, pricing, modalities, effort levels,
  dates — the provider's live listing wins over models.dev, which wins
  over the static catalog.** The provider is authoritative for what it
  serves and bills; models.dev fills what it doesn't publish; the static
  catalog is the offline floor.
- A new provider's native layer is merged through `mergeProviderNative`
  so it inherits both rules without re-deciding them. Do not special-case
  a provider's description as "better" — if models.dev is wrong, fix it
  upstream (community TOML, MIT). The system prompt is evaluated per-invocation using ADK\'s dynamic `InstructionProvider`.

### Tools

The surface is two-tier (`setupAgentSessionTools`,
agent_provider.go):

- **Core (wire)** — the tools sent in the API tool definitions
  every turn: read/write/edit/glob/grep/ls/bash+jobs/fetch/todos/
  task, the modal pair `ask_user_question`/`end_turn`
  (agent_tools_ask.go), the ask-built-in workflow tools
  (`workflow_list`/`workflow_get`/`workflow_create`/`workflow_edit`/
  `workflow_delete`/`workflow_copy` in agent_tools_workflow.go), and the registry pair
  `search_tools`/`invoke_tool` (agent_tools_registry.go). The
  workflow tools are a deliberate, documented core exception — see
  "Tool registry vs core tools" below.
- **Deferred registry** — everything else: the linear_* native
  twins (agent_tools_bridge.go, same cores as the bridge handlers)
  and every MCP tool (the project GitHub MCP plus user-configured
  servers, mcp_servers.go, as `mcp__<server>__<tool>`). Registry
  tools are registered and callable but never part of the wire
  tool definitions: the model discovers them via `search_tools`
  (name + description + full input_schema) and calls them via
  `invoke_tool`, which dispatches straight to the inner tool's
  `.Run` and returns its response verbatim (IsError/StopTurn/image
  all pass through).

The loopback bridge is never attached in-process. Memory recall is
injected natively (agent_memory.go) at three points: the system
prompt at session start, the wire prompt per user turn, and a
per-file footer on read/edit/write results. Permissions: when
`SkipAllPermissions` is off, mutating tools (bash beyond the safe
read-only list, edit/write, fetch) block on the existing approval
modal; denial returns an error result with `StopTurn` so the model
ends its turn instead of retrying. Registry tools carry no approval
gate of their own — `invoke_tool` adds none, matching direct-call
behavior.

### Tool contracts: typed structs in, typed structs out

Every tool is built natively with ADK's `functiontool.New[TArgs, TResults]` over two real structs — ADK derives BOTH the input and the output JSON schema from them automatically using `jsonschema-go`.

```go
type ReadParams struct { FilePath string `json:"file_path" jsonschema:"..."` }
type ReadResult struct {
	Content    string `json:"content,omitempty" jsonschema:"..."`
	Lines      int    `json:"lines,omitempty"`
	NextOffset int    `json:"next_offset,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

func ReadTool(env *ToolEnv) Tool {
	return NewTypedTool("read", ReadToolDescription,
		func(ctx agent.Context, p ReadParams) (ReadResult, error) { … })
}
```

Rules, all pinned by `pkg/tools/contract_test.go`:

- **Both sides typed.** No `map[string]any`, no `any` as TResults.
- **`agent.Context`, never `context.Context`.** A plain context has to be converted to a fake, and the fake drops important ADK features (State, Actions, Escalation, Confirmation).
- **Failures are Go errors.** `return ReadResult{}, fmt.Errorf(...)`, not an error field. ADK's `OnToolErrorCallback` keys on a real error, which lets ADK reflect and retry.
- **One `Run` shape.** Tool handlers are seamlessly adapted to ADK without custom wrappers.

`NativeBridgeTool[In, Out]` wraps its handler's output in `BridgeResult[Out]{Content, Data}`, so the linear_* and workflow_* tools carry their real output struct too.

**Reading a tool result.** `engine.ToolResultText(resp)` finds the human-readable field for any tool (preference order in `toolResultTextFields`) and reports whether the result is an error. Every UI path goes through it.

### Tool registry vs core tools

**New tools go into the deferred registry, NEVER into the core list
in `setupAgentSessionTools`.** Every core addition costs context
tokens on every call of every session, and churns the wire toolset
that anthropic's cached tool block depends on (`refreshToolset` keeps
the wire set byte-stable for the whole session precisely because MCP
`tools/list_changed` refreshes only touch the registry). A tool earns
a core slot only by deliberate, documented exception, and the bar is
"the agent cannot function without seeing it unprompted" — the
registry pair itself, `end_turn` (the workflow runner's per-step
contract), `fetch`, `web_search`, and the ask-built-in `workflow_*`
tools are the canonical examples. The
exceptions split into two flavours: `web_search` and the
ask-built-in workflow tools are the *content* exceptions — an agent
cannot function without seeing them unprompted. `web_search` has
two backends behind one name: providers with first-party search
(anthropic, openai) register a provider-executed tool via the spec's
`nativeWebSearch` + `WithProviderDefinedTools` (it streams through
`OnToolCall`/`OnToolResult` as `ProviderExecuted` so the transcript
renders it like any other tool, and it is NOT in `coreTools`);
everyone else (DeepSeek, Kimi, any openaicompat backend) gets the
Brave-backed native core tool, which degrades to a graceful "not
configured" notice without a `BRAVE_API_KEY`. The ask-built-in
workflow tools (agent_tools_workflow.go) are promoted because the
two-stage workflow guard in agent_tools_todos.go forces the model
to call `workflow_list` as a precondition for
multi-step work, and an extra `search_tools` round-trip on every
guard interaction is pure overhead. The workflow-guard disarm hook
(`env.markWorkflowsChecked`) lives
in the `workflow_list` tool closure itself
so the guard clears on the direct call — they are NOT in
`invoke_tool` anymore (and a model that still routes them through
`invoke_tool` gets the existing "core tool — call it directly"
error). To add a registry tool, append it to `s.deferredBase`
(native) or expose it from an MCP server; it becomes searchable and
invokable with zero wire cost. The extension tools
(`pkg/tools/extensions.go`) are the canonical registry-only set.

Plumbing invariants worth knowing before touching this area:

- `invoke_tool` replicates fantasy's required-field validation —
  fantasy only validates wire tools, and `json.Unmarshal` would
  silently zero-value a missing param otherwise.
- The invoke-level `description` phrase is injected into the inner
  params only when the inner tool *requires* `description` (the
  natives); MCP tools never get unexpected keys.
- `invoke_tool`/`search_tools` are invisible as constructs: the live
  emit (agent_run.go OnToolCall/OnToolResult) and history replay
  (agent_session.go loadHistory) both unwrap via
  `unwrapInvokeToolCall`, so the transcript, status line, and resumed
  sessions all show the real registry tool.

**Description phrases.** Every native tool — the coding core, the
modal pair, the registry pair, and the bridge twins (injected
generically by `nativeBridgeTool`, skipping inputs whose own
`description` is real payload like linear_create_issue's Markdown
body) — takes a *required* `description` param: a model-authored
phrase (under 10 words) saying what the call is doing. The
claude-code/crush trick: the model writes the headline itself in the
same tool call, no second summarization pass. The UI renders it as
the call headline (`▸ bash — Looking for the latest files`,
tool_output.go) — in short mode (default) the phrase IS the whole
entry; full mode adds the param rows — and as the streaming status
(`bash: Looking for the latest files`, agent_run.go).
`toolCallPhrase` gates what qualifies (single line, ≤120 chars) so
payload `description` fields on MCP/bridge tools never masquerade as
the headline; calls without a phrase (old transcripts, MCP tools)
fall back to the `shortToolFields` allowlist, keyed by the native
lowercase names.

### Sessions & turn hygiene

Transcripts persist as ADK message arrays (typed parts survive
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
gone — its tools are native fantasy tools (agent_tools_ask.go for
the modal pair, agent_tools_bridge.go for the linear_* twins, and
agent_tools_workflow.go for the ask-built-in workflow_* tools on
the core wire toolset) and the question modal is driven by
`askToolRequestMsg` (ask_wire.go) directly.

## Conventions

- No new runtime dependencies without asking. We already carry Charm (bubbletea/bubbles/lipgloss/glamour/ultraviolet), Google ADK 2.0 (`google.golang.org/adk/v2`), Google GenAI (`google.golang.org/genai`), the official MCP SDK, golang.org/x/net (fetch tool's HTML→text), and stdlib.
- Only emojis that already exist in the codebase (`✓`, `✗`, `▸`, `›`, `▏`) — nothing new unless the user asks.
- Comments: default to none. Only add one when a reader cannot derive the reason from the code.
- Debug logging uses `debugLog(format, args...)` and is a no-op unless `ASK_DEBUG=1`. Add one when crossing an async boundary (paste command, MCP handler, provider stream, tool dispatch).

## Known-fragile areas

- `layout()` extra-row math: any change to what appears between viewport and input (chip, thumbnail strip, spacer row) needs the `extra` term in `layout()` and the emission order in `viewBody()` kept in sync.
- Scrollbar column is drawn over `m.width-1`. If any text-rendering style grows a margin or a user-bar width past `m.width-1`, the scrollbar will be overwritten or vice-versa.
- `askToolRequestMsg` is rejected if the modal is already open — only one MCP ask at a time. Double-calls from Claude return `cancelled: true` for the second one.
- `contentFingerprint` must mix in `len(m.history[m.shellOutIdx].text)` whenever a shell output entry is active. The frame cache is keyed on `len(m.history) | m.width`, and shell mode appends streamed lines in place to a single history entry, so without that extra term the cache returns a stale (first-line-only) view until something else (spinner row, window resize) perturbs the key.

## @-link references

Markdown files that are part of the system prompt (CLAUDE.md, AGENTS.md, eager rules, skill bodies, subagent bodies) may reference other markdown files using `@path/to/file.md` syntax. These references are resolved at prompt-assembly time and the referenced files' bodies are inlined.

Syntax: `@path/to/file.md` — exactly the @-prefixed relative path to a `.md` file.

Do not place `@`-links inside fenced code blocks — they are stripped before extraction and are never followed.

Resolution rules:
- The path is relative to the repository root (or the working directory when there is no `.git` ancestor).
- The path must not start with `/`, `./`, or `../`, and must not contain a `..` segment anywhere.
- The path must end in `.md` (case-insensitive — `.MD`, `.Md` etc. are accepted).
- The resolved absolute path must lie inside the repository root (checked via `filepath.Rel`).
- The file must exist as a regular file and not be empty.

Loading:
- `@`-references are extracted from the **untruncated** bodies of context files (CLAUDE.md, etc.) and eager rules, and carried on `LoadedContextDoc.Links` / `Rule.Links`. Prompt assembly seeds the graph from those lists via `LoadContextLinksFrom`, never by re-scanning the capped `Body` — a link living past the cap is still a real dependency of the instructions. Do not "simplify" this back to `LoadContextLinks(root, []string{doc.Body})`.
- The loader walks the reference graph breadth-first, with deduplication by absolute path (cycle-safe). Each loaded file is likewise scanned for further links *before* it is truncated, so a deep chain survives a large intermediate document.
- Each loaded file is capped at `AgentContextFileCap` (128 000 characters, same cap as context files and rules); whitespace-only files are skipped. Truncation goes through `TruncateInstructionDoc`: it cuts on a line boundary, never splits a UTF-8 rune (`body[:cap]` used to emit invalid UTF-8), and the notice names the file plus the byte counts so the model can read the part that did not fit. The cap is a backstop against pathological files — it is NOT a budget for trimming hand-written instructions, so raise it rather than letting a real CLAUDE.md lose its tail.
- The resulting documents are rendered into a single `<included_docs>` block in the system prompt, sorted by path, placed between `<project_rules>` and `<project_memory>`.

Lazy surfaces (JIT path-scoped rules, `/skill-name` invocations, subagent definitions) resolve their own `@`-links at injection time: rule-linked docs appear as `### Included from <path>` under the rule body; skill-linked docs appear as `<file path="...">` blocks after `<loaded_skill>`; subagent-linked docs appear under `## @-linked docs` in the subagent's `Prompt`.

