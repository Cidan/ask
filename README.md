# ask

A terminal chat UI for Claude Code. `ask` spawns the `claude` CLI as a
subprocess and wraps it in a Bubble Tea TUI with inline markdown,
image attachments, a scrollable history, session resume, and a
custom MCP server that replaces the built-in `AskUserQuestion` tool
with a far richer tabbed modal.

![Made with VHS](https://vhs.charm.sh/vhs-4bXH7YlhqAMXxv6lqsjjTs.gif)

## Features

- **Chat with Claude Code** via streaming JSON input/output
- **Tabs** — `Ctrl+T` opens a new tab with its own claude subprocess, shell, MCP bridge, history, session, and cwd; `Ctrl+←` / `Ctrl+→` cycle between tabs; a byobu-style strip at the bottom shows each tab's shortened cwd (prefixed with `▸` when that tab is busy); closing the last tab quits
- **Resume sessions** — `/resume` opens a picker of prior conversations in the current directory
- **Pick the Claude model** — `/model` opens a picker (default / haiku / sonnet / opus / custom) and persists the choice
- **Configurable UI** — `/config` toggles quiet mode, cursor blink, inline diff rendering, and skip-all-permissions; persisted to `~/.config/ask/ask.json`
- **Themes** — pick a palette from `/config` → Theme (15 flavors: `default`, `dracula`, `nord`, `gruvbox`, `tokyo night`, the four Catppuccin variants `latte`/`frappé`/`macchiato`/`mocha` plus the green-leaning Mocha sibling `matcha`, `rose pine`, `fighter` (Monokai Pro), `love` (crush), `hacker` (Matrix), `amber` (CRT)). Backgrounds, foregrounds, borders, and glamour markdown/syntax highlighting all follow the active theme.
- **Inline markdown rendering** with [glamour](https://github.com/charmbracelet/glamour), cached per history entry so typing stays responsive in long chats
- **Live turn status** — spinner line surfaces the tool Claude is running (`Read: file.go`, `Bash: <description>`, `Grep: <pattern>`, `Task: <subagent>`, …)
- **Live todo panel** — `TodoWrite` entries render inline as a bordered box with ☐ / ▸ / ✓ markers while the turn is active
- **Inline diffs** — `Edit` / `Write` / `NotebookEdit` structured patches render as colored unified diffs in history (toggle with `/config`)
- **Input history** — `↑` / `↓` at the first line of the input walks prior sent messages
- **Shell mode** — type `!` on an empty prompt to run a command through your `$SHELL`; stdout/stderr stream into history (capped at 100 lines), `cd` persists, `Esc` / `Ctrl+C` / double-backspace exits, `↑` / `↓` walks shell history separately from LLM input history
- **Image attachments** via clipboard paste
  - `Ctrl+V` on Wayland reads the clipboard with `wl-paste`
  - In Kitty-compatible terminals (Kitty, Ghostty) images render as inline thumbnails using the Kitty graphics protocol with Unicode placeholders
  - In any other terminal they fall back to a text chip
  - Multiple attachments pasted in a row show side-by-side with a bordered preview
- **Draggable scrollbar** in the right column (mouse or `PgUp`/`PgDn`); the viewport sticks to the bottom only while you are at the bottom, so scrolling up during a stream no longer yanks you back
- **Built-in MCP server** exposing two tools:
  - `ask_user_question` — tabbed modal with three question kinds:
    - `pick_one` — single select radio list
    - `pick_many` — multi-select checkboxes
    - `pick_diagram` — radio list with an ASCII-art preview box rendered beside it
    - All kinds support `allow_custom` (appends an Enter-your-own free-text option) and per-question notes (`n`)
  - `approval_prompt` — wired as Claude's `--permission-prompt-tool`, shows a per-tool allow / deny / always-allow modal (concise one-line summary — no field dump) before the tool runs; "always allow" records a session-scoped rule so repeat calls for the same file or command skip the prompt
- **PreToolUse hook** injected at launch that blocks Claude's built-in `AskUserQuestion` and redirects the model to our MCP tool instead

## Install

```
go install github.com/Cidan/ask@latest
```

Requires Go 1.26+ and the `claude` CLI on your `PATH`.

Optional dependencies:

- `wl-clipboard` — for image paste on Wayland (`pacman -S wl-clipboard`, etc.)
- A terminal speaking the Kitty graphics protocol — for inline thumbnails (Kitty, Ghostty). Without one, images still send to Claude; only the local preview falls back to a text chip.

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
| `/model`           | Choose the Claude model (default / haiku / sonnet / opus / custom)    |
| `/config`          | Open the `ask` config modal (see [Config](#config))                   |

Claude's own slash commands (the ones surfaced by `claude` at init) are
merged into the popover alongside these. Typing `/` filters both lists.

### Built-in path commands

`ask` intercepts `cd` and `ls` as local shell-style builtins before the
input is ever sent to Claude, so you can navigate without dropping out
of the TUI.

| Command         | What it does                                                                 |
|-----------------|------------------------------------------------------------------------------|
| `cd [path]`     | Change the working directory. No arg → home. Tilde (`~`, `~/foo`) expands. Kills the live claude subprocess and clears history, since Claude sessions are bound to a cwd. |
| `ls [path]`     | Colorized listing (dirs first, executables, symlinks) with mode, human size, and "X ago" timestamps. No arg → current dir. Globs (`*`, `?`, `[…]`) and tilde expansion both work; `ls path/to/file` prints a single-row entry. |

`Tab` on `cd ` or `ls ` triggers path completion against the current
prefix, same as anywhere else a path is expected.

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
| `Ctrl+C` / `Esc`       | While a turn is running, open a `Stop this turn?` confirm box; on confirm it kills the claude subprocess and a new one spawns on the next send. `Esc` also clears pending attachments when idle. |
| `Ctrl+C` (twice, idle) | Close the current tab. First press shows a `Press ctrl+c again to exit` hint; a second `Ctrl+C` closes the tab (or quits if it was the last). Any other key disarms the hint. |
| `Ctrl+D`               | Close the current tab immediately; quits if it's the last one |
| `Ctrl+T`               | Open a new tab (inherits the active tab's cwd)     |
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

| Toggle               | Default | What it does                                                                                 |
|----------------------|---------|----------------------------------------------------------------------------------------------|
| Quiet Mode           | on      | When on, assistant text chunks stream silently and the combined turn is rendered once at the end; when off, each chunk is appended as it arrives. |
| Cursor Blink         | on      | Blinking input cursor at a 650ms cadence. Off keeps a steady cursor.                         |
| Render Diffs         | on      | Render `Edit` / `Write` / `NotebookEdit` structured patches as inline colored diffs. Off suppresses the diff block (the edit still happens). |
| Skip All Permissions | off     | Pass `--dangerously-skip-permissions` to the `claude` subprocess so every tool call bypasses the approval modal. Toggling kills the running subprocess; the next send respawns with the new flag state. |
| Worktree             | off     | Pass `--worktree` to fresh `claude` invocations so the session runs inside an isolated git worktree. Not passed on `--resume` (resume handles it internally) and not passed to the one-off init probe that caches slash commands. Silently dropped outside a git checkout (claude refuses to start with `--worktree` in that case). Toggling kills the running subprocess; the next send respawns with the new flag state. As an opinionated safety check, enabling this (via toggle or by starting with it already on in the config file) also appends `.claude/worktrees/` to the repo's `.gitignore` if no existing rule already covers that path. No-op outside a git checkout. |

Other fields the config file stores automatically:

- `claude.model` — last `/model` pick; passed as `--model` to the claude subprocess on the next spawn.
- `claude.slashCommands` — cache of slash commands reported by `claude`'s init event, so the popover has completions before the first real call.

The file is created on first launch and rewritten whenever a value
changes; hand-editing it while `ask` is closed is fine.

## MCP server

When ask launches it listens on `127.0.0.1:<random-port>` and exposes
two Streamable-HTTP MCP tools, `ask_user_question` and
`approval_prompt`. The spawned `claude` subprocess is given:

- `--mcp-config` pointing at this endpoint
- `--settings` installing a `PreToolUse` hook that blocks the built-in `AskUserQuestion` tool and redirects the model to `mcp__ask__ask_user_question`
- `--permission-prompt-tool mcp__ask__approval_prompt` so every permission-gated tool call routes through the ask TUI's approval modal

### `ask_user_question` schema

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

### Diagram format (strict)

The tool description pins the rules the model must follow for
`pick_diagram` previews:

- Monospace box-drawing characters only: `╭╮╰╯─│├┤┬┴┼`
- Fill blocks: `░` for content areas, `▓` for interactive/accent areas
- No emoji, no tabs, no trailing whitespace
- ≤ 40 columns × ≤ 12 rows (all diagrams in one question are padded
  to the same bounding box before rendering)

## Debugging

Set `ASK_DEBUG=1` to write a trace to `/tmp/ask.log` (paste/send/claude
stream events, MCP tool dispatch, etc.). Helpful when the TUI feels
stuck — pair it with the in-history stderr surfaced on Claude exit.

## License

See LICENSE.
