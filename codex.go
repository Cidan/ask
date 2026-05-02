package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// codexUsage snapshots what codex has reported about plan rate limits
// and thread token usage. Codex ships these numbers directly in the
// JSON-RPC stream (account/rateLimits/updated, thread/tokenUsage/updated),
// so unlike claude we don't need an out-of-band plugin to fetch them.
type codexUsage struct {
	primary            codexRateLimitWindow
	secondary          codexRateLimitWindow
	hasRateLimits      bool
	contextTokens      int
	modelContextWindow int
}

type codexRateLimitWindow struct {
	usedPercent int
	resetsAt    time.Time
}

// codexUsageMsg is emitted when codex sends account/rateLimits/updated.
// primary and secondary are individually nullable on the wire —
// parseCodexRateLimitWindow returns a zero-valued window for either.
type codexUsageMsg struct {
	primary   codexRateLimitWindow
	secondary codexRateLimitWindow
	proc      *providerProc
}

// codexContextMsg is emitted when codex sends thread/tokenUsage/updated.
// window == 0 means codex didn't report modelContextWindow; the chip
// falls back to modelContextLimit(m.providerModel) in that case.
type codexContextMsg struct {
	tokens int
	window int
	proc   *providerProc
}

// codexProvider implements the Provider interface for OpenAI's codex CLI
// running in `codex app-server` mode. The app-server speaks JSON-RPC 2.0
// over stdio (newline-delimited), much like claude's stream-json. Every
// per-session piece of state lives on the providerProc payload.
//
// MVP scope: plain-text user turns in, agent text out. No images, no
// resume, no model/effort pickers, no native worktree. Richer features
// get layered on top.
type codexProvider struct{}

func init() { registerProvider(codexProvider{}) }

func (codexProvider) ID() string          { return "codex" }
func (codexProvider) DisplayName() string { return "Codex" }

func (codexProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Resume:       true,
		ModelPicker:  true,
		EffortPicker: true,
	}
}

// ModelPicker exposes /model for codex. Options are fetched live
// from a short-lived `codex app-server model/list` probe so the list
// reflects whatever the account has access to right now — model
// releases don't strand users on stale cached names. The probe costs
// ~0.5s; callers (the /model flow and the Ctrl+B switcher) invoke
// ModelPicker once when the picker opens and reuse the snapshot for
// the picker's lifetime, so this isn't called on every render.
// Failure silently degrades to the "default" row + AllowCustom so
// users can still type a model id by hand.
func (codexProvider) ModelPicker() ProviderPicker {
	opts := []string{"default"}
	if live, err := fetchCodexModels(); err == nil {
		opts = append(opts, live...)
	} else {
		debugLog("codex ModelPicker: live fetch failed: %v", err)
	}
	return ProviderPicker{
		Prompt:      "Select Codex model",
		Options:     opts,
		AllowCustom: true,
	}
}

// EffortOptions matches codex's ReasoningEffort enum ("default" is the
// sentinel used throughout the picker UI meaning "don't set it"; codex
// itself has no "default" value and picks one server-side).
func (codexProvider) EffortOptions() []string {
	return []string{"default", "none", "minimal", "low", "medium", "high", "xhigh"}
}

func (codexProvider) BaseSlashCommands() []slashCmd {
	return []slashCmd{
		{"/resume", "resume a previous Codex session"},
		{"/new", "start a new Codex session"},
		{"/clear", "start a new Codex session"},
		{"/model", "select the Codex model"},
		{"/effort", "select the Codex reasoning effort"},
		{"/add-dir", "add a directory the agent may access"},
	}
}

// ProbeInit forks a short-lived codex app-server, runs skills/list,
// and returns a providerInitLoadedMsg so the existing update.go
// handler persists the entries via persistSlashCmdsCmd. Re-runs on
// every tab Init so a new skill installed between launches shows up
// immediately, matching claude's per-launch cache-refresh pattern.
// Errors are non-fatal: the picker still works with the static
// BaseSlashCommands list even if discovery failed this round.
// PreMintSessionID returns "" — codex assigns its own threadID at
// handshake (recorded on state.threadID) and the app-server protocol
// has no caller-chosen id slot.
func (codexProvider) PreMintSessionID(_ ProviderSessionArgs) string { return "" }

