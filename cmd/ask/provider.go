package main

import (
	"io"
	"os/exec"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

// askSteeringPromptP1 is the first paragraph of the steering prompt, defining machine pace.
const askSteeringPromptP1 = `You are an AI LLM and can work at super human speeds. Do not think of execution, especially with code and process that can and will be executed by yourself, in human terms and human timelines. Favor offering and doing things yourself instead of telling the user what to run, though still ask the user before you do take action if it makes sense. Remember that you can, and will, execute all tasks much faster than any human ever could, so do not put off work for "a later commit" or "a later version" because you believe the work to be too much.

However, do NOT start doing work, planning modifications, or establishing todo lists if the user is only asking questions, exploring the codebase, or posing possibilities (e.g., asking how something works or what would happen if a change were made). In these situations, you must simply chat, explain, and explore options with the user in plain text. Do not execute or prepare modifications unless the user's intent is explicit and they make a clear, affirmative statement/instruction to proceed with changes.

Examples:
- User intent is informational: "How is the sidebar layout calculated?"
  Your response: Explain the layout math in sidebar.go in plain text. Do NOT propose a plan, write a todo list, or start tool execution for modifications. Just chat and answer.
- User intent is exploratory: "What if we increased the maximum sidebar width?"
  Your response: Discuss the potential visual and layout effects of increasing the clamp, referencing the relevant files. Do NOT start making changes or formulate a plan to change it. Just explore the possibilities with the user.
- User intent is active and explicit: "Increase the maximum sidebar width to 48." OR "Please implement the layout change we just discussed."
  Your response: (This is explicit and affirmative.) Formulate the plan, check workflows, confirm the plan, write the todos, and execute.`

// askSteeringPromptWorkflowCheck instructs the model to pre-validate against project workflows.
const askSteeringPromptWorkflowCheck = `Before you start any multi-step task, checking the project's workflows is a hard precondition, not a suggestion. The moment a request looks like it needs more than one step — before you write a plan, before you reach for the todos tool, before you touch a file — call workflow_list to see this project's defined workflows. If any defined workflow fits the task, even loosely, you MUST surface it to the user and let them decide whether to run it; following an established workflow is always preferred over ad-hoc execution because it follows the team's procedures, keeps output consistent, and tracks progress. Only if no workflow fits do you proceed on your own. Once the user approves a workflow, its steps are pre-cleared — you proceed without further confirmation gates per step. Skipping this check and starting work directly is a failure, and the runtime will interrupt your first todos call to send you back here if you do.`

// askSteeringPromptSideEffects requires the model to confirm plans before changes.
const askSteeringPromptSideEffects = `Whenever the user asks you to build, fix, or change something, you must first investigate the codebase. Then, summarize your proposed solution with detailed rationale (explaining *why* you are taking this approach) and stop. You must explicitly ask the user how they want to proceed (e.g., 'Should I edit these files directly, or would you prefer I run the X workflow?'). Do NOT mutate files or call finalized_plan until the user authorizes a specific path. If the user authorizes you to proceed but is ambiguous about how to execute (for example, saying 'looks good' or 'go ahead'), you must default to executing via a workflow using the 'finalized_plan' tool if a workflow fits; otherwise, proceed with inline edits.

CRITICAL WORKFLOW PLANNING REQUIREMENT:
Workflow execution runs in an isolated subagent context and CANNOT read the current chat history. Therefore, any plan submitted to 'finalized_plan' MUST be a self-contained, code-complete specification. It must include exact file-by-file changes, concrete before/after code blocks, full function signatures, a wire/caller verification matrix (ensuring every newly created function is actively called in runtime paths), anti-stub negative assertions (forbidding empty placeholders or mock returns), and specific behavioral test assertions. High-level summaries or vague bullet points are strictly prohibited.`

// askSteeringPromptInWorkflowSideEffects is used in place of pre-checks and confirmations when already in a workflow.
const askSteeringPromptInWorkflowSideEffects = `You are running as a step in an automated workflow. All changes are pre-cleared by the user — proceed with implementing changes (writing or editing files, modifying configuration, executing commands, etc.) directly without asking for confirmation.`

// askSteeringPromptP4 demands robust, non-thin implementations.
const askSteeringPromptP4 = `You must value correct and complete implementations instead of conservative "thin" wrappers or "v1" shapes. Never, ever think in terms of "first version" or "for now" or "we can expand on this later" as these are human constructs that are not correct for you and your way of working.`

// askSteeringPromptP5 enforces codebase-as-truth and bans guessing.
const askSteeringPromptP5 = `You must never rely on your internal memory or pre-trained knowledge to guide you on how a system works. You must always treat the codebase as the absolute source of truth. You must actively read code, documentation, and search the web to gather context before answering questions or acting. Unless you have directly observed the API, documentation, or code in the current session, you must not state facts or build a solution. Never guess. Never implement or suggest implementations for a system or process in which you have not explicitly read the relevant files yourself.`

// askSteeringPromptP6 ensures turn completion on actual work done.
const askSteeringPromptP6 = `End the turn only when the work you committed to in your text is actually done. Do not write a closing sentence that promises future work ("Let me X next", "I will then Y", "Then I'll commit") without immediately performing that work via tool calls in the same turn. The turn ends the moment you stop emitting tool_use blocks — there is no implicit continuation, no follow-up prompt, no human listening to say "go on." If you genuinely have more work to do, do it now; if you genuinely don't, do not narrate hypothetical follow-ups.`

// steeringPromptFor returns the assembled steering prompt, potentially with an extra worktree pinning clause
// appended when args.Cwd points inside `.claude/worktrees/<name>`. The clause pins the agent to that worktree
// directory so workflow steps (which run unattended with permissions skipped) can't wander into
// the project root or a sibling worktree and modify the wrong tree.
// Chat sessions in a worktree get the same clause; non-worktree
// sessions get the base prompt unchanged so legitimate cross-repo
// reads (CLAUDE.md's /tmp reference clones, etc.) aren't constrained.
func steeringPromptFor(args ProviderSessionArgs) string {
	return providers.SteeringPrompt(providers.SteeringOptions{
		InWorkflow: args.InWorkflow,
		Cwd:        args.Cwd,
	})
}

// Provider is the TUI-facing adapter over an LLM backend. Each
// implementation owns its session lifecycle, message translation into
// tea.Msgs, the commands it supports, and where/how prior sessions are
// persisted. The UI is provider-agnostic; model code dispatches to
// whichever provider the tab runs on.
//
// agentAPIProvider (agent_provider.go) implements it for every entry in
// the pkg/providers registry and is registered from an init(); a new
// LLM backend is a pkg/providers implementation, not a new type here.
type Provider interface {
	// ID is the short stable identifier stored in config, matching
	// providers.Provider.ID.
	ID() string

	// DisplayName is the human-readable name used in UI copy and errors.
	DisplayName() string

	// Capabilities reports optional features so the app knows which
	// fallbacks to engage (hiding /model when the provider has no
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
	// pre-bind for this provider before starting the session, or ""
	// when the provider can't accept a caller-chosen id. When non-empty,
	// ask records the virtual-session row up front so a first-turn
	// cancel cannot orphan the worktree. The returned id is passed back
	// via args.NewSessionID on the StartSession that uses it.
	PreMintSessionID(args ProviderSessionArgs) string

	// NativeSessionID returns the provider-assigned session id carried
	// on a started proc, or "" when the id was pre-minted (the model
	// already knows it). Surfacing a late-assigned id here lets the
	// model capture it at startup instead of at turn end — otherwise an
	// interrupt before the turn completes leaves m.sessionID empty and
	// the next send wrongly takes the fresh-session path.
	NativeSessionID(p *providerProc) string

	// StartSession starts the agent session. Returns the session
	// handle and its event channel on success; err non-nil when the
	// session cannot start (missing credentials, no project, …).
	StartSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error)

	// Send queues a user turn (text + optional image attachments) on a
	// running session.
	Send(p *providerProc, text string, attachments []pendingAttachment) error

	// Interrupt cancels the in-flight turn cooperatively. Returns
	// handled=true when the provider accepted the cancel (the app
	// should keep the session alive and wait for turnCompleteMsg) and
	// handled=false when the provider has no cancel protocol and the
	// caller should fall back to killing the session. Errors always
	// push the caller to the kill fallback.
	Interrupt(p *providerProc) (handled bool, err error)

	// ListSessions enumerates prior sessions rooted at cwd. Backs
	// /resume. Empty slice + nil error is fine when the provider has no
	// persisted history.
	ListSessions(cwd string) ([]sessionEntry, error)

	// LoadHistory replays a prior session's message log as the faithful,
	// mode-independent transcript. The caller projects it to renderable
	// history under the current view modes, so replay and live streaming
	// share one mapping.
	LoadHistory(sessionID string) ([]transcriptItem, error)

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
	// handed here so the target provider can resume from the prior
	// conversation natively, without injecting a prelude into the
	// wire payload.
	Materialize(workspace string, turns []NeutralTurn) (sessionID, cwd string, err error)
}

