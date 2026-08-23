# Repo notes for coding agents

This file is the always-loaded root. It holds only what applies to every
change. Area detail lives next to the code (`<dir>/CLAUDE.md`) or in a
path-scoped rule (`.claude/rules/*.md`) that loads when you read a
matching file — see "Where the detail lives". When you change behavior in
an area, update that area's document, not this one. Keep this file under
10 KB; both ask and Claude Code send it in full on every turn.

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

## What ask is

`ask` is a Bubble Tea v2 TUI coding agent. The agent loop runs in-process
on Google ADK 2.0 (`google.golang.org/adk/v2`) and the GenAI SDK; the LLM
providers are Vertex AI Gemini and OpenRouter (`pkg/providers`). There are
no CLI subprocesses and no loopback MCP server: every tool is native Go,
and ask is an MCP *client* only. The TUI is `cmd/ask`; the headless
runtime is `pkg/engine`, re-exported as a library by the root package
(`ask.Run` in `ask.go`).

## Repo map

| Path | What lives there |
|------|------------------|
| `cmd/ask/` | The TUI: one `package main`, one file per concern (tabs, sidebar, screens, overlays, shell mode, Kitty images, the provider adapter and session goroutine, issue/PR kanban, workflow builder). |
| `pkg/engine/` | Headless ADK runtime: runner loop, sessions and transcript store, system-prompt assembly (CLAUDE.md, `.claude/rules`, `@`-links), skills, subagents, plugin discovery, the model builder with transient retry, headless workflow runner. |
| `pkg/tools/` | Every agent tool (file, search, bash + jobs, fetch, web_search, todos, ask_user_question/end_turn, workflow_*, search_tools/invoke_tool, artifacts, memory, extensions, MCP client + OAuth + server config, sudo askpass IPC). |
| `pkg/providers/` | The `Provider` contract and registry, Vertex and OpenRouter, the shared OpenAI-protocol model, static catalog, models.dev client, merged model metadata and pricing, the steering prompt. |
| `pkg/workflow/` | Workflow definitions, the three-scope store, compile to an ADK graph, progress adapter, run tracker, sources. |
| `pkg/plugin/` | Claude Code plugin-marketplace format: manifests, marketplace/plugin state, install, publish, import from Claude. |
| `pkg/memory/` | sqlite-vec memory store with a local llama.cpp embedding model (cgo). |
| `pkg/config/` | `~/.config/ask/ask.json` shapes, per-provider blocks, legacy migration, worktree helpers. |
| `pkg/diff/` | Pure-Go Myers unified diff and parser. |
| `build/` | Gitignored: the llama.cpp checkout and static libraries that `pkg/memory` links against. |

## Where the detail lives

| Document | Loads when you read | Owns |
|----------|---------------------|------|
| `cmd/ask/CLAUDE.md` | anything under `cmd/ask/` | Bubble Tea wiring, layout and overlay rules, sidebar, screens, shell mode, clipboard/Kitty, the provider message protocol, grouped file map. |
| `pkg/engine/CLAUDE.md` | anything under `pkg/engine/` | Runner loop, sessions, prompt assembly, context files, `@`-links, rules discovery. |
| `pkg/memory/CLAUDE.md` | anything under `pkg/memory/` | The memory store, embedding model, injection points, build prerequisite. |
| `.claude/rules/providers.md` | `pkg/providers/**`, `pkg/config/**`, the provider-facing TUI files | The `Provider` interface, adding a provider, config blocks, model metadata precedence, pricing. |
| `.claude/rules/tools.md` | `pkg/tools/**`, `cmd/ask/agent_tools*.go`, `agent_run.go`, `agent_provider.go` | Typed tool contract, core vs deferred registry, description phrases, approval gate, todos/workflow guard, MCP client. |
| `.claude/rules/workflows.md` | `pkg/workflow/**`, `cmd/ask/workflow*.go`, the workflow tools | Schema and scopes, compile to ADK graph and its invariants, progress, artifacts, builder screen, supplant. |
| `.claude/rules/skills-plugins.md` | `pkg/plugin/**`, skills/subagent files, `skills_browser*.go` | Marketplace format, scopes and state dirs, discovery, publish/pull, the browser. |
| `.claude/rules/issues.md` | `cmd/ask/issue*.go`, `pr_provider*.go`, `mcp_linear*.go` | Kanban, carry-and-drop, loader, issue providers. |
| `.claude/rules/testing.md` | any `*_test.go` | Behavioral-only tests, seams and fakes, isolation, where tests live. |

