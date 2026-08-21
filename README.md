# ask

<p align="center">
  <a href="https://github.com/Cidan/ask/releases"><img src="https://img.shields.io/github/v/release/Cidan/ask?color=ff75b7&label=release" alt="Latest Release"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/Cidan/ask?color=a78bfa" alt="MIT License"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/github/go-mod/go-version/Cidan/ask?logo=go&logoColor=white&color=00add8" alt="Go version"></a>
  <a href="https://github.com/charmbracelet/bubbletea"><img src="https://img.shields.io/badge/bubble%20tea-v2-ff75b7?logo=go&logoColor=white" alt="Bubble Tea v2"></a>
</p>

<p align="center">
  A Bubble Tea v2 TUI for coding agents.<br />
  Streaming markdown, inline images, tabs, themes, colored diffs,<br />
  a draggable scrollbar, and a native question modal.
</p>

<p align="center"><img width="800" alt="ask demo" src="https://vhs.charm.sh/vhs-4bXH7YlhqAMXxv6lqsjjTs.gif" /></p>

`ask` runs an in-process Google ADK 2.0 loop against Vertex AI Gemini (or Anthropic/OpenAI/DeepSeek), driving a rich Bubble Tea TUI with NO CLI subprocesses and NO loopback MCP servers. Sessions resume, tabs isolate, shell mode drops you straight into `$SHELL`, memory is preserved via local `sqlite-vec`, and sixteen themes re-paint the whole UI live.

## Features