func (codexProvider) ProbeInit(args ProviderSessionArgs) tea.Cmd {
	return func() tea.Msg {
		skills, err := fetchCodexSkills(args.Cwd)
		if err != nil {
			debugLog("codex ProbeInit skills/list: %v", err)
			return providerInitLoadedMsg{tabID: args.TabID, err: err}
		}
		return providerInitLoadedMsg{tabID: args.TabID, slashCmds: skills}
	}
}

func (codexProvider) ListSessions(cwd string) ([]sessionEntry, error) {
	return loadCodexSessions(cwd)
}

func (codexProvider) LoadHistory(sessionID string, opts HistoryOpts) ([]historyEntry, error) {
	return loadCodexHistory(sessionID, opts)
}

func (codexProvider) Materialize(workspace string, turns []NeutralTurn) (string, string, error) {
	return writeCodexSyntheticSession(workspace, turns)
}

func (codexProvider) LoadSettings() ProviderSettings {
	cfg, _ := loadConfig()
	return ProviderSettings{
		Model:         cfg.Codex.Model,
		Effort:        cfg.Codex.Effort,
		SlashCommands: cfg.Codex.SlashCommands,
	}
}

func (codexProvider) SaveSettings(s ProviderSettings) error {
	cfg, _ := loadConfig()
	cfg.Codex.Model = s.Model
	cfg.Codex.Effort = s.Effort
	cfg.Codex.SlashCommands = s.SlashCommands
	return saveConfig(cfg)
}

// codexState is stashed on providerProc.payload. Send reads threadID
// and bumps nextID on every user turn. The stdin mutex serialises
// writes from the Update goroutine (Send) and the stream-reader
// goroutine (approval responders), which would otherwise risk
// interleaving JSON frames on the pipe. done is closed when
// readCodexStream exits so approval responder goroutines can abort
// instead of blocking forever on a dead subprocess.
type codexState struct {
	mu            sync.Mutex
	stdin         io.Writer
	threadID      string
	currentTurnID string
	nextID        uint64
	tabID         int
	effort        string
	skipPerms     bool
	done          chan struct{}
}

// codexCLIArgs returns the argv passed to the codex binary. Broken out so
// tests can assert shape without forking a process.
//
// AddedDirs translate to a -c sandbox_workspace_write.writable_roots
// TOML override (the same config key codex's interactive --add-dir
// flag writes to). Each path is strconv.Quote'd so spaces or quotes
// don't break codex's TOML parser.
//
// IssueMCP translates to a -c mcp_servers.<name>.url override pair.
// Codex pulls the bearer token from an env var (not from config), so
// we point it at codexIssueMCPBearerEnv and codexEnv exports the
// matching key.
func codexCLIArgs(args ProviderSessionArgs) []string {
	out := []string{"app-server", "--listen", "stdio://"}
	if len(args.AddedDirs) > 0 {
		quoted := make([]string, len(args.AddedDirs))
		for i, d := range args.AddedDirs {
			quoted[i] = strconv.Quote(d)
		}
		out = append(out, "-c",
			"sandbox_workspace_write.writable_roots=["+strings.Join(quoted, ",")+"]")
	}
	if mcp := args.IssueMCP; mcp != nil && mcp.Name != "" && mcp.URL != "" {
		base := "mcp_servers." + mcp.Name
		out = append(out, "-c", base+".url="+strconv.Quote(mcp.URL))
		if codexIssueBearerToken(mcp) != "" {
			out = append(out, "-c",
				base+".bearer_token_env_var="+strconv.Quote(codexIssueMCPBearerEnv))
		}
	}
	return out
}

// codexIssueMCPBearerEnv is the env-var name codex reads for the
// streamable-HTTP bearer token. Hard-coded — codex's MCP config takes
// only one bearer slot per server and ask only injects one issue MCP
// per session, so a single well-known env var is enough.
const codexIssueMCPBearerEnv = "ASK_ISSUE_MCP_BEARER"

