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

and when looking at the codex protocol, you must always run the following in a temp dir:

* codex app-server generate-json-schema --out .

to generate the app server protocol used communicate with codex; use this to understand how to work with codex

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
| `shell.go`             | Shell-mode execution: `$SHELL -c` fork, stdout/stderr pipe streaming, 100-line cap, cwd capture via `pwd > tmpfile`, pgroup SIGKILL on cancel. |
| `worktree.go`          | `inGitCheckout()` (cwd contains `.git`) and `ensureWorktreeGitignore()`. When worktree is enabled, the latter appends `.claude/worktrees/` to `./.gitignore` unless an existing rule already covers it. Both no-op outside a cwd-level git checkout — we do not walk upward. Called at startup when worktree is on in config, on the `/config` → Worktree toggle going true, and guarding the `--worktree` flag in `ensureProc`. Also exports `validateAskCwd(cwd)` — refuses to start an LLM session when ask is inside `.claude/worktrees/<name>` (with a `/resume` hint naming `<name>`) or in any subdirectory of a git/jj checkout. Plain checkout roots and non-checkout dirs pass. The chat-facing gate fires from `sendToProvider`, `handleCommand` slash dispatch, Ctrl+B, and silently from `Init`. `validateExecutorCwd(args, root)` is the executor-level defense in `prepareProviderSessionAt`: when worktree mode is on at a real checkout, `args.Cwd` must point inside `.claude/worktrees/`. |
| `clipboard.go`         | `wl-paste` integration, returns raw bytes + re-encoded PNG.             |
| `kitty.go`             | Kitty graphics protocol: detection, transmit over `/dev/tty`, Unicode placeholder rows. |
| `kitty_diacritics.go`  | The canonical 297-entry Kitty row/column diacritic table.               |
| `ask_question.go`      | Question modal state, rendering, navigation, submit/cancel flow.        |
| `mcp.go`               | MCP server bridge (Streamable HTTP), `ask_user_question` tool schema + handler. |
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
| `provider_test.go`         | Provider registry + claudeProvider metadata + Send protocol.     |
| `claude_cli_test.go`       | `claudeCLIArgs` / `claudeEnv` flag construction.                 |
| `claude_stream_test.go`    | `readClaudeStream` stream-json → `tea.Msg` translation.          |
| `mcp_test.go`              | MCP bridge conversion + permission/approval wire shapes.         |
| `worktree_test.go`         | `.claude/worktrees/` lifecycle against tmp git repos.            |
| `cwd_guard_test.go`        | `validateAskCwd` / `validateExecutorCwd` plus the entry-path gates (sendToProvider, /resume, Ctrl+B, Init). |
| `session_test.go`          | `~/.claude/projects/` parsing + history loading.                 |
| `config_test.go`           | `loadConfig` / `saveConfig` / ollama validation.                 |
| `update_test.go`           | `model.Update` dispatcher behavior via `fakeProvider`.           |
| `util_test.go` / `paths_test.go` | Pure helpers, path completion, frontmatter parsing.       |

### Testing conventions

- **Every new piece of functionality ships with tests.** This is non-negotiable: when adding a feature, fixing a bug, or refactoring anything in the file table above, add or extend tests in the matching `_test.go` file. A PR that grows the codebase without growing the tests is incomplete.
- Tests must be **behavioral**, not rendering-based. Assert on `model` state, emitted `tea.Msg` values, serialized JSON bytes, file-system state, exec argv — never on styled output strings or view snapshots.
- **No subprocess spawning** except `git` in `worktree_test.go`. Everything else uses the `fakeProvider` from `testhelpers_test.go` or direct function calls.
- Worktree / git tests use `t.TempDir()` + `t.Chdir(...)` so they self-isolate and survive parallel runs.
- HOME-sensitive tests (`session`, `config`, `paths`) call `isolateHome(t)` to pin `$HOME` at a tmp dir so the user's real state is never touched.
- Prefer a few larger scenarios over dozens of trivial one-liners, but do cover each branch of complex functions (see `claudeCLIArgs` and `readClaudeStream` tests for the pattern).
- Keep the full suite under ~1 second — if you add something slow, figure out how to fake it.

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

## Shell mode

- **Activation**: `updateInput` intercepts `msg.Text == "!"` on an empty prompt (not busy, no pending attachments) and flips `m.shellMode`. Subsequent keys route through `updateShellInput` until exit (Esc, Ctrl+C, or two backspaces on empty). On Enter the command is recorded into `m.shellHistory` (separate from `m.inputHistory`), the user text is rendered as a userBar entry, and `startShellCmd` dispatches.
- **Output pipeline**: `startShellCmd` forks `$SHELL -c '<input>\npwd > <tmpfile>'` with `Setpgid: true`. Two goroutines scan stdout and stderr into a channel as `shellLineMsg`; `nextShellStreamCmd` blocks on the first message then non-blockingly drains up to 500 more (and a trailing `shellDoneMsg`) into a single `shellBatchMsg` so large outputs render in chunks, not line-by-line.
- **100-line cap**: the two stream goroutines share a `shellStreamState` with an atomic counter and `marked` bool. Past the cap they stop forwarding lines and emit a one-shot `… output truncated at 100 lines` marker via `CompareAndSwap`. The pipe is kept draining so the child doesn't block on a full kernel buffer.
- **Cwd persistence**: the `pwd > tmpfile` suffix runs after the user's command (newline-separated — works in bash/zsh/fish). The done handler reads the tmpfile, `os.Chdir`s if it differs from the current cwd, then calls `refreshPrompt` and `refreshPathMatches`. Temp file is removed on both success and error paths.
- **Cancel**: `killShellProc` does `Kill(-pgid, SIGKILL)` so children (`sleep 100`, etc.) die with the wrapper. Do NOT combine `Setpgid: true` with `Setsid: true` on the same `SysProcAttr`: the child's `setpgid(2)` returns EPERM when called on a session leader, so exec fails with `operation not permitted`. This is the trap creack/pty falls into if you try to add PTY support naively.
- **Popups**: `View()`'s popup gate is `m.mode == modeInput && !m.busy && !m.shellMode`, so the path picker (from `cd `/`ls ` prefix) and slash popover both stay hidden in shell mode even though the input text might still prefix-match.
- **Curses apps are not supported** — output flows through pipes, so altscreen sequences from vim/htop/less render as raw text in history. Rollback artifact: there was a PTY-based path; removed because `Setpgid + Setsid` collision made non-curses commands fail with EPERM.

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
- `contentFingerprint` must mix in `len(m.history[m.shellOutIdx].text)` whenever a shell output entry is active. The frame cache is keyed on `len(m.history) | m.width`, and shell mode appends streamed lines in place to a single history entry, so without that extra term the cache returns a stale (first-line-only) view until something else (spinner row, window resize) perturbs the key.