- **Chat with powerful AI models** via an in-process Google ADK 2.0 loop
- **[Tabs](#tabs)** — `Ctrl+T` opens a new tab with its own agent loop, shell, MCP bridge, history, session, and cwd; `Ctrl+←` / `Ctrl+→` cycle between tabs; a byobu-style strip at the bottom shows each tab's shortened cwd (prefixed with `▸` when that tab is busy); closing the last tab quits
- **Resume sessions** — `/resume` opens a picker of prior conversations in the current directory
- **Pick the provider + model** — `Ctrl+M` opens a crush-style picker: search box on top, "Recently used" first, then every provider's models with human-friendly names; `↑`/`↓` choose, `Enter` selects. Picking a model whose provider has no API key prompts for one inline and saves it to the config
- **Configurable UI** — `/config` toggles quiet mode, cursor blink, inline diff rendering, and skip-all-permissions; persisted to `~/.config/ask/ask.json`
- **Themes** — pick a palette from `/config` → Theme (16 flavors: `default`, `dracula`, `nord`, `gruvbox`, `tokyo night`, the four Catppuccin variants `latte`/`frappé`/`macchiato`/`mocha` plus the green-leaning Mocha sibling `matcha`, `rose pine`, `fighter` (Monokai Pro), `love` (crush), `hacker` (Matrix), `amber` (CRT), `ayu` (Ayu Mirage)). Backgrounds, foregrounds, borders, and glamour markdown/syntax highlighting all follow the active theme.
- **Inline markdown rendering** with [glamour](https://github.com/charmbracelet/glamour), cached per history entry so typing stays responsive in long chats
- **Live turn status** — spinner line surfaces the tool the agent is running (`Read: file.go`, `Bash: <description>`, `Grep: <pattern>`, `Task: <subagent>`, …)
- **Live todo panel** — `TodoWrite` entries render inline as a bordered box with ☐ / ▸ / ✓ markers while the turn is active
- **Issues screen** — `Ctrl+I` opens a kanban view of the project's issue tracker (GitHub today; the provider interface is open). Per-column queries fetch in parallel with cursor-based pagination, `↑`/`↓` move within a column, `←`/`→`/`Tab` cycle columns, `Enter` opens the markdown detail view, `/` opens an inline filter, and a reload key can be bound in `/config` → Keybindings (unbound by default — `Ctrl+R` opens the PRs screen). **Carry-and-drop status changes**: `Space` picks up the focused card (warn-color highlight, pinned to the top of whichever column you focus), `←`/`→`/`Tab` carry it across columns, `Space` drops to commit (optimistic local move + provider call; rollback + toast on failure), `Esc` cancels. Same-column drops are no-ops; opening `/`, reloading, or `Ctrl+O` silently cancels an in-flight carry. Cards (and the detail view) carry a per-issue status icon (`▸` running, `✓` done, `✗` failed) reflecting the most recent workflow run for that issue.
- **Workflow pipelines** — `f` on a focused issue picks a per-project pipeline and runs it in a fresh tab. Each pipeline is a chain of one-shot agent calls; every step pins its own provider (`anthropic` / `openai` / …) + model + prompt, so a single workflow can chain `anthropic → openai → anthropic` if you want. Steps run sequentially in the same cwd, and the previous step's assistant output is forwarded to the next as a `Previous step output:` block. The workflow tab is read-only — no input box, just a banner showing the current step (`▸ workflow "fix" · step 2/3: review (codex/gpt-5)`); `Ctrl+C` cancels the chain and `Ctrl+D` closes the tab. The kanban repaints in real time as each step runs. `Ctrl+F` on a chat tab pops the same picker against the current chat instead of an issue — the spawned workflow gets the user/assistant turns appended verbatim under a `Reference (chat transcript):` block (tool calls, shell output, and other system entries are filtered out). Build pipelines via `Ctrl+W` (or `/workflows`, or `/config` → Project Options → Workflows…); the dedicated screen offers per-project add/rename/reorder/delete with a multi-line in-app prompt editor. Edits on a workflow that's currently running are blocked with a toast until the run finishes.
- **Inline diffs** — `Edit` / `Write` / `NotebookEdit` structured patches render as colored unified diffs in history (toggle with `/config`)
- **Input history** — `↑` / `↓` at the first line of the input walks prior sent messages
- **Shell mode** — type `!` on an empty prompt to run a command through your `$SHELL`; stdout/stderr stream into history (capped at 100 lines), `cd` persists, `Esc` / `Ctrl+C` / double-backspace exits, `↑` / `↓` walks shell history separately from LLM input history
- **Image attachments** via clipboard paste
  - `Ctrl+V` on Wayland reads the clipboard with `wl-paste`
  - In Kitty-compatible terminals (Kitty, Ghostty) images render as inline thumbnails using the Kitty graphics protocol with Unicode placeholders
  - In any other terminal they fall back to a text chip
  - Multiple attachments pasted in a row show side-by-side with a bordered preview
- **Draggable scrollbar** in the right column (mouse or `PgUp`/`PgDn`); the viewport sticks to the bottom only while you are at the bottom, so scrolling up during a stream no longer yanks you back
- **Native question modal** (`ask_user_question` tool) with three question kinds:
  - `pick_one` — single select radio list
  - `pick_many` — multi-select checkboxes
  - `pick_diagram` — radio list with an ASCII-art preview box rendered beside it
  - All kinds support `allow_custom` (appends an Enter-your-own free-text option) and per-question notes (`n`)
- **Native approval modal** (`approval_prompt`) — shows a per-tool allow / deny / always-allow modal (concise one-line summary — no field dump) before the tool runs; "always allow" records a session-scoped rule so repeat calls for the same file or command skip the prompt

## Demos

Rendered with [VHS](https://github.com/charmbracelet/vhs).

### `cd`, `ls`, and tabs

![cd, ls, and tabs](https://vhs.charm.sh/vhs-6Dul4zuJDXNHmG60kqg8Cg.gif)

ask intercepts `cd` and `ls` as local shell-style builtins — the line
never reaches the agent — so you can walk the tree, inspect mode bits,
sizes, and "X ago" mtimes, and land at the right cwd without ever
leaving the TUI. `Tab` on `cd ` / `ls ` completes against the current
prefix, `~` and `~/foo` expand to `$HOME`, and globs (`*`, `?`, `[…]`)
work for `ls`. `cd` also kills the live agent session and clears the
turn history, because agent sessions are bound to a cwd — the next
send spawns a fresh session rooted at the new directory.

`Ctrl+T` opens a new tab. Each tab is a fully independent sandbox: its
own agent loop, shell subprocess, MCP client manager, session id, viewport scroll, pending attachments, and cwd. Nothing
about one tab leaks into another — pasting an image, running a shell
command, or typing `cd` only affects the active tab. A new tab inherits
the active tab's cwd at spawn time; after that the two drift apart.
`Ctrl+Left` / `Ctrl+Right` cycle (wrapping at the ends) and `ask`
`chdir`s the process on each switch so anything that reads `os.Getwd` —
`/resume`, path completion, the prompt — sees that tab's directory.

The byobu-style strip at the bottom appears whenever more than one tab
is open. It shows each tab's shortened cwd; the active tab is
highlighted, and any tab with a streaming turn or a running shell
command gets a leading `▸` so background work is visible at a glance.
If the bar runs out of width, overflow tabs collapse into a trailing
`…`. `Ctrl+D` (or a second `Ctrl+C` on an empty idle prompt) closes the
current tab; closing the last one quits ask.

See [Built-in path commands](#built-in-path-commands) and
[Tabs](#tabs) for the full reference.

### The `/config` modal

![/config modal](https://vhs.charm.sh/vhs-1B2hEL8frht4oFbNQk7gb7.gif)

`/config` opens a filterable modal backed by `~/.config/ask/ask.json`.
Every toggle writes to disk the moment you press `Enter`, so there's no
save step; `↑` / `↓` move the cursor, `Enter` flips the highlighted
entry, and typing narrows the list on the fly — the cursor snaps to
the first remaining match so `render` + `Enter` toggles Render Diffs
without scrolling.

The toggles on offer are **Quiet Mode** (batch vs. streaming assistant
output), **Cursor Blink** (steady vs. 650 ms blink), **Render Diffs**
(inline colored unified diffs for `Edit` / `Write` / `NotebookEdit`),
**Skip All Permissions** (tell the agent to dangerously skip permissions), and **Worktree** (run each session inside an isolated git
worktree). The last two kill the running agent session so the next
send respawns with the new flag state; toggling Worktree on also
appends `.claude/worktrees/` to the repo's `.gitignore` if no existing
rule already covers it.

A sixth row, **Theme**, opens the picker shown below.
See [Config](#config) for the full table of defaults and behaviors.

### Themes

![theme picker](https://vhs.charm.sh/vhs-5Zk9peJkMSQB0eKgdAAKmf.gif)

The Theme row under `/config` opens a dedicated picker with live
preview — `↑` / `↓` repaint the backgrounds, prompt colors, borders,
glamour markdown, inline diffs, and scrollbar on every press, so you
can eyeball each palette against the real conversation underneath
rather than a swatch. `Enter` saves the selection to
`~/.config/ask/ask.json`; `Esc` reverts to whatever theme was active
when you opened the picker.

Sixteen flavors ship by default: `default` (respects your terminal's
own background), `dracula`, `nord`, `gruvbox`, `tokyo night`, all four
official Catppuccin variants (`latte`, `frappé`, `macchiato`, `mocha`),
the green-leaning Mocha sibling `matcha`, `rose pine`, `fighter` (the
softer Monokai Pro / Octagon palette), `love` (the Charm crush
charmtone palette), `hacker` (Matrix phosphor green on CRT black),
`amber` (1970s DEC/IBM amber phosphor), and `ayu` (Ayu Mirage). All glamour markdown and
syntax highlighting follow the active theme, so code blocks in
responses re-theme too.

### The slash-command popover

![slash-command popover](https://vhs.charm.sh/vhs-8HLPTov9XsaMEG03QIMDh.gif)

Typing `/` at the prompt opens a popover with every slash command ask
knows about. Five are built into the TUI itself — `/resume`, `/new`,
`/clear`, `/effort`, `/config` — and the rest are discovered from
the tool surface the first time the session starts and cached
into `~/.config/ask/ask.json` so the popover has completions from the
first keystroke on the next launch.

Descriptions for the discovered commands are harvested from the YAML
frontmatter of the command and skill files on disk. ask walks
`~/.claude/commands/`, `./.claude/commands/`, `~/.claude/skills/`, and
`./.claude/skills/` for project- and user-level entries, and
`~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/commands` and
`.../skills/` for plugin-published ones. Plugin commands get prefixed
with `<plugin>:` so two plugins can both expose a `/review` without
colliding.

Continue typing to filter both sets together — `/r` narrows to
`/resume` alongside any agent-side `/release-notes`, `/review`,
`/security-review`, etc., while `/eff` jumps to `/effort`. `↑` / `↓`
walk the filtered list and `Tab` auto-completes the highlighted entry
into the input.

## Install

```
go install github.com/Cidan/ask@latest
```

Requires Go 1.26+.

Optional dependencies:

- `wl-clipboard` — for image paste on Wayland (`pacman -S wl-clipboard`, etc.)
- A terminal speaking the Kitty graphics protocol — for inline thumbnails (Kitty, Ghostty). Without one, images still send to the model; only the local preview falls back to a text chip.

> [!TIP]
> Pair `ask` with Kitty or Ghostty to get inline image thumbnails via the
> Kitty graphics protocol. Everywhere else you'll see a text chip, but the
> image is still sent to the model — nothing is dropped.

> [!WARNING]
> Clipboard paste is **Wayland only** by design. X11 and macOS fallbacks
> haven't been added. If you need them, open an issue before wiring one up —
> they're out of scope right now.

## Usage

Launch in any directory:

```
ask
```

### Slash commands

| Command            | What it does                                                          |
|--------------------|-----------------------------------------------------------------------|
| `/resume`          | Pick a prior session in this directory                                |
| `/new` / `/clear`  | Discard history and start a fresh session                             |
| `/effort`          | Choose the provider's reasoning effort                                |
| `/config`          | Open the `ask` config modal (see [Config](#config))                   |

Provider and model switching live on `Ctrl+M` (no slash command): a
searchable picker with a "Recently used" section followed by every
provider's models under per-provider headings. Picking a model whose
provider has no API key configured prompts for the key inline and
saves it to `~/.config/ask/ask.json` before applying the switch.

The model's own slash commands (the ones surfaced by the agent) are
merged into the popover alongside these. Typing `/` filters both lists.

### Built-in path commands

`ask` intercepts `cd` and `ls` as local shell-style builtins before the
input is ever sent to the model, so you can navigate without dropping out
of the TUI.

| Command         | What it does                                                                 |
|-----------------|------------------------------------------------------------------------------|
| `cd [path]`     | Change the working directory. No arg → home. Tilde (`~`, `~/foo`) expands. Kills the live agent session and clears history, since agent sessions are bound to a cwd. |
| `ls [path]`     | Colorized listing (dirs first, executables, symlinks) with mode, human size, and "X ago" timestamps. No arg → current dir. Globs (`*`, `?`, `[…]`) and tilde expansion both work; `ls path/to/file` prints a single-row entry. |

`Tab` on `cd ` or `ls ` triggers path completion against the current
prefix, same as anywhere else a path is expected.

### Tabs

`Ctrl+T` opens a new tab. Each tab is its own sandbox: a separate
`agent` session, shell subprocess, MCP client manager (with its own
localhost port), history, session id, viewport scroll position,
pending attachments, and working directory. Nothing about one tab
leaks into another — stopping a turn, pasting an image, running a
shell command, or typing `cd` only affects the active tab.

A new tab inherits the active tab's cwd at spawn time; after that the
two cwds are independent. When you switch tabs (`Ctrl+←` / `Ctrl+→`,
wraps), `ask` also `chdir`s the process so anything that reads
`os.Getwd` — `/resume`, path completion, the prompt — sees that tab's
directory.

A byobu-style strip appears at the bottom of the screen whenever more
than one tab is open. It shows each tab's shortened cwd; the active
tab is highlighted, and busy tabs (turn streaming, shell command
running) get a leading `▸` so you can see background work at a glance.
If the bar runs out of width the overflow tabs collapse into a
trailing `…`.

MCP calls (`ask_user_question`, `approval_prompt`) are routed to the
tab that spawned them, not the active one. When a request arrives for
a background tab, `ask` switches focus to it automatically so the
modal is visible. If a tab is closed while an MCP call is still
pending, the reply is auto-cancelled so the blocked tool call on the
agent side unwinds cleanly.

`Ctrl+D`, or a second `Ctrl+C` on an empty idle prompt, closes the
current tab (killing its agent session, its shell, and stopping its MCP
bridge). Closing the last tab quits `ask`.

### Shell mode

Type `!` as the first character of an empty prompt to enter shell
mode. The `!` is consumed and a **Shell Mode** indicator appears on
the spinner row. Enter sends the input to your `$SHELL` (falling back
to `/bin/sh`), and stdout/stderr stream line-by-line into history in
the same slot LLM responses use. Output is capped at 100 lines per
command with a `… output truncated at 100 lines` marker; the command
still runs to completion, and the pipe stays drained so it can't block
on a full kernel buffer.

`cd` and anything else that changes `$PWD` inside the subshell
persists into ask's own process after the command returns, so the
prompt, `/resume`, and path completion all track the new directory.

Exit shell mode with `Esc`, `Ctrl+C` on an empty prompt, or two
consecutive `Backspace` presses on an empty prompt. While a command is
running, `Ctrl+C` SIGKILLs the whole process group instead of leaving
the mode. Shell mode keeps its own `↑` / `↓` history independent of
the LLM input history, and the `/`-slash popover plus `cd` / `ls` path
picker are suppressed while active.

Curses / full-screen apps (vim, htop, less, …) are **not supported** —
output goes through pipes, not a PTY, so their altscreen sequences
render as raw text in history. Drop to a separate shell for those.

### Keybindings

| Key                    | Action                                             |
|------------------------|----------------------------------------------------|
| `Enter`                | Send message / confirm                             |
| `Shift+Enter`, `Ctrl+J`| Insert newline in the input                        |
| `Ctrl+V`               | Paste image from clipboard                         |
| `Ctrl+C` / `Esc`       | While a turn is running, open a `Stop this turn?` confirm box; on confirm it kills the agent session and a new one spawns on the next send. `Esc` also clears pending attachments when idle. |
| `Ctrl+C` (twice, idle) | Close the current tab. First press shows a `Press ctrl+c again to exit` hint; a second `Ctrl+C` closes the tab (or quits if it was the last). Any other key disarms the hint. |
| `Ctrl+D`               | Close the current tab immediately; quits if it's the last one |
| `Ctrl+T`               | Open a new tab (inherits the active tab's cwd)     |
| `Ctrl+M`               | Open the provider/model picker (search, recently used, per-provider sections) |
| `Ctrl+I`               | Open the issues screen (kanban)                    |
| `Ctrl+R`               | Open the PRs screen (kanban)                       |
| `Ctrl+←` / `Ctrl+→`    | Cycle to the previous / next tab (wraps)           |
| `PgUp` / `PgDn`        | Scroll the viewport half a page                    |
| Mouse wheel            | Scroll the viewport                                |
| Mouse click on `│`     | Jump to that position on the scrollbar             |
| `↑` / `↓`              | Navigate lists (session picker, slash menu, modal); at the first line of an empty/unmodified input they walk prior sent messages. In shell mode they walk the shell-only history. |
| `Tab`                  | Auto-complete a path or slash command              |
| `!` (empty prompt)     | Enter [shell mode](#shell-mode)                    |

### Question modal (via MCP tool)

| Key                | Action                                                   |
|--------------------|----------------------------------------------------------|
| `↑` / `↓`          | Move cursor between options                              |
| `Space`            | Toggle selection (pick-many)                             |
| `Enter`            | Commit current tab and advance; submit on the last tab   |
| `←` / `→`, `Tab`   | Switch between question tabs                             |
| `n`                | Add a note to the current question                       |
| Typing on "Enter your own" | Fills the custom answer in place; `Shift+Enter` for a newline |
| `Esc`              | Cancel the dialog                                        |

## Config

`/config` opens a modal with toggles that persist to
`~/.config/ask/ask.json`. Typing filters the list; `↑` / `↓` move, `Enter`
toggles the highlighted entry and writes the file immediately, `Esc`
closes the modal.

> [!NOTE]
> Every toggle writes to disk the moment you press `Enter` — there's no save
> step. Hand-editing `~/.config/ask/ask.json` while ask is closed is fine too.

| Toggle               | Default | What it does                                                                                 |
|----------------------|---------|----------------------------------------------------------------------------------------------|
| Quiet Mode           | on      | When on, assistant text chunks stream silently and the combined turn is rendered once at the end; when off, each chunk is appended as it arrives. |
| Cursor Blink         | on      | Blinking input cursor at a 650ms cadence. Off keeps a steady cursor.                         |
| Render Diffs         | on      | Render `Edit` / `Write` / `NotebookEdit` structured patches as inline colored diffs. Off suppresses the diff block (the edit still happens). |
| Render Tool Output   | off     | Show each tool call and its output inline in history (the Bash command that ran, the Grep results, the file that Read returned, the shell/mcp call codex made). Off keeps tool activity off-screen with only the status line. Quiet Mode overrides this — same contract as Render Diffs. Output is truncated to 20 lines / 2000 chars with a "… N more lines" marker. |
| Skip All Permissions | off     | Tell the agent session to dangerously skip permissions so every tool call bypasses the approval modal. Toggling kills the running session; the next send respawns with the new flag state. |
| Worktree             | off     | Run each session inside an isolated git worktree. Toggling kills the running session; the next send respawns with the new flag state. As an opinionated safety check, enabling this (via toggle or by starting with it already on in the config file) also appends `.claude/worktrees/` to the repo's `.gitignore` if no existing rule already covers that path. No-op outside a git checkout. |

Other fields the config file stores automatically:

- `<provider>.model` — last `Ctrl+M` pick for that provider, used as its default model on the next session.
- `<provider>.apiKey` — saved when the `Ctrl+M` picker prompts for a missing key (the provider's environment variable also works).
- `recentModels` — the `Ctrl+M` picker's "Recently used" list (newest first, capped at 5).
- `<provider>.slashCommands` — cache of discovered slash commands, so the popover has completions before the first real call.

The file is created on first launch and rewritten whenever a value
changes; hand-editing it while `ask` is closed is fine.

## MCP server

When ask launches, its in-process ADK loop provides the `ask_user_question` and `approval_prompt` tools natively, allowing the agent to prompt the user seamlessly via the TUI.

<details>
<summary><strong><code>ask_user_question</code> schema</strong></summary>

```jsonc
{
  "questions": [
    {
      "kind": "pick_one" | "pick_many" | "pick_diagram",
      "prompt": "…",
      "options": [
        { "label": "…", "diagram": "…optional; required for pick_diagram…" }
      ],
      "allow_custom": false  // pick_one and pick_many only
    }
  ]
}
```

Response:

```jsonc
{
  "answers": [
    { "picks": ["…"], "custom": "…optional…", "note": "…optional…" }
  ],
  "cancelled": false
}
```

</details>

<details>
<summary><strong>Diagram format (strict)</strong></summary>

The tool description pins the rules the model must follow for
`pick_diagram` previews:

- Monospace box-drawing characters only: `╭╮╰╯─│├┤┬┴┼`
- Fill blocks: `░` for content areas, `▓` for interactive/accent areas
- No emoji, no tabs, no trailing whitespace
- ≤ 40 columns × ≤ 12 rows (all diagrams in one question are padded
  to the same bounding box before rendering)

</details>

## Debugging

Set `ASK_DEBUG=1` to write a trace to `/tmp/ask.log` (paste/send/agent
stream events, MCP tool dispatch, etc.). Helpful when the TUI feels
stuck.

## License

[MIT](./LICENSE)

---

<p align="center">
  Built on <a href="https://github.com/charmbracelet/bubbletea">Bubble Tea</a>,
  <a href="https://github.com/charmbracelet/bubbles">Bubbles</a>,
  <a href="https://github.com/charmbracelet/lipgloss">Lip Gloss</a>,
  <a href="https://github.com/charmbracelet/glamour">Glamour</a>, and
  <a href="https://github.com/charmbracelet/ultraviolet">Ultraviolet</a>.<br />
  Inspired by <a href="https://github.com/charmbracelet/crush">crush</a>.
</p>