// codexIssueBearerToken extracts the raw bearer token from an issue
// MCP descriptor's Authorization header (`Bearer <token>`). Returns
// empty string when the descriptor has no Authorization header — the
// caller then omits the bearer_token_env_var override, leaving the
// MCP server unauthenticated (which is fine for public servers).
func codexIssueBearerToken(mcp *issueMCPServer) string {
	if mcp == nil {
		return ""
	}
	auth := mcp.Headers["Authorization"]
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimPrefix(auth, prefix)
}

// codexEnv returns the env block for the codex subprocess. Inherits
// the parent process env and layers on the bearer-token env var when
// an issue MCP with auth is configured. Pulled out so tests assert
// the env without spawning a process.
func codexEnv(args ProviderSessionArgs) []string {
	env := os.Environ()
	if tok := codexIssueBearerToken(args.IssueMCP); tok != "" {
		env = append(env, codexIssueMCPBearerEnv+"="+tok)
	}
	return env
}

// handshake request ids — fixed so the handshake scanner can match by id.
const (
	codexInitializeID  = 1
	codexThreadStartID = 2
	// codexTurnStartBaseID is the first id used for user-turn requests; the
	// handshake occupies 1 and 2.
	codexTurnStartBaseID = 3
)

// codexHandshakeTimeout bounds how long StartSession will wait for the
// thread/start response. Observed latency is ~200ms; a 15s ceiling prevents
// a hung codex from locking the tea Update loop forever. The sync handshake
// blocks Update briefly; moving it fully async is deferred (it would require
// a queued-turn path in Send and a ready/error msg type).
const codexHandshakeTimeout = 15 * time.Second

func (codexProvider) StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	cmd := exec.Command("codex", codexCLIArgs(args)...)
	cmd.Env = codexEnv(args)
	if args.Cwd != "" {
		cmd.Dir = args.Cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr := &stderrBuf{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	state := &codexState{
		nextID:    codexTurnStartBaseID,
		stdin:     stdin,
		tabID:     args.TabID,
		effort:    args.Effort,
		skipPerms: args.SkipAllPermissions,
		done:      make(chan struct{}),
	}
	proc := &providerProc{cmd: cmd, stdin: stdin, stderr: stderr, payload: state}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<22)

	// Arm a watchdog that force-kills the subprocess if the handshake hasn't
	// completed in time. Without it, a hung or mis-auth'd codex would block
	// the tea Update loop indefinitely.
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-handshakeDone:
		case <-time.After(codexHandshakeTimeout):
			_ = cmd.Process.Kill()
		}
	}()

	err = codexHandshake(stdin, sc, state, args)
	close(handshakeDone)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("codex handshake: %w (stderr: %s)", err, stderr.String())
	}

	ch := make(chan tea.Msg, 32)
	go readCodexStream(sc, proc, ch)
	return proc, ch, nil
}

// codexHandshake exchanges initialize / initialized / thread/start with the
// app-server and blocks until the thread/start response arrives. It mutates
// state.threadID on success. Frames we see during handshake that aren't the
// thread/start response are discarded — any notifications interleaved here
// (initialize response, mcp startup) have no user-visible effect on a fresh
// thread.
func codexHandshake(stdin io.Writer, sc *bufio.Scanner, state *codexState, args ProviderSessionArgs) error {
	if err := codexWriteJSON(stdin, codexRequest(codexInitializeID, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "ask", "version": "0.1"},
	})); err != nil {
		return err
	}
	if err := codexWriteJSON(stdin, codexNotification("initialized", nil)); err != nil {
		return err
	}
	method := "thread/start"
	params := map[string]any{}
	if args.Cwd != "" {
		params["cwd"] = args.Cwd
	}
	if args.SessionID != "" {
		// Resume: thread/resume takes the thread id directly and
		// returns the same shape as thread/start (thread{id}). Our
		// stored id carries forward; conversation history is
		// replayed from disk by loadCodexHistory earlier in the flow.
		method = "thread/resume"
		params["threadId"] = args.SessionID
	}
	// Skip-all-permissions mirrors claude's --dangerously-skip-permissions:
	// never ask the user, give the agent full access. Applied per-thread
	// here so a subsequent toggle-off + new proc reverts to the default
	// approval flow without any extra plumbing.
	if args.SkipAllPermissions {
		params["approvalPolicy"] = "never"
		params["sandbox"] = "danger-full-access"
	}
	if err := codexWriteJSON(stdin, codexRequest(codexThreadStartID, method, params)); err != nil {
		return err
	}
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		idf, ok := ev["id"].(float64)
		if !ok || int(idf) != codexThreadStartID {
			// Frames we discard here are the initialize response (id=1) and
			// startup notifications (mcpServer/startupStatus/updated,
			// thread/started). None require client action today, but log
			// them so a future auth/capability handshake doesn't fail
			// invisibly.
			debugLog("codex handshake: skipped frame %s", sc.Text())
			continue
		}
		if rpcErr, ok := ev["error"].(map[string]any); ok {
			msg, _ := rpcErr["message"].(string)
			if msg == "" {
				msg = "unknown"
			}
			return fmt.Errorf("thread/start rejected: %s", msg)
		}
		result, _ := ev["result"].(map[string]any)
		thread, _ := result["thread"].(map[string]any)
		tid, _ := thread["id"].(string)
		if tid == "" {
			return fmt.Errorf("thread/start response missing thread.id")
		}
		state.threadID = tid
		debugLog("codex handshake: threadID=%s", tid)
		return nil
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("codex exited before thread/start response")
}

