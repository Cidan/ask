package main

import (
	"io"
	"os/exec"

	tea "charm.land/bubbletea/v2"
)

// Provider is an agent-CLI backend (claude, codex, gemini, …). Each
// implementation owns its own subprocess lifecycle, wire-protocol
// translation into tea.Msgs, the commands it supports, and where/how
// prior sessions are persisted. The UI is provider-agnostic; model code
// dispatches to whichever provider was selected at startup.
//
// Adding a new provider means implementing this interface and calling
// registerProvider from an init() — no changes to update/view/ask code.
type Provider interface {
	// ID is the short stable identifier stored in config ("claude",
	// "codex", …).
	ID() string

	// DisplayName is the human-readable name used in UI copy and errors.
	DisplayName() string

	// Capabilities reports optional features so the app knows which
	// fallbacks to engage (externally-managed worktrees for providers
	// without a --worktree flag, hiding /model if the provider has no
	// picker, etc.).
	Capabilities() ProviderCapabilities

	// ModelPicker returns the /model picker for this provider. Empty
	// Options means /model is hidden.
	ModelPicker() ProviderPicker

	// EffortOptions returns the /effort choices. Empty means /effort is
	// hidden.
	EffortOptions() []string

	// BaseSlashCommands returns the always-present provider-specific
	// slash commands (everything except the app-level /config).
	BaseSlashCommands() []slashCmd

	// ProbeInit returns an async command that discovers extra slash
	// commands (plugins, skills, user directories). Return nil when the
	// provider has no dynamic discovery.
	ProbeInit(args ProviderSessionArgs) tea.Cmd

	// PreMintSessionID returns a fresh native session id ask should
	// pre-bind for this provider before forking, or "" when the
	// provider can't accept a caller-chosen id. When non-empty, ask
	// records the virtual-session row up front so a first-turn cancel
	// cannot orphan the worktree. The returned id is passed back via
	// args.NewSessionID on the StartSession that uses it (claude:
	// --session-id <uuid>).
	PreMintSessionID(args ProviderSessionArgs) string

	// StartSession forks the agent CLI in streaming mode. Returns the
	// process handle and its event channel on success; err non-nil on
	// launch failure.
	StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error)

	// Send writes a user turn (text + optional image attachments) to a
	// running session's stdin.
	Send(p *providerProc, text string, attachments []pendingAttachment) error

	// Interrupt cancels the in-flight turn cooperatively. Returns
	// handled=true when the provider accepted the cancel (the app
	// should keep the proc alive and wait for turn/completed) and
	// handled=false when the provider has no cancel protocol and the
	// caller should fall back to killing the subprocess. Errors
	// always push the caller to the kill fallback.
	Interrupt(p *providerProc) (handled bool, err error)

	// ListSessions enumerates prior sessions rooted at cwd. Backs
	// /resume. Empty slice + nil error is fine when the provider has no
	// persisted history.
	ListSessions(cwd string) ([]sessionEntry, error)

	// LoadHistory replays a prior session's message log as history
	// entries the UI can render directly.
	LoadHistory(sessionID string, opts HistoryOpts) ([]historyEntry, error)

	// LoadSettings returns the provider's persisted UI settings (model,
	// effort, cached slash commands). Each provider owns its own config
	// section so /model, /effort and slash-command caches never trample
	// another provider's stored state.
	LoadSettings() ProviderSettings

	// SaveSettings persists the provider's UI settings to disk.
	SaveSettings(s ProviderSettings) error

	// Materialize writes a fresh provider-native session file seeded
	// with `turns` (a provider-neutral user/assistant transcript) and
	// returns the new session id plus the cwd the provider resolves
	// sessions under. Used by virtual-session translation: when the
	// current tab's provider has no native mapping for a VS, the
	// source provider's turns are distilled to []NeutralTurn and
	// handed here so --resume / thread/resume can continue from the
	// prior conversation natively, without injecting a prelude into
	// the wire payload.
	Materialize(workspace string, turns []NeutralTurn) (sessionID, cwd string, err error)
}

// ProviderSettings is the per-provider slice of askConfig the UI
// reads/writes through Provider.LoadSettings / SaveSettings. Each
// provider decides where these values live on disk.
type ProviderSettings struct {
	Model         string
	Effort        string
	SlashCommands []providerSlashEntry
}

// ProviderCapabilities flags optional features a provider supports. The
// app consults these to decide whether to engage ask-side fallbacks
// (e.g., hiding /resume for providers that can't resume).
type ProviderCapabilities struct {
	// Resume means /resume makes sense for this provider.
	Resume bool

	// ModelPicker means /model is exposed.
	ModelPicker bool

	// EffortPicker means /effort is exposed.
	EffortPicker bool

	// AskUserQuestionMCP means the provider needs the MCP
	// ask_user_question bridge + the PreToolUse redirect hook to
	// intercept the built-in AskUserQuestion tool. Providers with
	// native question-asking in their own protocol leave this false.
	AskUserQuestionMCP bool

	// PermissionPromptMCP means the provider consumes a
	// --permission-prompt-tool callback via the MCP bridge for tool
	// approvals. Providers that model approvals natively leave this
	// false.
	PermissionPromptMCP bool
}

