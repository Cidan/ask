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
| `clipboard.go`         | `wl-paste` integration, returns raw bytes + re-encoded PNG.             |
| `kitty.go`             | Kitty graphics protocol: detection, transmit over `/dev/tty`, Unicode placeholder rows. |
| `kitty_diacritics.go`  | The canonical 297-entry Kitty row/column diacritic table.               |
| `ask_question.go`      | Question modal state, rendering, navigation, submit/cancel flow.        |
| `mcp.go`               | MCP server bridge (Streamable HTTP), `ask_user_question` tool schema + handler. |
| `util.go`              | Small helpers (`short`, `humanDuration`, `humanBytes`, `shortCwd`).     |
| `debug.go`             | `ASK_DEBUG=1` → `/tmp/ask.log`.                                         |

## Build, verify, install

```
go build ./...
go vet ./...
go install .
```

The installed binary lives at `$(go env GOPATH)/bin/ask`. No test
suite yet — verify changes by running the binary in a real terminal.
TUI-level feature changes must be exercised by the user; code alone
won't catch layout regressions.

## Bubble Tea wiring

- `Update` is a **value receiver** (`func (m model) Update(...) (tea.Model, tea.Cmd)`). Helpers that need to mutate (`layout`, `appendUser`, `killProc`, etc.) are pointer receivers — Go takes `&m` implicitly on the local copy and the returned `m` propagates back to the runtime.
- `View()` composes everything into one string. When an overlay is needed (slash popover, path picker, modal, scrollbar), we draw onto a `uv.ScreenBuffer` and return its rendered content; otherwise we return the plain body.
- The modal is drawn **on top** of the normal body so the user sees the history underneath — do not early-return a modal-only view.

### Stick-to-bottom rule

`layout()` captures `AtBottom()` **before** any `SetWidth` / `SetHeight` / `SetContent`, then calls `GotoBottom()` only if it was true. Reversing the order causes a 1-row resize to flip the viewport off the bottom and never snap back — a real bug we hit and fixed.

### Markdown cache

Glamour rendering is cached per `historyEntry` in `entry.rendered`. `viewportContent()` fills it lazily on first render; `WindowSizeMsg` invalidates every response entry so wrap recomputes at the new width. Don't re-render in `renderEntry` — that path runs on every spinner tick and every keystroke.

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

## MCP server

- `newMCPBridge()` binds `127.0.0.1:0`, stores the port, builds the `mcp.Server`, registers `ask_user_question`, then returns.
- `start(p *tea.Program)` is called after the program is constructed so the handler can call `p.Send(...)`. Uses `atomic.Pointer[tea.Program]` so the goroutine can read it safely.
- Tool handler packs input questions into the internal `question` type, `p.Send`s an `askToolRequestMsg` with a reply channel, then blocks on the channel.
- `submitAsk` / the Esc cancel path write to `m.askReply` if present; the `/qq` mock path (reply == nil) prints a summary to history instead.

## Conventions

- No new runtime dependencies without asking. We already carry Charm (bubbletea/bubbles/lipgloss/glamour/ultraviolet), the official MCP SDK, and stdlib.
- Only emojis that already exist in the codebase (`✓`, `▸`, `›`, `▏`) — nothing new unless the user asks.
- Comments: default to none. Only add one when a reader cannot derive the reason from the code.
- Debug logging uses `debugLog(format, args...)` and is a no-op unless `ASK_DEBUG=1`. Add one when crossing an async boundary (paste command, MCP handler, claude stream, tool dispatch).

## Known-fragile areas

- `layout()` extra-row math: any change to what appears between viewport and input (chip, thumbnail strip, spacer row) needs the `extra` term in `layout()` and the emission order in `viewBody()` kept in sync.
- Scrollbar column is drawn over `m.width-1`. If any text-rendering style grows a margin or a user-bar width past `m.width-1`, the scrollbar will be overwritten or vice-versa.
- `askToolRequestMsg` is rejected if the modal is already open — only one MCP ask at a time. Double-calls from Claude return `cancelled: true` for the second one.