// Interrupt sends `turn/interrupt` so codex winds the turn down
// cleanly without dropping the thread. Returns handled=false when
// we have no turn id to target — nothing in flight — so the caller
// knows to just kill instead. Kept under the state mutex so the
// write doesn't interleave with a concurrent Send or approval reply.
func (codexProvider) Interrupt(p *providerProc) (bool, error) {
	state, _ := p.payload.(*codexState)
	if state == nil {
		return false, nil
	}
	state.mu.Lock()
	tid := state.threadID
	turnID := state.currentTurnID
	id := state.nextID
	state.nextID++
	state.mu.Unlock()
	if tid == "" || turnID == "" {
		return false, nil
	}
	return true, codexWriteJSONLocked(state, codexRequest(id, "turn/interrupt", map[string]any{
		"threadId": tid,
		"turnId":   turnID,
	}))
}

func (codexProvider) Send(p *providerProc, text string, attachments []pendingAttachment) error {
	state, _ := p.payload.(*codexState)
	if state == nil {
		return fmt.Errorf("codex: session not initialized")
	}
	state.mu.Lock()
	id := state.nextID
	state.nextID++
	tid := state.threadID
	state.mu.Unlock()
	if tid == "" {
		return fmt.Errorf("codex: session not initialized")
	}

	params := map[string]any{
		"threadId": tid,
		"input":    codexUserInput(text, attachments),
	}
	// Effort is set per-turn rather than per-thread because codex's
	// schema only accepts it on TurnStartParams (not ThreadStartParams).
	// "default" (UI sentinel) means "don't override codex's own default".
	if effort := codexEffortForWire(state, p); effort != "" {
		params["effort"] = effort
	}
	return codexWriteJSONLocked(state, codexRequest(id, "turn/start", params))
}

// codexEffortForWire pulls the effort we should send this turn. Right
// now it just reads a per-proc setting we track on the state so every
// turn after a /effort pick lands with the right value; the indirection
// leaves room to read overrides from elsewhere in the future.
func codexEffortForWire(state *codexState, _ *providerProc) string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.effort == "" || state.effort == "default" {
		return ""
	}
	return state.effort
}

// codexUserInput converts the prompt + any image attachments into
// codex's UserInput array. Text lives in TextUserInput; images land in
// ImageUserInput with a base64 data URL so no temp files are needed
// and codex can inline the bytes directly.
func codexUserInput(text string, attachments []pendingAttachment) []map[string]any {
	out := []map[string]any{{"type": "text", "text": text}}
	for _, a := range attachments {
		if len(a.data) == 0 {
			continue
		}
		mime := a.mime
		if mime == "" {
			mime = "image/png"
		}
		url := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(a.data)
		out = append(out, map[string]any{"type": "image", "url": url})
	}
	return out
}

