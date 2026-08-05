This implements the SUDO_ASKPASS architecture to allow password entry for sudo commands directly inside ask, without exposing it to LLMs, transcripts, or logs.

### Summary
- Adds Unix Domain Socket manager to handle IPC between helper and main process.
- Updates bash tool to inject `ASK_INTERNAL_SUDO_ASKPASS` using `extraEnv` to ensure thread safety.
- Adds Bubble Tea password modal (`EchoPassword`) for secure, native TUI password entry.
- Adds comprehensive test coverage for handshakes, retry logic, and wrappers.

### Motivation
To allow users to run commands requiring sudo inside the tool securely, without hanging on password prompts and without passing passwords through the shell context.

### Testing
- Full test suite passed (`make test`).
- Verified UI retry states and graceful fallbacks.