// ProviderPicker describes a /model-style picker.
type ProviderPicker struct {
	// Prompt is the title shown in the picker modal.
	Prompt string
	// Options is the list of selectable labels in display order.
	Options []string
	// AllowCustom appends an "Enter your own" free-text row.
	AllowCustom bool
	// SubConfig maps an option label to a sub-config key (e.g. the
	// Claude "ollama (configure...)" label → "ollama" which triggers
	// the ollama host/model config form). Empty map means no
	// sub-configs.
	SubConfig map[string]string
}

// ProviderSessionArgs bundles everything a provider may need to spawn a
// session or run its init probe. Unused fields are ignored.
//
// SessionID and NewSessionID are mutually exclusive: SessionID is for
// resuming a session the provider has already persisted (claude:
// --resume); NewSessionID carries an id ask pre-minted for a fresh
// session (claude: --session-id) so the worktree+VS pairing exists
// before the first turn lands. Setting both is a programmer error.
type ProviderSessionArgs struct {
	Cwd                string
	MCPPort            int
	TabID              int
	Model              string
	Effort             string
	OllamaHost         string
	OllamaModel        string
	SkipAllPermissions bool
	Worktree           bool
	SessionID          string
	NewSessionID       string
	ResumeCwd          string
	// PluginDir is passed to claude as --plugin-dir so the embedded
	// ask-usage plugin's SessionStart hook fires. Empty when the plugin
	// failed to extract at startup.
	PluginDir string
	// AddedDirs are absolute paths the user has registered with /add-dir.
	// Providers translate these into their native equivalents (claude:
	// --add-dir; codex: sandbox_workspace_write.writable_roots). The
	// list is deduped and ordered as the user added them.
	AddedDirs []string
	// IssueMCP is the project's issue-provider MCP server (the same
	// backend ask uses for ctrl+i), exposed to the chat agent so it can
	// search/read issues directly. nil when no provider is configured
	// or the configured one is missing credentials. Providers wire this
	// into their native MCP plumbing — claude appends it to its
	// --mcp-config; providers without an MCP surface ignore it.
	IssueMCP *issueMCPServer
}

// HistoryOpts narrows what the history replay renders. ToolOutput is
// the same tri-state ("full" | "short" | "off") the live UI honours so
// replay matches whatever the user currently sees in fresh sessions.
type HistoryOpts struct {
	RenderDiffs bool
	ToolOutput  toolOutputMode
	QuietMode   bool
}

// providerProc is an opaque subprocess handle. The UI uses it as an
// equality token for dispatching stream messages; the provider owns
// the process and any provider-specific extras go on payload.
type providerProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *stderrBuf
	payload any
}

// providerResult carries the end-of-turn summary from a provider.
// SessionID is the provider-side session identifier used as the key
// for history persistence.
type providerResult struct {
	IsError   bool
	Result    string
	SessionID string
}

// providerSlashEntry is a dynamically-discovered slash command entry
// (name + description) cached in config so the first render doesn't
// block on discovery.
type providerSlashEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// providerDoneMsg fires at end-of-turn with the provider's result
// summary (session id, final text, error flag).
type providerDoneMsg struct {
	res  providerResult
	err  error
	raw  string
	proc *providerProc
}

// providerExitedMsg fires after the subprocess reaper collects Wait().
type providerExitedMsg struct {
	err  error
	proc *providerProc
}

// providerInitLoadedMsg carries the discovered slash commands from a
// ProbeInit run.
type providerInitLoadedMsg struct {
	tabID     int
	slashCmds []providerSlashEntry
	err       error
}

// providerCwdMsg reports the provider's current working directory for
// this session (providers that switch cwd — e.g., claude --worktree —
// emit this once at init so ask can surface the worktree chip).
type providerCwdMsg struct {
	cwd  string
	proc *providerProc
}

type providerQueuedTurn struct {
	text        string
	attachments []pendingAttachment
}

type providerStartDoneMsg struct {
	tabID        int
	seq          uint64
	providerID   string
	proc         *providerProc
	streamCh     chan tea.Msg
	worktreeName string
	err          error
	turn         providerQueuedTurn
}

var providerRegistry []Provider

// registerProvider is called from each provider's init() so the app can
// list them at startup. First registered wins when config points at an
// unknown ID.
func registerProvider(p Provider) { providerRegistry = append(providerRegistry, p) }

// providerByID returns the provider with the given ID, or the first
// registered provider when nothing matches (including the empty id).
func providerByID(id string) Provider {
	for _, p := range providerRegistry {
		if p.ID() == id {
			return p
		}
	}
	if len(providerRegistry) > 0 {
		return providerRegistry[0]
	}
	return nil
}

// kill terminates the subprocess and closes stdin. Safe on nil or
// already-reaped receivers.
func (p *providerProc) kill() {
	if p == nil {
		return
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// appBuiltinSlashCmds is the set of slash commands not owned by any
// provider (they configure the app itself).
var appBuiltinSlashCmds = []slashCmd{
	{"/config", "configure ask"},
}