// readCodexStream translates codex's JSON-RPC wire protocol into the
// provider-neutral tea.Msg set. Each event emitted here carries the proc
// pointer so the UI can drop messages from stale subprocesses.
//
// Server-initiated requests (approvals) are dispatched to a separate
// handler that forwards them to the approval modal and writes the
// response back on stdin via the state mutex. Closing state.done on
// exit releases any responder goroutines still waiting on a reply.
func readCodexStream(sc *bufio.Scanner, proc *providerProc, ch chan tea.Msg) {
	defer close(ch)
	if state, ok := proc.payload.(*codexState); ok && state != nil && state.done != nil {
		defer close(state.done)
	}
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if _, _, isRequest := codexServerRequest(ev); isRequest {
			msgs, _ := handleCodexServerRequest(proc, ev)
			for _, msg := range msgs {
				ch <- msg
			}
			continue
		}
		for _, msg := range codexEventToMsgs(ev, proc) {
			ch <- msg
		}
	}
	var err error
	if proc.cmd != nil {
		err = proc.cmd.Wait()
	}
	ch <- providerExitedMsg{err: err, proc: proc}
}

// codexEventToMsgs translates a single codex JSON-RPC frame into zero or
// more tea.Msg values. Pure function so tests exercise it without spawning
// goroutines or subprocesses. Request/response frames (id without method)
// are ignored — we only care about notifications.
func codexEventToMsgs(ev map[string]any, proc *providerProc) []tea.Msg {
	method, _ := ev["method"].(string)
	if method == "" {
		return nil
	}
	switch method {
	case "turn/started":
		// Remember the active turn id so cancelTurn can send
		// `turn/interrupt` cooperatively instead of killing the proc.
		if state, _ := proc.payload.(*codexState); state != nil {
			params, _ := ev["params"].(map[string]any)
			turn, _ := params["turn"].(map[string]any)
			if tid, _ := turn["id"].(string); tid != "" {
				state.mu.Lock()
				state.currentTurnID = tid
				state.mu.Unlock()
			}
		}
		return []tea.Msg{streamStatusMsg{status: "thinking…", proc: proc}}
	case "item/started":
		params, _ := ev["params"].(map[string]any)
		item, _ := params["item"].(map[string]any)
		if status := codexItemStatus(item); status != "" {
			return []tea.Msg{streamStatusMsg{status: status, proc: proc}}
		}
		return nil
	case "item/completed":
		params, _ := ev["params"].(map[string]any)
		item, _ := params["item"].(map[string]any)
		itype, _ := item["type"].(string)
		switch itype {
		case "agentMessage":
			if text, _ := item["text"].(string); text != "" {
				return []tea.Msg{assistantTextMsg{text: text, proc: proc}}
			}
		case "fileChange":
			// The UI toggle + quiet-mode guard lives on the consumer
			// side (see toolDiffMsg handler in update.go) so codex
			// gets the exact same `renderDiffs && !quietMode` gate
			// claude already follows.
			diffs := codexFileChanges(item)
			if len(diffs) == 0 {
				return nil
			}
			msgs := make([]tea.Msg, 0, len(diffs))
			for _, d := range diffs {
				msgs = append(msgs, toolDiffMsg{
					filePath: d.Path,
					hunks:    d.Hunks,
					proc:     proc,
				})
			}
			return msgs
		case "commandExecution", "mcpToolCall":
			return codexToolOutputMsgs(item, proc)
		}
		return nil
	case "turn/completed":
		params, _ := ev["params"].(map[string]any)
		threadID, _ := params["threadId"].(string)
		turn, _ := params["turn"].(map[string]any)
		status, _ := turn["status"].(string)
		// Clear the cached turn id so a stray Ctrl+C between turns
		// doesn't send turn/interrupt for a turn that already ended
		// (which would then sit forever waiting for a turn/completed
		// that will never re-fire).
		if state, _ := proc.payload.(*codexState); state != nil {
			state.mu.Lock()
			state.currentTurnID = ""
			state.mu.Unlock()
		}
		res := providerResult{SessionID: threadID}
		if status != "" && status != "completed" {
			res.IsError = true
			if turnErr, _ := turn["error"].(map[string]any); turnErr != nil {
				res.Result, _ = turnErr["message"].(string)
			}
		}
		return []tea.Msg{
			providerDoneMsg{res: res, proc: proc},
			turnCompleteMsg{proc: proc},
		}
	case "account/rateLimits/updated":
		params, _ := ev["params"].(map[string]any)
		rl, _ := params["rateLimits"].(map[string]any)
		if rl == nil {
			return nil
		}
		return []tea.Msg{codexUsageMsg{
			primary:   parseCodexRateLimitWindow(rl["primary"]),
			secondary: parseCodexRateLimitWindow(rl["secondary"]),
			proc:      proc,
		}}
	case "thread/tokenUsage/updated":
		params, _ := ev["params"].(map[string]any)
		tu, _ := params["tokenUsage"].(map[string]any)
		if tu == nil {
			return nil
		}
		tokens := codexContextTokens(tu)
		window := jsonInt(tu["modelContextWindow"])
		return []tea.Msg{codexContextMsg{tokens: tokens, window: window, proc: proc}}
	case "error":
		params, _ := ev["params"].(map[string]any)
		errObj, _ := params["error"].(map[string]any)
		msg, _ := errObj["message"].(string)
		if msg == "" {
			msg = "codex reported an unknown error"
		}
		// When willRetry is true, codex is retrying server-side — keep the
		// turn alive so the spinner doesn't flip to idle under the retry.
		// Surface a status line instead.
		willRetry, _ := params["willRetry"].(bool)
		if willRetry {
			return []tea.Msg{streamStatusMsg{status: "retrying: " + truncate(msg, 60), proc: proc}}
		}
		return []tea.Msg{
			providerDoneMsg{res: providerResult{IsError: true, Result: msg}, proc: proc},
			turnCompleteMsg{proc: proc},
		}
	}
	return nil
}