// ProviderSettings is the per-provider slice of askConfig the UI
// reads/writes through Provider.LoadSettings / SaveSettings. Each
// provider decides where these values live on disk.
type ProviderSettings = providers.ProviderSettings

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

	// AskUserQuestionMCP means the provider needs an external
	// ask_user_question bridge to intercept a built-in question tool.
	// In-process providers ask through the question modal natively
	// and leave this false.
	AskUserQuestionMCP bool

	// PermissionPromptMCP means the provider needs an external
	// permission-prompt callback for tool approvals. In-process
	// providers gate approvals natively and leave this false.
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
	// SubConfig maps an option label to a sub-config key that opens a
	// configuration form instead of applying the option directly.
	// Empty map means no sub-configs.
	SubConfig map[string]string
}

// ProviderSessionArgs bundles everything a provider may need to spawn a
// session or run its init probe. Unused fields are ignored.
//
// SessionID and NewSessionID are mutually exclusive: SessionID is for
// resuming a session the provider has already persisted; NewSessionID
// carries an id ask pre-minted for a fresh session so the worktree+VS
// pairing exists before the first turn lands. Setting both is a
// programmer error.
type ProviderSessionArgs struct {
	Cwd                string
	MCPPort            int
	TabID              int
	Model              string
	Effort             string
	SkipAllPermissions bool
	Worktree           bool
	// WorktreeName carries the tab's already-chosen worktree directory
	// name so a re-dispatch (provider swap, kill+retry) reuses the same
	// worktree instead of creating a new one. Empty on a fresh session.
	WorktreeName        string
	SessionID           string
	NewSessionID        string
	ResumeCwd           string
	InWorkflow          bool
	IsWorkflowFinalStep bool
	// AddedDirs are absolute paths the user has registered with /add-dir.
	// Providers decide how to expose them to the agent. The list is
	// deduped and ordered as the user added them.
	AddedDirs []string
	// ProjectMCP is the project-level MCP server (today: the GitHub
	// MCP slot the github issue provider also piggybacks on), exposed
	// to the chat agent so it can call the same tools ask uses for
	// ctrl+i. nil when the project hasn't configured a token. The chat
	// agent gets this whenever projectConfig.MCP.GitHub.Token is set,
	// regardless of whether issues are wired up. Providers wire this
	// into their MCP client; providers without an MCP surface ignore it.
	ProjectMCP *issueMCPServer
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
// for history persistence. Subtype and StopReason carry *why* a turn
// ended (refusal, max turns, budget, …) when the provider reports it,
// so the UI can surface that rather than a generic "error".
//
// NumTurns, PermissionDenials, and DeferredToolUse make a permission-
// or hook-induced premature stop distinguishable from a clean end_turn;
// without them a workflow step could advance while the model still
// believed work remained.
type providerResult struct {
	IsError           bool
	Result            string
	SessionID         string
	Subtype           string
	StopReason        string
	NumTurns          int
	PermissionDenials int
	DeferredToolUse   *deferredToolUse
}

// deferredToolUse describes a tool call the agent loop stopped on
// without executing, attached so the caller can inspect and optionally
// resume it. Ask has no resume surface for this, so workflow steps
// treat a non-nil value as a step failure.
type deferredToolUse struct {
	ID    string
	Name  string
	Input map[string]any
}

// providerSlashEntry is a dynamically-discovered slash command entry
// (name + description) cached in config so the first render doesn't
// block on discovery.
type providerSlashEntry = config.ProviderSlashEntry

// providerDoneMsg fires at end-of-turn with the provider's result
// summary (session id, final text, error flag).
type providerDoneMsg struct {
	res   providerResult
	err   error
	raw   string
	tabID int
	proc  *providerProc
}

// providerExitedMsg fires after the subprocess reaper collects Wait().
type providerExitedMsg struct {
	err   error
	tabID int
	proc  *providerProc
}

// providerInitLoadedMsg carries the discovered slash commands from a
// ProbeInit run.
type providerInitLoadedMsg struct {
	tabID     int
	slashCmds []providerSlashEntry
	err       error
}

// providerCwdMsg reports the provider's current working directory for
// this session (a provider that runs the session in another cwd, such
// as a worktree, emits this once at start so ask can surface the
// worktree chip).
type providerCwdMsg struct {
	cwd   string
	tabID int
	proc  *providerProc
}

type providerQueuedTurn struct {
	text        string
	attachments []pendingAttachment
}

var providerRegistry []Provider

// registerProvider appends to the registry. First registered wins when
// config points at an unknown ID.
func registerProvider(p Provider) { providerRegistry = append(providerRegistry, p) }

// Providers register here, in one place, so the registry order (and
// with it the default provider for an empty config) is explicit
// instead of an accident of file-name init() ordering.
func init() {
	for _, p := range providers.All() {
		registerProvider(agentAPIProvider{prov: p})
	}
}

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

// providerByIDStrict returns the provider with the given ID and true,
// or nil and false when nothing matches. Unlike providerByID it does
// not fall back to the first registered provider — used by callers
// (the resume-side LastProvider override) that need to detect a
// missing/renamed id and surface it instead of silently swapping
// providers under the user.
func providerByIDStrict(id string) (Provider, bool) {
	if id == "" {
		return nil, false
	}
	for _, p := range providerRegistry {
		if p.ID() == id {
			return p, true
		}
	}
	return nil, false
}

// kill tears the session down: closing stdin ends the in-process
// session goroutine (agentStdin.Close), and a cmd, when one exists, is
// killed. Safe on nil or already-reaped receivers.
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
	{"/workflows", "edit workflow pipelines"},
	{"/skills", "browse skills, agents, and marketplaces"},
	{"/savings", "show bash output token savings"},
}
