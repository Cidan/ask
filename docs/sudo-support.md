# Ask Sudo Support Architecture

This document details the architecture and implementation steps for adding `sudo` support to the `ask` terminal UI. 

## The Problem
By default, `sudo` requires a secure TTY to prompt for a password. Because the `bash` tool in `ask` uses standard pipes (`StdoutPipe`/`StderrPipe`) to capture output, `sudo` execution hangs or fails. Currently, `ask` hard-blocks the word `sudo ` in `agent_tools_bash.go` to prevent background jobs from freezing.

## The Solution
We will use `sudo`'s built-in `SUDO_ASKPASS` mechanism, combined with a single-binary architecture and secure local IPC (Unix Domain Sockets), to prompt the user for their password via a Bubble Tea modal when a background shell command invokes `sudo`.

### Architecture Components

#### 1. The Wrapper Script & Environment
Some versions of `sudo` restrict arguments in `SUDO_ASKPASS` and scrub environment variables for the target command. To ensure our helper runs correctly and inherits the necessary context, the `bash` tool will create a short-lived wrapper script in a secure temporary directory (e.g., `/tmp/ask-sudo-wrapper-<rand>.sh`) with `0700` permissions. 

This wrapper script will execute the `ask` binary with a specific internal environment variable:
```sh
#!/bin/sh
export ASK_INTERNAL_SUDO_ASKPASS=1
export ASK_SUDO_TOKEN="<secure_random_token>"
export ASK_SUDO_SOCKET="/tmp/ask-sudo-<pid>.sock"
exec /path/to/ask
```
The `bash` tool will then inject `SUDO_ASKPASS=/tmp/ask-sudo-wrapper-<rand>.sh` and `SUDO_ASKPASS=1` (to force askpass even without a TTY) into the execution environment of the command.

#### 2. Main Binary Interception (`main.go`)
At the very beginning of `main.go`, before Bubble Tea is initialized, we will check for the `ASK_INTERNAL_SUDO_ASKPASS=1` environment variable. If present, the binary skips the normal startup and jumps directly into a `runAskPassHelper()` function.

#### 3. Secure Unix Domain Socket (IPC)
When the main `ask` application initializes a session, it will:
1. Create a Unix Domain Socket at `/tmp/ask-sudo-<pid>.sock`.
2. Ensure the socket has `0600` permissions (read/write only by the user).
3. Generate a secure, random `ASK_SUDO_TOKEN` and hold it in memory.
4. Start a background goroutine listening on this socket.

#### 4. The Handshake and Authentication
When the `askpass` helper runs, it connects to the socket and sends its token:
`TOKEN: <secure_random_token>`

The main `ask` process's listener reads the token and verifies it against its in-memory token. 
* **Defense in Depth:** We can also use `SO_PEERCRED` (Linux) or `LOCAL_PEERCRED` (macOS) to verify that the connecting process's UID matches the user's UID.
* If authentication fails, the connection is immediately dropped.

#### 5. Triggering the UI
If authentication succeeds, the listener sends a `sudoPasswordRequestedMsg` to the Bubble Tea `Update` loop.
The helper process blocks, waiting for a reply from the socket.

#### 6. The Password Modal
The Bubble Tea view intercepts the `sudoPasswordRequestedMsg`, dims the terminal, and renders a secure text input modal (`bubbles/textinput` with `EchoMode` set to `EchoPassword`). 
The UI informs the user which command/job is requesting elevated privileges.

#### 7. Handoff and Execution
Once the user types their password and presses Enter:
1. A Bubble Tea `Update` handler takes the password string and sends it over the active Unix socket to the waiting helper.
2. The UI closes the modal.
3. The helper receives the password, prints it to `stdout` (`fmt.Println(password)`), and exits with code `0`.
4. `sudo` reads the password from the helper's `stdout` and continues executing the elevated command.

### Security Considerations Summary
* **No Context Leaks:** The password never enters the `agentJob` output buffer and is never seen by the LLM or written to transcript files.
* **Socket Permissions (`0600`):** Prevents other users on the system from connecting to the socket.
* **Cryptographic Token:** Prevents malicious scripts running as the same user from triggering fake sudo prompts, as they won't know the randomly generated `ASK_SUDO_TOKEN`.
* **Peer Credentials (`SO_PEERCRED`):** Enforces OS-level strict user identity checks on the IPC connection.

## Implementation Steps
1. Add IPC socket setup, cleanup, and random token generation to the `agentRunShell` lifecycle or main `tea.Program` startup.
2. Add `main.go` short-circuit logic for `ASK_INTERNAL_SUDO_ASKPASS`.
3. Implement `runAskPassHelper()` to connect, authenticate, request, and wait.
4. Update `agent_tools_bash.go` to generate the secure wrapper script and set `SUDO_ASKPASS` and `SUDO_ASKPASS=1`. Remove the `sudo ` block.
5. Create the Bubble Tea UI components (modal, messages, update loops) to handle the password prompt and socket dispatch.