// codexToolOutputMsgs converts a completed commandExecution or
// mcpToolCall item into a toolCallMsg + toolResultMsg pair. The
// consumer in update.go gates rendering on
// `renderToolOutput && !quietMode` — same contract as toolDiffMsg.
//
// Codex bundles call info and output on a single item/completed frame,
// so we always have both halves when we get here; we emit them as two
// messages to match claude's wire shape (tool_use on assistant, then
// tool_result on the next user turn) and keep update.go's handling
// uniform across providers.
func codexToolOutputMsgs(item map[string]any, proc *providerProc) []tea.Msg {
	itype, _ := item["type"].(string)
	name, input := codexToolCallSummary(itype, item)
	output, isError := codexToolResultSummary(item)
	var msgs []tea.Msg
	if name != "" || len(input) > 0 {
		msgs = append(msgs, toolCallMsg{name: name, input: input, proc: proc})
	}
	if output != "" || isError {
		msgs = append(msgs, toolResultMsg{name: name, output: output, isError: isError, proc: proc})
	}
	return msgs
}

// codexToolCallSummary pulls the human-readable name and input payload
// off the item. For commandExecution that's "shell" + {command, cwd};
// for mcpToolCall it's "mcp: server/tool" + the arguments map.
func codexToolCallSummary(itype string, item map[string]any) (string, map[string]any) {
	switch itype {
	case "commandExecution":
		input := map[string]any{}
		if cmd, _ := item["command"].(string); cmd != "" {
			input["command"] = cmd
		}
		if cwd, _ := item["cwd"].(string); cwd != "" {
			input["cwd"] = cwd
		}
		return "shell", input
	case "mcpToolCall":
		server, _ := item["server"].(string)
		tool, _ := item["tool"].(string)
		name := "mcp"
		switch {
		case server != "" && tool != "":
			name = "mcp: " + server + "/" + tool
		case tool != "":
			name = "mcp: " + tool
		}
		args, _ := item["arguments"].(map[string]any)
		return name, args
	}
	return itype, nil
}

