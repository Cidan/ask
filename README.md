# ask

A terminal chat UI for Claude Code. `ask` spawns the `claude` CLI as a
subprocess and wraps it in a Bubble Tea TUI with inline markdown,
image attachments, a scrollable history, session resume, and a
custom MCP server that replaces the built-in `AskUserQuestion` tool
with a far richer tabbed modal.

![Made with VHS](https://vhs.charm.sh/vhs-4bXH7YlhqAMXxv6lqsjjTs.gif)

## Features

- **Chat with Claude Code** via streaming JSON input/output
- **Resume sessions** — `/resume` opens a picker of prior conversations in the current directory
- **Inline markdown rendering** with [glamour](https://github.com/charmbracelet/glamour), cached per history entry so typing stays responsive in long chats
- **Image attachments** via clipboard paste
  - `Ctrl+V` on Wayland reads the clipboard with `wl-paste`
  - In Kitty-compatible terminals (Kitty, Ghostty) images render as inline thumbnails using the Kitty graphics protocol with Unicode placeholders
  - In any other terminal they fall back to a text chip
  - Multiple attachments pasted in a row show side-by-side with a bordered preview
- **Draggable scrollbar** in the right column (mouse or `PgUp`/`PgDn`); the viewport sticks to the bottom only while you are at the bottom, so scrolling up during a stream no longer yanks you back
- **Built-in MCP server** exposing a single tool, `ask_user_question`, that presents a tabbed modal with three question kinds:
  - `pick_one` — single select radio list
  - `pick_many` — multi-select checkboxes
  - `pick_diagram` — radio list with an ASCII-art preview box rendered beside it
  - All kinds support `allow_custom` (appends an Enter-your-own free-text option) and per-question notes (`n`)
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

| Command            | What it does                               |
|--------------------|--------------------------------------------|
| `/resume`          | Pick a prior session in this directory     |
| `/new` / `/clear`  | Discard history and start a fresh session  |

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

### Keybindings

| Key                    | Action                                             |
|------------------------|----------------------------------------------------|
| `Enter`                | Send message / confirm                             |
| `Shift+Enter`, `Ctrl+J`| Insert newline in the input                        |
| `Ctrl+V`               | Paste image from clipboard                         |
| `Ctrl+C` / `Esc`       | Cancel the live turn (kills the claude subprocess; a new one spawns on the next send). `Esc` also clears pending attachments when not mid-turn. |
| `Ctrl+D`               | Quit                                               |
| `PgUp` / `PgDn`        | Scroll the viewport half a page                    |
| Mouse wheel            | Scroll the viewport                                |
| Mouse click on `│`     | Jump to that position on the scrollbar             |
| `↑` / `↓`              | Navigate lists (session picker, slash menu, modal) |
| `Tab`                  | Auto-complete a path or slash command              |

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

## MCP server

When ask launches it listens on `127.0.0.1:<random-port>` and exposes a
single Streamable-HTTP MCP tool, `ask_user_question`. The spawned
`claude` subprocess is given a `--mcp-config` pointing at this
endpoint, plus a `--settings` layer that installs a `PreToolUse` hook
blocking the built-in `AskUserQuestion` tool and redirecting the model
to `mcp__ask__ask_user_question`.

### Tool schema

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
