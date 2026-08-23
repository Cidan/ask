# ask

<p align="center">
  <a href="https://github.com/Cidan/ask/releases"><img src="https://img.shields.io/github/v/release/Cidan/ask?color=ff75b7&label=release" alt="Latest Release"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/Cidan/ask?color=a78bfa" alt="MIT License"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/github/go-mod/go-version/Cidan/ask?logo=go&logoColor=white&color=00add8" alt="Go version"></a>
  <a href="https://github.com/charmbracelet/bubbletea"><img src="https://img.shields.io/badge/bubble%20tea-v2-ff75b7?logo=go&logoColor=white" alt="Bubble Tea v2"></a>
</p>

<p align="center">
  A Bubble Tea v2 TUI coding agent on Google ADK 2.0.<br />
  Vertex AI Gemini and OpenRouter, native Go tools, tabs with a task sidebar,<br />
  inline markdown, images, diffs, kanban issue and PR boards, and agent workflows.
</p>

<p align="center"><img width="800" alt="ask demo" src="https://vhs.charm.sh/vhs-4bXH7YlhqAMXxv6lqsjjTs.gif" /></p>

`ask` runs the agent loop in-process on [Google ADK 2.0](https://github.com/google/adk-go)
against Vertex AI Gemini or any model on OpenRouter. There are no CLI
subprocesses and no loopback MCP server: every tool the agent uses —
file edits, search, shell, fetch, web search, todos, sub-agents,
workflows, memory — is native Go, and ask is an MCP *client* to the
servers you configure. Sessions resume, tabs isolate, shell mode drops
you into `$SHELL`, project memory lives in a local sqlite-vec store, and
sixteen themes re-paint the whole UI live.

## Features

- **Two providers, one picker** — Vertex AI Gemini (Application Default
  Credentials or a service-account key) and OpenRouter (any model it
  serves, through the OpenAI-compatible API). `Ctrl+M` opens a model
  picker with a search box, a "Recently used" section, and every
  provider's models with descriptions, context window, output limit,
  pricing, and modalities pulled from [models.dev](https://models.dev)
  and the provider's live listing. Picking a model whose provider has
  no key yet prompts for it inline. The sidebar shows the running cost
  of every tab.
- **Tabs and a task sidebar** — `Ctrl+T` opens a tab with its own agent
  session, shell, MCP clients, history, and cwd. A right-hand column
  shows one card per tab: an LLM-generated title, provider · model,
  session spend, the agent's current todo or status, and `⚠` needs
  input · `✓` done · `✗` failed · `●` busy badges. `Tab` focuses the
  list (`↑`/`↓` or `j`/`k` switch tabs instantly), typing jumps back to
  the input; `Ctrl+←`/`Ctrl+→` and `Ctrl+↑`/`Ctrl+↓` switch from anywhere.
- **Resume sessions** — `/resume` picks a prior conversation in the
  current directory; `ask resume <vs-id>` reopens one from the shell
  (ask prints the id on quit).
- **Inline markdown** with [glamour](https://github.com/charmbracelet/glamour),
  cached per history entry; **inline colored diffs** for every `edit` /
  `write`; a **live todo panel** (`Ctrl+X` expands and collapses it)
  while a turn runs; a **status line** naming the tool in flight and the
  model's own one-line description of what it is doing.
- **Question, plan, and approval modals** — the agent can ask
  single-choice, multi-choice, or diagram-preview questions; present a
  finalized plan for you to run inline or hand to a workflow; and every
  mutating tool call (shell beyond a read-only allowlist, `edit`,
  `write`, `fetch`, `web_search`, plugin installs) waits for
  allow / deny / always-allow unless you turn permissions off. `sudo`
  inside the agent's shell prompts for the password in the TUI.
- **Issues and PRs as kanban** — `Ctrl+I` opens the project's issue
  tracker (GitHub or Linear), `Ctrl+R` the GitHub pull requests, as
  column boards with cursor pagination, a `/` filter, a markdown detail
  view, and carry-and-drop (`Space`) to change status.
- **Workflows** — `f` on an issue or `Ctrl+F` on a chat runs a named
  pipeline of agent steps (each with its own provider, model, and
  prompt; optional loops) as one ADK graph. Build them on `Ctrl+W`;
  store them per user, per repo (`.ask/workflows/`, committed), or
  globally; ship them in plugins.
- **Skills, subagents, plugins** — the [Agent Skills](https://agentskills.io)
  standard (`SKILL.md`, `/skill-name` slash commands), named subagents
  with their own tools, model, and even provider, and Claude Code
  plugin marketplaces. `Ctrl+S` opens a browser over everything
  installed and every marketplace; the agent can also create, edit,
  search, install, and publish them through its tools.
- **MCP client** — stdio, SSE, and streamable-HTTP servers from
  `ask.json` or a project `.mcp.json`, with OAuth (PKCE + dynamic
  registration) for remote servers, live tool-list refresh, and
  elicitation rendered as the question modal.
- **Project memory** — a sqlite-vec store embedded by a local
  llama.cpp model. Recall is injected into the system prompt, per turn,
  and next to every file the agent reads; the agent can index notes on
  request; finished workflows are filed automatically.
- **Shell mode** — `!` on an empty prompt runs a line through `$SHELL`
  with streaming output, persistent `cd`, and its own history.
- **Image attachments** — `Ctrl+V` pastes an image from the clipboard
  (Wayland `wl-paste`, macOS `osascript`); Kitty-protocol terminals
  render inline thumbnails, others show a text chip; the image still
  goes to the model when it supports vision.
- **Themes, keybindings, config** — sixteen palettes, every global
  shortcut remappable in `/config → Keybindings`, all settings in
  `~/.config/ask/ask.json`.

## Install

ask links against llama.cpp for the memory embedder, so the build is
through the Makefile:

```
git clone https://github.com/Cidan/ask
cd ask
make install      # clones + builds llama.cpp into build/, downloads the embedding model, go install ./cmd/ask
```

Requirements: Go 1.26+, `git`, `cmake` and a C/C++ toolchain (for
llama.cpp), `curl`. The embedding model
(`embeddinggemma-300M-Q8_0.gguf`, ~300 MB) lands in
`~/.local/share/ask/models/`. `make build` produces `bin/ask` instead
of installing.

Optional:

- `wl-clipboard` on Wayland for image paste (`pacman -S wl-clipboard`).
- A terminal speaking the Kitty graphics protocol (Kitty, Ghostty) for
  inline thumbnails. Elsewhere you see a text chip; the image is still
  sent to the model.
- `rg` (ripgrep) — the agent's `grep` tool uses it when present and
  falls back to a pure-Go search otherwise.

> [!NOTE]
> Image paste is Wayland and macOS only. X11 is not supported for
> paste; copying text out of the transcript works everywhere (OSC 52
> plus `wl-copy` / `xclip` / `xsel` / `pbcopy`).

## Providers

| Provider | Settings (`/config → <Provider>...`) | Environment fallback |
|----------|--------------------------------------|----------------------|
| Vertex AI (default) | `project` (required), `location` (default `global`), `serviceAccountKey` (path; otherwise Application Default Credentials) | `GOOGLE_CLOUD_PROJECT`, `GOOGLE_APPLICATION_CREDENTIALS` |
| OpenRouter | `apiKey`, `baseURL` | `OPENROUTER_API_KEY` |

Without a service-account key, Vertex uses ADC: `gcloud auth
application-default login` or the GCE metadata server. The default
models are `gemini-3.7-flash` on Vertex and `anthropic/claude-3.7-sonnet`
on OpenRouter; `/effort` picks the reasoning effort.

## Usage

```
ask                # start in the current directory
ask resume <vid>   # reopen a virtual session (vs-<hex>, listed by /resume; printed on quit)
ask --help
```

### Slash commands

Typing `/` opens a popover listing every command; keep typing to
filter, `↑`/`↓` to move, `Tab` to complete.

| Command | What it does |
|---------|--------------|
| `/resume` | Pick a prior session in this directory |
| `/new`, `/clear` | Discard history and start a fresh session |
| `/effort` | Choose the reasoning effort for the current provider |
| `/config` | Open the config modal |
| `/workflows` | Open the workflow builder |
| `/skills` | Open the skills browser (`/skills add marketplace <src> [project]`, `/skills remove marketplace <name>`, `/skills import claude`, `/skills refresh` run without it) |
| `/<skill>` | Run a user-invocable skill; its arguments replace `$ARGUMENTS` in the skill body |

### Built-in path commands

`cd` and `ls` are intercepted before the line reaches the model.

| Command | What it does |
|---------|--------------|
| `cd [path]` | Change directory (`~` expands; no arg → home). Kills the live agent session and clears history — sessions are bound to a cwd. |
| `ls [path]` | Colorized listing with mode, size, and age; globs and `~` work. |

`Tab` after `cd ` / `ls ` completes paths.

### Tabs and the sidebar

`Ctrl+T` opens a tab that inherits the active tab's cwd and nothing
else. Switching tabs also `chdir`s the process so `/resume`, path
completion, and the prompt see that tab's directory. `Ctrl+D`, or a
second `Ctrl+C` on an empty idle prompt, closes the tab; closing the
last one quits.

The sidebar (about a fifth of the width, 30–48 columns) is the only tab
strip. `Tab` moves focus into it when the input has no other use for
the key; there `↑`/`↓`/`j`/`k` switch tabs, `Ctrl+D` closes the selected
one, `Esc`/`Enter`/`Tab` return, and any printable key returns and
types. A question or approval that fires on a background tab does not
steal focus — its card shows `⚠` until you switch to it.

### Shell mode

`!` as the first character of an empty prompt enters shell mode. Enter
runs the line through `$SHELL -c`; stdout and stderr stream into the
transcript, capped at 100 lines per command (the command still runs to
completion). `cd` inside the command persists to ask. `Esc`, `Ctrl+C`
on an empty prompt, or two `Backspace`s on an empty prompt leave the
mode; `Ctrl+C` while a command runs kills its process group. Shell
history is separate from prompt history. Full-screen programs (vim,
htop, less) are not supported — output goes through pipes, not a PTY.

### Keybindings

Global shortcuts are remappable in `/config → Keybindings` (press
Enter on a row, then the new key). Defaults:

| Key | Action |
|-----|--------|
| `Enter` | Send / confirm |
| `Ctrl+V` | Paste an image from the clipboard |
| `Ctrl+C` | While a turn runs: confirm cancelling it. On an empty idle prompt, twice: close the tab |
| `Ctrl+D` | Close the tab (quits on the last one) |
| `Ctrl+T` | New tab |
| `Ctrl+←` / `Ctrl+→`, `Ctrl+↑` / `Ctrl+↓` | Previous / next tab |
| `Tab` | Focus the sidebar (or complete a path / slash command) |
| `Ctrl+M` | Model picker |
| `Ctrl+O` | Chat screen |
| `Ctrl+I` | Issues screen |
| `Ctrl+R` | Pull requests screen |
| `Ctrl+W` | Workflow builder |
| `Ctrl+S` | Skills and marketplace browser |
| `Ctrl+F` | Run a workflow on the current chat |
| `Ctrl+X` | Expand / collapse the todo panel |
| `Ctrl+Z` | Suspend ask |
| `PgUp` / `PgDn`, mouse wheel, click on the scrollbar | Scroll the transcript |
| `↑` / `↓` | Lists; on the first line of an unmodified input, prompt history |
| `!` (empty prompt) | Shell mode |

Reload on the issue and PR boards is unbound by default and can be
bound on the same screen.

### Question modal

| Key | Action |
|-----|--------|
| `↑` / `↓` | Move between options |
| `Space` | Toggle an option (pick-many) |
| `Enter` | Commit this question and advance; submit on the last |
| `←` / `→`, `Tab` / `Shift+Tab` | Switch between questions |
| `n` | Add a note to the current question |
| Typing on "Enter your own" | Free-text answer |
| `Esc` | Cancel |

## Issues and pull requests

`Ctrl+I` opens the issue board for the project, `Ctrl+R` the GitHub
pull-request board. Configure the tracker under `/config → Project
Options`: the issue provider (`github` or `linear`), a GitHub MCP
endpoint and PAT, or a Linear API key and team key.

Columns fetch in parallel with cursor pagination. `↑`/`↓` move within a
column, `←`/`→`/`Tab` change column, `Enter` opens the markdown detail,
`/` filters. `Space` picks up the focused card; carry it across columns
and `Space` again drops it — the move is applied locally at once and
rolled back with a toast if the provider call fails. `Esc` cancels a
carry. Cards show `▸` / `✓` / `✗` for the latest workflow run on that
issue, and `f` starts one.

## Workflows

A workflow is a named list of steps; each step pins a provider, a
model, and a prompt, and a `loop` step repeats its inner steps until
the last one calls `exit_loop` or the iteration cap is reached. The
whole chain runs as one ADK graph inside the tab that launched it
(`f` on an issue, `Ctrl+F` on a chat): the transcript shows one line per
step from the step's own `end_turn` summary, and `Enter` when it
finishes restores the conversation. Steps hand structured data to each
other through `save_artifact` / `load_artifacts`; the final step reports
the outcome and artifacts (PR links, tickets) with `finish_workflow`.

`Ctrl+W` (or `/workflows`) is the builder: create, rename, describe,
reorder, delete; edit a step's prompt in a multi-line editor; pick its
model through the same `Ctrl+M` picker. Workflows live in one of three
scopes — **user** (`~/.config/ask/ask.json`, per project), **repo**
(`<root>/.ask/workflows/*.json`, committed), **global**
(`~/.config/ask/workflows/`) — and `c` / `s` copy or move between them.
Plugins can ship workflows too; those are read-only until copied.

The agent has `workflow_list` / `workflow_get` / `workflow_create` /
`workflow_edit` / `workflow_delete` / `workflow_copy`, and its
`finalized_plan` modal offers to run a plan through a matching workflow.

## Skills, subagents, and plugins

- **Skills** follow the Agent Skills standard: a directory with a
  `SKILL.md` (frontmatter `name`, `description`, optional `user-invocable`)
  whose body loads only when used. User-invocable skills appear as
  `/name` in the slash popover.
- **Subagents** are markdown files with frontmatter `name`, `description`,
  `tools`, `model`, and ask's `provider`; the body is the system prompt.
  The agent's `task` tool runs them — on a different provider than the
  parent when the definition says so.
- **Plugins** use the Claude Code plugin-marketplace format unchanged
  (`.claude-plugin/marketplace.json`, `.claude-plugin/plugin.json`,
  `skills/`, `agents/`, `commands/`, plus ask's `workflows/`). A plugin
  published from ask installs in Claude Code; Claude Code ignores what
  it does not know.

| | user | project |
|---|---|---|
| skills / agents | `~/.config/ask/skills`, `~/.config/ask/agents` | `<root>/.ask/skills`, `<root>/.ask/agents` |
| marketplaces, enabled plugins | `~/.config/ask/plugins/` | `<root>/.ask/plugins.json` |

ask also reads the Claude Code and Agent Skills locations — `~/.claude/skills`,
`~/.claude/agents`, `~/.agents/skills`, and the project's `.claude/skills`,
`.claude/agents`, `.agents/skills` — so definitions you already have are
available without copying. New ones created from ask go to the `.ask` /
`~/.config/ask` directories above.

`Ctrl+S` opens the browser: an **Installed** lens (project, user, and
one group per plugin — skills, agents, and workflows tagged) and a
**Marketplace** lens (every registered marketplace and its plugins);
`Tab` switches lens, typing filters, `Enter` inserts a skill's slash
command or drills into a plugin. Install, remove, create, edit,
publish, and pull actions are on `Ctrl+` keys listed in the footer.
"Import from Claude Code" registers the marketplaces and plugins your
`~/.claude` already has — ask never writes there. The same operations
are agent tools (`skill_create`, `marketplace_search`, `plugin_install`,
`skill_publish`, …), so "turn this conversation into a skill" is one
request.

## Config

`/config` opens a filterable modal backed by `~/.config/ask/ask.json`;
every change writes immediately.

| Row | Default | What it does |
|-----|---------|--------------|
| Quiet Mode | on | Render the assistant's text once per completed block instead of streaming chunks |
| Cursor Blink | on | Blinking input cursor |
| Render Diffs | on | Show `edit` / `write` results as inline colored diffs |
| Tool Output | short | `short` shows each tool call as the model's one-line description; `full` adds the parameters and output; `off` hides tool calls |
| Skip All Permissions | off | Bypass the approval modal for every tool call (restarts the session) |
| Worktree | off | Run each session inside an isolated git worktree under `.claude/worktrees/` (adds it to `.gitignore`) |
| Gate Todos Before Mutate | off | Refuse `edit` / `write` until the agent has posted a todo list, and make it check the project's workflows first |
| Theme | default | Live-preview picker over 16 palettes |
| Default Provider | vertex | Provider for new sessions |
| Web Search... | | Brave Search API key for the agent's `web_search` tool (`BRAVE_API_KEY` also works) |
| Vertex AI... / OpenRouter... | | The provider's settings (see [Providers](#providers)) |
| Keybindings... | | Remap any global shortcut |

Project Options (same modal) hold the issue provider, the GitHub MCP
endpoint and PAT, the Linear endpoint, API key, and team key, and the
project's user-scope workflows. Other keys the file stores:
`providers.<id>.model` (last pick per provider), `recentModels`,
`keybindings`, `mcpServers`, `effort`, `ui.retry` (`maxRetries`,
`initialDelayMs`, `backoffFactor` for the agent's retry on transient
errors), and `projects.<root>` blocks.

## MCP servers

ask is an MCP client. Servers come from three layers, later ones
winning by name: the project's `.mcp.json` (Claude Code's shape), the
global `mcpServers` map in `ask.json`, and the per-project `mcpServers`
map. `${VAR}` and `${VAR:-default}` expand in every string.

```jsonc
{
  "mcpServers": {
    "fs":     { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."] },
    "remote": { "url": "https://mcp.example.com/mcp", "oauth": true, "enabledTools": ["search"] },
    "old":    { "type": "sse", "url": "https://legacy.example.com/sse", "disabled": true }
  }
}
```

`type` is inferred (`command` → stdio, `url` → streamable HTTP) unless
set to `sse`. `oauth: true` runs the authorization-code flow with PKCE
and dynamic client registration in your browser and stores the token
under `~/.config/ask/mcp-oauth/`. MCP tools are not part of the
model's wire tool list: the agent finds them with `search_tools` and
calls them with `invoke_tool`, so large servers cost no context until
used.

## Memory

`~/.config/ask/memory/memory.db` holds project-scoped memories embedded
by `embeddinggemma-300M` through llama.cpp. Recall runs at session
start (into the system prompt), per turn, and next to every file the
agent reads or edits; `memory_index` (approval-gated) writes; entries
not recalled for 30 days are swept. If the model file or the store
cannot be opened, ask starts without memory and says so on stderr.

## Debugging

`ASK_DEBUG=1` writes a trace to `/tmp/ask.log` (paste, send, agent
stream events, tool dispatch, MCP traffic).

## License

[MIT](./LICENSE)

---

<p align="center">
  Built on <a href="https://github.com/charmbracelet/bubbletea">Bubble Tea</a>,
  <a href="https://github.com/charmbracelet/bubbles">Bubbles</a>,
  <a href="https://github.com/charmbracelet/lipgloss">Lip Gloss</a>,
  <a href="https://github.com/charmbracelet/glamour">Glamour</a>,
  <a href="https://github.com/charmbracelet/ultraviolet">Ultraviolet</a>, and
  <a href="https://github.com/google/adk-go">Google ADK</a>.<br />
  Inspired by <a href="https://github.com/charmbracelet/crush">crush</a>.
</p>