// codexToolResultSummary pulls the output string and error flag off a
// completed item. Codex varies field names across item types (output /
// stdout / result), so we try several before giving up.
func codexToolResultSummary(item map[string]any) (string, bool) {
	isError := false
	if status, _ := item["status"].(string); status != "" && status != "completed" && status != "applied" && status != "succeeded" {
		isError = true
	}
	if code, ok := item["exitCode"].(float64); ok && code != 0 {
		isError = true
	}
	for _, key := range []string{"output", "result", "stdout"} {
		if s, _ := item[key].(string); s != "" {
			return s, isError
		}
	}
	// Some variants split stdout/stderr. Concatenate when present.
	stdout, _ := item["stdout"].(string)
	stderr, _ := item["stderr"].(string)
	switch {
	case stdout != "" && stderr != "":
		return stdout + "\n" + stderr, isError
	case stderr != "":
		return stderr, true
	}
	return "", isError
}

// codexItemStatus maps an item/started payload to a single-line status
// shown next to the spinner. Returns "" when the item type has no useful
// status (e.g. userMessage is our own echo).
func codexItemStatus(item map[string]any) string {
	itype, _ := item["type"].(string)
	switch itype {
	case "reasoning":
		return "reasoning…"
	case "agentMessage":
		return "responding…"
	case "commandExecution":
		if cmd, _ := item["command"].(string); cmd != "" {
			return "shell: " + truncate(cmd, 60)
		}
		return "shell"
	case "fileChange":
		return "editing files…"
	case "mcpToolCall":
		server, _ := item["server"].(string)
		tool, _ := item["tool"].(string)
		switch {
		case server != "" && tool != "":
			return "mcp: " + server + "/" + tool
		case tool != "":
			return "mcp: " + tool
		default:
			return "mcp"
		}
	case "plan":
		return "planning…"
	}
	return ""
}

// codexRequest builds a JSON-RPC 2.0 request frame.
// parseCodexRateLimitWindow decodes codex's RateLimitWindow shape. Nil
// or malformed entries produce a zero-valued window — the chip treats
// zero-percent + zero-time as "no data for this slot" and hides it.
func parseCodexRateLimitWindow(raw any) codexRateLimitWindow {
	m, ok := raw.(map[string]any)
	if !ok || m == nil {
		return codexRateLimitWindow{}
	}
	pct := jsonInt(m["usedPercent"])
	var reset time.Time
	// resetsAt is an int64 unix seconds timestamp (optional on the wire).
	if secs := jsonInt(m["resetsAt"]); secs > 0 {
		reset = time.Unix(int64(secs), 0).UTC()
	}
	return codexRateLimitWindow{usedPercent: pct, resetsAt: reset}
}

func codexRequest(id uint64, method string, params any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
}

// codexNotification builds a JSON-RPC 2.0 notification frame (no id). A nil
// params is elided from the wire frame to match the optional-params shape
// in the codex schema.
func codexNotification(method string, params any) map[string]any {
	m := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		m["params"] = params
	}
	return m
}

// fetchCodexModels runs a short-lived codex app-server and asks it
// for the current model list. Returns the non-hidden model ids,
// ordered default-first when codex marks one isDefault=true so the
// picker lands sensibly. A 15s ceiling bounds the worst case when
// codex takes too long to initialize; anything longer falls back to
// the cached list.
func fetchCodexModels() ([]string, error) {
	cmd := exec.Command("codex", codexCLIArgs(ProviderSessionArgs{})...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	killed := false
	cleanup := func() {
		if killed {
			return
		}
		killed = true
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	defer cleanup()

	// Watchdog: kill the subprocess if it never sends the response
	// line. The done channel closes on the happy path so the
	// goroutine exits immediately instead of sleeping for the full
	// timeout — same pattern StartSession uses for its handshake.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-time.After(codexHandshakeTimeout):
			_ = cmd.Process.Kill()
		}
	}()

	if err := codexWriteJSON(stdin, codexRequest(1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "ask", "version": "0.1"},
	})); err != nil {
		return nil, err
	}
	if err := codexWriteJSON(stdin, codexNotification("initialized", nil)); err != nil {
		return nil, err
	}
	if err := codexWriteJSON(stdin, codexRequest(2, "model/list", map[string]any{})); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		idf, ok := ev["id"].(float64)
		if !ok || int(idf) != 2 {
			continue
		}
		if rpcErr, ok := ev["error"].(map[string]any); ok {
			msg, _ := rpcErr["message"].(string)
			return nil, fmt.Errorf("model/list rejected: %s", msg)
		}
		result, _ := ev["result"].(map[string]any)
		data, _ := result["data"].([]any)
		return codexParseModelList(data), nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("codex exited before model/list response")
}