## Build and test

`pkg/memory` is cgo against llama.cpp, so every `go build` / `go test`
needs the static libraries and the `CGO_*` flags the Makefile exports.
The Makefile targets do all of it:

```
make build      # clone+build llama.cpp (once), download the embedding model (once), go build -o bin/ask ./cmd/ask
make install    # same prerequisites, then go install ./cmd/ask
make test       # same prerequisites, then go test ./...
make clean
```

- Running `go test ./...` directly works once `build/llama.cpp` exists,
  with the three `CGO_*` variables from the Makefile in the environment.
- The whole suite takes about 10 seconds; `cmd/ask` is the slow package.
  Tests never spawn subprocesses (except `git` and `jj` in the worktree
  tests) and
  never touch the network or the real `$HOME` — see `.claude/rules/testing.md`.
- `./...` also walks into `build/llama.cpp` and reports a Go package
  with no tests there; that is expected.

## Conventions

- **Every change ships with tests** in the matching `_test.go`, and tests
  are behavioral (state, messages, bytes, files), never rendering.
- **Look at how the neighbouring code does it first.** Module layout,
  naming, message flow, and UI silhouettes must match what exists.
- **No new runtime dependencies without asking.** Direct dependencies
  today: Charm (bubbletea, bubbles, lipgloss, glamour, ultraviolet,
  x/ansi), Google ADK v2 and GenAI, `cloud.google.com/go/auth`,
  `openai-go/v3`, the official MCP go-sdk, `google/jsonschema-go`,
  `google/uuid`, `mattn/go-sqlite3` + `sqlite-vec-go-bindings`,
  `golang.org/x/net`, `golang.org/x/oauth2`, and stdlib.
- **Pluggable components are Go interfaces** with optional capability
  interfaces found by type assertion — never a struct of nil-able func
  fields.
- **Square corners everywhere.** New surfaces use `lipgloss.NormalBorder()`;
  popups are wide-and-flat (landscape, few visible rows, scroll windows).
- **Only glyphs already in the codebase** (`✓`, `✗`, `▸`, `›`, `▏`, `⟳`,
  `⚠`, `●`) — nothing new unless the user asks.
- **Comments:** default to none. Add one only when a reader cannot derive
  the reason from the code.
- **Debug logging:** `debugLog(format, args...)`, a no-op unless
  `ASK_DEBUG=1` (writes `/tmp/ask.log`). Add one when crossing an async
  boundary.
- **Clipboard:** image paste reads Wayland (`wl-paste`) and macOS
  (`osascript`) only — no X11 image path without asking. Copy-out of
  selected text uses OSC 52 plus `pbcopy` / `wl-copy` / `xclip` / `xsel`.

## State on disk

| Location | Holds |
|----------|-------|
| `~/.config/ask/ask.json` | Config: provider blocks, UI toggles, keybindings, MCP servers, recent models, per-project blocks (issues, MCP tokens, user-scope workflows). |
| `~/.config/ask/agent-sessions/<provider>/` | ADK transcripts behind `/resume`. `~/.config/ask/sessions.json` is the virtual-session index. |
| `~/.config/ask/{skills,agents,workflows}/` | User-scope skills, subagents, workflows. |
| `~/.config/ask/plugins/` | Known marketplaces, installed plugins, publications, the plugin cache. |
| `~/.config/ask/cache/models-dev.json` | models.dev snapshot (24h TTL). |
| `~/.config/ask/mcp-oauth/` | OAuth tokens for remote MCP servers (0600). |
| `~/.config/ask/memory/memory.db` | The sqlite-vec memory store. |
| `~/.local/share/ask/models/` | The embedding model the Makefile downloads. |
| `<project>/.ask/` | Committed project scope: `skills/`, `agents/`, `workflows/*.json`, `plugins.json`. |
| `<project>/.mcp.json` | Project MCP servers in the Claude Code shape, merged under ask's own config. |