// fetchCodexSkills queries the running codex app-server for the
// skills available at cwd. Each returned skill maps to a slash
// command: typing `/<skill-name>` lets the user invoke it from the
// prompt. Disabled skills are dropped so the picker only advertises
// what codex will actually run. Uses the same short-lived-probe +
// watchdog pattern as fetchCodexModels.
func fetchCodexSkills(cwd string) ([]providerSlashEntry, error) {
	cmd := exec.Command("codex", codexCLIArgs(ProviderSessionArgs{})...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	killed := false
	cleanup := func() {
		if killed {
			return
		}
		killed = true
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	defer cleanup()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-time.After(codexHandshakeTimeout):
			_ = cmd.Process.Kill()
		}
	}()

	if err := codexWriteJSON(stdin, codexRequest(1, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "ask", "version": "0.1"},
	})); err != nil {
		return nil, err
	}
	if err := codexWriteJSON(stdin, codexNotification("initialized", nil)); err != nil {
		return nil, err
	}
	params := map[string]any{}
	if cwd != "" {
		params["cwds"] = []string{cwd}
	}
	if err := codexWriteJSON(stdin, codexRequest(2, "skills/list", params)); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		idf, ok := ev["id"].(float64)
		if !ok || int(idf) != 2 {
			continue
		}
		if rpcErr, ok := ev["error"].(map[string]any); ok {
			msg, _ := rpcErr["message"].(string)
			return nil, fmt.Errorf("skills/list rejected: %s", msg)
		}
		result, _ := ev["result"].(map[string]any)
		data, _ := result["data"].([]any)
		return codexParseSkillsList(data), nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("codex exited before skills/list response")
}

// codexParseSkillsList flattens the per-cwd SkillsListEntry records
// into a single deduped list of slash entries. Duplicates (same skill
// visible from multiple cwds) collapse to the first occurrence. A
// missing description falls back to shortDescription, matching how
// codex itself surfaces skills in its own UI.
func codexParseSkillsList(data []any) []providerSlashEntry {
	seen := map[string]struct{}{}
	var out []providerSlashEntry
	for _, entry := range data {
		em, _ := entry.(map[string]any)
		if em == nil {
			continue
		}
		skills, _ := em["skills"].([]any)
		for _, s := range skills {
			sm, _ := s.(map[string]any)
			if sm == nil {
				continue
			}
			if enabled, has := sm["enabled"].(bool); has && !enabled {
				continue
			}
			name, _ := sm["name"].(string)
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			desc, _ := sm["description"].(string)
			if desc == "" {
				desc, _ = sm["shortDescription"].(string)
			}
			out = append(out, providerSlashEntry{Name: name, Description: desc})
		}
	}
	return out
}

// codexParseModelList pulls model ids out of a /model/list data
// array. Hidden models are skipped; the default model (isDefault=true)
// moves to the front so the picker opens on it.
func codexParseModelList(data []any) []string {
	var defaults, rest []string
	for _, m := range data {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if hidden, _ := mm["hidden"].(bool); hidden {
			continue
		}
		id, _ := mm["id"].(string)
		if id == "" {
			continue
		}
		if isDefault, _ := mm["isDefault"].(bool); isDefault {
			defaults = append(defaults, id)
			continue
		}
		rest = append(rest, id)
	}
	return append(defaults, rest...)
}

// codexWriteJSON marshals msg as a single newline-terminated JSON frame —
// the on-wire format codex app-server uses for stdio:// transport.
// Direct callers must not share the pipe with another goroutine; prefer
// codexWriteJSONLocked when a codexState mutex is available.
func codexWriteJSON(w io.Writer, msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// codexWriteJSONLocked writes a frame under the state mutex so the
// Send path (user turns) and the stream-reader path (approval
// responses) can never interleave bytes on the pipe.
func codexWriteJSONLocked(state *codexState, msg map[string]any) error {
	if state == nil {
		return fmt.Errorf("codex: no state")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stdin == nil {
		return fmt.Errorf("codex: stdin closed")
	}
	return codexWriteJSON(state.stdin, msg)
}
