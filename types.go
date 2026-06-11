package main

import (
	"os/exec"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
)

type slashCmd struct {
	name string
	desc string
}

type sessionEntry struct {
	id      string
	cwd     string
	preview string
	modTime time.Time

	// virtualSessionID pairs the picker entry with a VirtualSession
	// in sessions.json. Downstream handlers use it to look the VS
	// back up so the current provider picks the right native id — or
	// translates when no mapping exists yet.
	virtualSessionID string
}

type viewMode int

const (
	modeInput viewMode = iota
	modeSessionPicker
	modeAskQuestion
	modeApproval
	modeConfig
	modeProviderSwitch
)

type streamStatusMsg struct {
	status string
	proc   *providerProc
}

// usageMsg carries the running context size in tokens pulled from an
// assistant event's message.usage block. Emitted once per assistant
// message; update.go uses it to keep model.lastUsageTokens fresh for
// the ctx chip segment.
type usageMsg struct {
	tokens int
	proc   *providerProc
}

// providerModelMsg carries the model name claude reports in its
// system/init event. The providerChip prefers this over the user's
// selected alias ("opus[1m]") because claude resolves shorthands to a
// full id ("claude-opus-4-7-1m"), which is what we need to pick the
// right context-window denominator.
type providerModelMsg struct {
	model string
	proc  *providerProc
}

type assistantTextMsg struct {
	text string
	proc *providerProc
}

type turnCompleteMsg struct {
	proc *providerProc
}

type todoItem struct {
	Content    string
	ActiveForm string
	Status     string
}

type todoUpdatedMsg struct {
	todos []todoItem
	proc  *providerProc
}

type bgTaskStartedMsg struct {
	taskID string
	// toolUseID is the assistant message tool_use_id of the Task call
	// that spawned this background worker, taken from the task_started
	// stream event. Empty when the CLI didn't include it. Stashed
	// alongside taskID so the SubagentStop hook can reap stuck entries
	// even when its agent_id is the tool_use_id rather than the task_id
	// (claude's CLI uses different identifier namespaces for the two).
	toolUseID string
	proc      *providerProc
}

type bgTaskEndedMsg struct {
	taskID string
	proc   *providerProc
}

// hookSubagentStartMsg is delivered when claude's SubagentStart hook
// fires. It covers every Task-spawned sub-agent (foreground and
// background); the bgTasks map is driven by the background-only
// task_started stream event, so this message is observability-only
// unless agent_id happens to equal a task_id we're already tracking.
type hookSubagentStartMsg struct {
	tabID     int
	agentID   string
	agentType string
}

// hookSubagentStopMsg is delivered when claude's SubagentStop hook
// fires. We use it as an authoritative cleanup signal: agent_id is
// matched against either the bgTasks key (task_id) or the per-entry
// tool_use_id captured at task_started, plugging the case where
// task_notification never arrives. For foreground sub-agents nothing
// matches and the message is a no-op, which is fine.
type hookSubagentStopMsg struct {
	tabID     int
	agentID   string
	agentType string
}

// cancelWatchdogMsg fires some seconds after a cooperative cancel
// (Provider.Interrupt reported handled=true). If the same proc is
// still busy when it arrives, the UI treats the interrupt as lost
// and kills the subprocess as a fallback so the user never gets
// stuck staring at "cancelling…" forever.
type cancelWatchdogMsg struct {
	proc *providerProc
}

type diffHunk struct {
	oldStart int
	oldLines int
	newStart int
	newLines int
	lines    []string
}

type toolDiffMsg struct {
	filePath string
	hunks    []diffHunk
	proc     *providerProc
}

// toolCallMsg reports that a tool is about to run. Emitted when the
// provider announces the call (Claude tool_use block, Codex
// commandExecution/mcpToolCall item). The UI renders it according to
// the tool-output mode and quiet flag. id/background are populated for
// Claude tool_use blocks (codex leaves them zero); update.go uses them
// to decide whether to suppress the matching ack result in non-full
// modes.
type toolCallMsg struct {
	id         string
	name       string
	input      map[string]any
	background bool
	proc       *providerProc
}

// toolResultMsg carries a tool's output back to the UI. Rendered with
// the same gate as toolCallMsg. background mirrors the originating
// tool_use's run_in_background flag (set by the stream layer when the
// tool_use_id matches a previously-seen background call) so the UI can
// drop the ack-only payload in short/off modes without dropping
// foreground results.
type toolResultMsg struct {
	toolUseID  string
	name       string
	output     string
	isError    bool
	background bool
	proc       *providerProc
}

type stderrBuf struct {
	mu   sync.Mutex
	data []byte
}

func (s *stderrBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append(s.data, p...)
	if len(s.data) > 8192 {
		s.data = s.data[len(s.data)-8192:]
	}
	return len(p), nil
}

func (s *stderrBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.data)
}

type historyKind int

const (
	histPrerendered historyKind = iota
	histResponse
	histUser
)

type historyEntry struct {
	kind     historyKind
	text     string
	rendered string

	// wrapped is the soft-wrapped slice of rendered lines for the
	// width recorded in wrappedFor. It is the only thing chatView
	// reads when slicing the visible window: caching it per entry
	// means width changes only re-wrap visited entries instead of
	// the entire history (the perf win of the lazy viewport).
	//
	// wrappedFor == 0 means "cache invalid, recompute from rendered
	// (and re-glamour from text if rendered is empty)". Any non-zero
	// width may be served from the cache as long as it equals the
	// caller's requested width.
	wrapped    []string
	wrappedFor int

	// rawLines is the cached newline+1 count of the source string
	// (rendered if non-empty, else text). It's the cheap fallback
	// used by the chatView when an entry is off-screen and has not
	// been wrapped at the current width — refreshChatTotals reads
	// it once per frame per entry, so without this cache a 20 MB
	// history would cost a full O(text-size) walk per frame.
	//
	// rawLinesFor records len(text) at the moment rawLines was
	// computed. A change (shell streaming, in-place truncation)
	// invalidates the cache transparently on the next read.
	rawLines    int
	rawLinesFor int
}

type sessionsLoadedMsg struct {
	tabID    int
	sessions []sessionEntry
	err      error
}

type historyLoadedMsg struct {
	tabID     int
	sessionID string
	// virtualSessionID tags the load so Update can pair the reply
	// with the current VS. The translation path fires a load against
	// a source provider's native id while m.sessionID is still empty,
	// which would otherwise fail the sessionID gate.
	virtualSessionID string
	entries          []historyEntry
	err              error
	silent           bool
}

type frameCache struct {
	vpFP   string
	vpView string

	vbFP      string
	vbWithBar string
}

type closeTabMsg struct {
	tabID int
}

// focusTabMsg asks the app layer to switch the active tab to the
// one with the matching id. Used by the workflow `f` dispatch when a
// live workflow tab already exists for the issue: rather than spawn
// a duplicate, we focus the existing one.
type focusTabMsg struct {
	tabID int
}

// spawnWorkflowTabMsg asks the app layer to open a new tab dedicated
// to running `Workflow` against `Source`, rooted at `Cwd`. Issued by
// the issues screen when the user hits `f` (Source carries an
// issueRef) or by the chat screen when the user hits Ctrl+F (Source
// carries a chat-transcript snapshot). The app handler creates the
// tab, attaches a workflowRunState to its model, and dispatches the
// first step.
type spawnWorkflowTabMsg struct {
	OriginTabID int
	Cwd         string
	Workflow    workflowDef
	Source      workflowSource
}

// workflowRunStartStepMsg fires when a workflow tab is ready to
// dispatch its current step. Init issues it for step 0 right after
// the model is constructed; the runner re-issues it after each step
// completes so the next step's proc starts from a clean tea.Cmd
// boundary rather than chaining inside an Update branch.
type workflowRunStartStepMsg struct {
	tabID int
}

// workflowRunStepDoneMsg signals one step finished. The handler
// either advances StepIndex and emits another workflowRunStartStepMsg,
// or finalises the run (markFinal + read-only completion banner).
// err is nil on a clean step exit; non-nil means the proc errored
// out and the chain aborts at this step.
type workflowRunStepDoneMsg struct {
	tabID int
	err   error
}

// startupResumeMsg is fired by Init when the model was pre-seeded with
// a virtualSessionID by `ask resume <vid>` on the CLI. It's the same
// trigger as picking the row from the /resume picker — Update routes it
// straight into resumeVirtualSession so the cross-provider translation
// path stays in one place.
type startupResumeMsg struct {
	tabID int
	vsID  string
}

type model struct {
	id  int
	cwd string

	provider Provider

	input     textarea.Model
	chat      chatView
	spinner   spinner.Model
	renderer  *glamour.TermRenderer
	sessionID string
	resumeCwd string

	// sessionMinted is set when ask just pre-minted m.sessionID via
	// Provider.PreMintSessionID and the next fork must announce it as a
	// new session (claude: --session-id) instead of resuming. Cleared
	// once the dispatch closure has captured args, so subsequent forks
	// (after a kill, a retry) take the --resume branch since the
	// session file now exists on disk.
	sessionMinted bool

	// virtualSessionID pins the tab to a VirtualSession in
	// ~/.config/ask/sessions.json so upserts accumulate native session
	// ids under one id across providers. Set on /resume or first
	// providerDoneMsg; cleared by /new and /clear.
	virtualSessionID string
	busy             bool
	width            int
	height           int

	history []historyEntry

	mode      viewMode
	menuIdx   int
	sessions  []sessionEntry
	pickerIdx int

	// screen names which top-level surface the tab is currently
	// showing (chat vs issues vs future surfaces). Defaults to
	// screenAsk; switchScreen flips it. Distinct from mode, which
	// gates modal/picker overlays *within* a screen — both can be
	// non-default at the same time (e.g. an MCP modal popping while
	// the user is browsing issues will draw over the issues body).
	screen screenID
	// issues holds the per-tab state for the issues screen. Lazily
	// instantiated by newTab/newTestModel; the screen handler also
	// defends against nil so tests that bypass construction still
	// render correctly.
	issues *issuesState

	// prs holds the per-tab state for the PR screen. Same shape as
	// `issues` (issuesState is provider-agnostic — the kanban view
	// only consults the IssueProvider interface) but installs
	// githubPRProvider, draws against m.prs in the screen handler,
	// and gates the `m` merge keybind on the IssueMerger capability
	// interface. Splitting the state field means flipping between
	// Ctrl+I and Ctrl+P preserves each screen's filter / cursor /
	// page cache independently.
	prs *issuesState

	// mergePRConfirming gates the merge confirmation modal on the
	// PR screen. Same mode-gated pattern as cancelTurnConfirming:
	// modalOpen() returns true while it's set; the screen handler
	// only opens the modal after a successful Mergeable() pre-flight
	// reports canMerge=true.
	mergePRConfirming bool
	// mergePRChoice is the selection cursor in the modal. 0=No
	// (default — merge is destructive), 1=Yes.
	mergePRChoice int
	// mergePRItem snapshots the PR being merged at confirm time so
	// the dispatched Merge() call doesn't re-read a possibly-mutated
	// row from the cache. Cleared once the action resolves.
	mergePRItem issue
	// mergePRReason carries the optional pre-flight warning shown
	// in the confirm modal subtitle (e.g. "checks failing but
	// mergeable" for unstable). Empty when the state is clean.
	mergePRReason string

	pathMatches []string
	pathIdx     int

	status   string
	streamCh chan tea.Msg
	proc     *providerProc

	procStarting bool
	procStartSeq uint64
	queuedTurns  []providerQueuedTurn

	pending     []pendingAttachment
	nextImageID uint32

	scrollbarDragging bool

	// Mouse text selection in the chat viewport. Anchor/focus are in
	// *content* coordinates (row counts from the top of the rendered
	// viewport content, not the screen) so the selection survives
	// scrolling and resizes. selDragging is true while the left button
	// is held; selActive is true once the user releases with a non-zero
	// range, until something clears it (right-click copy, mode change,
	// new turn). buildVisualCopyText/selectionContains/selectionRange
	// consume these.
	selDragging bool
	selActive   bool
	selAnchor   cellPos
	selFocus    cellPos

	// toast carries transient top-right notifications (e.g. "copied to
	// clipboard"). Always non-nil after newTab so we don't have to
	// nil-check on every Update tick.
	toast *toastModel

	askQuestions        []question
	askAnswers          []qAnswer
	askTab              int
	askCursor           int
	askEditing          askEditField
	askNoteBackup       string
	askReply            chan askReply
	askMode             askMode
	askConfirmingCancel bool
	askCancelChoice     int

	approvalTool   string
	approvalInput  map[string]any
	approvalReply  chan approvalReply
	approvalChoice int

	cancelTurnConfirming bool
	cancelTurnChoice     int

	closeTabConfirming bool
	closeTabChoice     int

	shellMode         bool
	shellBsArmed      bool
	shellCh           chan tea.Msg
	shellProc         *exec.Cmd
	shellOutIdx       int
	shellHistory      []string
	shellHistoryIdx   int
	shellHistoryDraft string

	configFilter string
	configCursor int

	configThemePickerActive bool
	configThemeCursor       int
	configThemeBackup       string

	// configProviderPickerActive toggles the /config sub-picker that
	// sets cfg.Provider (default for new tabs). Uses the theme-picker
	// pattern so Esc restores the original value.
	configProviderPickerActive bool
	configProviderCursor       int
	configProviderBackup       string

	// configMemoryPickerActive toggles the /config → Memory sub-picker.
	// Cursor is over the submenu rows (Enabled, Gemini API key, Neo4j
	// host/port/user/password/database); future rows drop in by
	// appending to memoryPickerItems without reshaping the picker state
	// machine.
	configMemoryPickerActive bool
	configMemoryCursor       int

	// configMemoryFieldEditing names the row whose inline editor is
	// currently active (e.g. "geminiKey", "neo4jHost", "neo4jPassword").
	// Empty string means the editor is closed and the cursor is moving
	// over rows. While non-empty, key presses and pastes append to
	// configMemoryFieldDraft and Enter persists the draft to the
	// matching cfg.Memory.* field.
	configMemoryFieldEditing string
	configMemoryFieldDraft   string

	// /config → <API provider> sub-picker: API key + base URL for the
	// in-process providers (deepseek, anthropic, openai). One shared
	// state machine keyed by provider id — same shape as the Memory
	// picker (row cursor, inline field editor with draft). Empty
	// configAPIProviderPicker means closed.
	configAPIProviderPicker       string
	configAPIProviderCursor       int
	configAPIProviderFieldEditing string
	configAPIProviderFieldDraft   string

	// /config now layers into Global Options (existing knobs) vs
	// Project Options (per-cwd issue provider). configGlobalPicker-
	// Active is the gate for the Global submenu — it carries the
	// rows that lived directly on the top-level /config row list
	// before the layering. configProjectPickerActive opens the
	// per-cwd surface; its inline editor uses the same pattern as
	// the memory picker (configProjectFieldEditing/Draft).
	configGlobalPickerActive bool
	configGlobalCursor       int

	configProjectPickerActive bool
	configProjectCursor       int
	configProjectFieldEditing string
	configProjectFieldDraft   string

	// configKeybindingsPickerActive toggles the /config → Keybindings
	// sub-picker. Rows are the actions from actionMeta. Pressing Enter
	// on a row flips configKeybindingsCapturing — the next non-Esc
	// keypress is recorded as the new binding. Esc during capture
	// cancels without persisting.
	configKeybindingsPickerActive bool
	configKeybindingsCursor       int
	configKeybindingsCapturing    bool
	configKeybindingsError        string

	// Ctrl+B starts at the provider list (Level 0). Picking a provider
	// with model options advances to Level 1, which reuses the shared
	// ask/model modal rather than a separate switcher-specific editor.
	// Esc from that modal pops back to the provider list; applying a
	// choice switches the current tab only and leaves persisted defaults
	// alone.
	providerSwitchLevel   int
	providerSwitchProvIdx int

	themeName string

	quietMode          bool
	cursorBlink        bool
	renderDiffs        bool
	toolOutputMode     toolOutputMode
	skipAllPermissions bool
	worktree           bool
	worktreeName       string

	// addedDirs lists absolute paths the user has registered with
	// /add-dir for the current tab. Each entry surfaces to the active
	// provider on the next launch (claude: --add-dir, codex:
	// sandbox_workspace_write.writable_roots config override). Cleared by
	// /new and /clear. The /add-dir handler kills the live proc so the
	// next user turn relaunches with these wired in via --resume.
	addedDirs []string

	turnBuffer []string

	lastContentFP string

	fc *frameCache

	// rendererWidth records the wrap width m.renderer was built
	// for. ensureEntryWrapped checks it before glamour-rendering
	// an entry so a viewport resize transparently re-glamours at
	// the new width (matching table/code-block column layout to
	// the actual visible columns).
	rendererWidth int

	providerModel     string
	providerEffort    string
	providerSlashCmds []providerSlashEntry

	inputHistory []string
	historyIdx   int
	historyDraft string

	exitArmed bool

	todos []todoItem

	// bgTasks tracks live background workers (Agent tool calls launched
	// with run_in_background=true). Keyed on task_id from the
	// task_started stream event; the value is the optional tool_use_id
	// of the Task call that spawned the worker, used as a fallback for
	// the SubagentStop hook reap path because claude's CLI sometimes
	// reports agent_id as the tool_use_id rather than the task_id.
	bgTasks map[string]string

	// lastUsageTokens is the running context size reported by the
	// most recent assistant event's message.usage block. Divided by
	// modelContextLimit(modelForContext) for the ctx chip segment.
	lastUsageTokens int

	// modelForContext is the model id from claude's system/init event,
	// preferred over providerModel for the context-limit denominator
	// because claude resolves aliases ("opus[1m]") to fully-qualified
	// ids. Falls back to providerModel before the init event lands.
	modelForContext string

	// currentTurn accumulates the per-turn signal (prompt, tools,
	// files, response text) that flushMemoryTurn writes to memmy on
	// turnCompleteMsg. The maps stay nil between turns; resetMemory-
	// Turn populates them when sendToProvider dispatches a new user
	// prompt.
	currentTurn memoryTurn

	// workflowRun, when non-nil, marks this tab as a workflow runner.
	// The textarea is replaced with a read-only banner showing the
	// current step + provider/model; user input is suppressed; the
	// tab cannot pop ask/approval modals (they auto-cancel). Cleared
	// on the final step's completion (with `complete` banner) or on
	// fatal step error (with `failed` banner). Workflow tabs share
	// the rest of the chat machinery (viewport, scrollback, copy
	// selection) so the user can read the chain's output as it
	// streams in.
	workflowRun *workflowRunState

	// workflowsBuilder, when non-nil, holds the per-tab state of the
	// workflows builder screen (levels, cursors, in-flight name and
	// prompt drafts). Lazily allocated on screen entry; cleared on
	// exit. Only consulted while m.screen == screenWorkflows.
	workflowsBuilder *workflowsBuilderState

	// workflowPicker, when non-nil, draws the small centered modal
	// the issues screen pops on `f`. Owns its own cursor + the list
	// of pipelines the user can pick from. Set on the issues screen
	// model and consulted by the screen handler before normal key
	// dispatch so picker keys never bleed into the kanban behind it.
	workflowPicker *workflowPickerState
}

// workflowRunState carries per-tab workflow execution state. Owned
// by the model on a workflow tab and dropped to nil when the chain
// finalises (so the read-only chrome stays — the post-run banner
// reads from `workflowDone` / `workflowErr` instead — but no fresh
// step dispatch fires).
type workflowRunState struct {
	Workflow workflowDef
	Source   workflowSource

	// StepIdx is the top-level cursor: the index into Workflow.Steps of
	// the step (agent or loop) currently executing. While inside a loop
	// it points at the loop step and `loop` carries the inner position.
	StepIdx int

	// loop is non-nil while the runner is executing inside a loop step
	// (Workflow.Steps[StepIdx].isLoop()). It tracks the inner cursor,
	// iteration count, and the bounded per-iteration context. Nil for
	// the linear portions of the chain.
	loop *loopRunFrame

	// pendingEndTurn holds what the current step registered via the
	// end_turn MCP tool this turn: the always-required summary plus, in a
	// loop, the optional break/continue decision. Reset to nil at every
	// dispatch and consumed by advanceWorkflowStep when the turn completes
	// — the tool only records intent; the runner acts on it at turn
	// boundaries. A step that ends its turn with this still nil is
	// re-prompted (every step must call end_turn).
	pendingEndTurn *endTurnSignal

	// linearRetry / linearText are the re-prompt bookkeeping for the
	// non-loop portion of the chain — the mirror of loopRunFrame's
	// retry / retryText. linearRetry counts how many times the current
	// linear step has been re-prompted for a missing end_turn call;
	// linearText stashes its prior output so the re-prompt can feed it
	// back rather than make the step redo the work. Both reset when the
	// linear step finally registers and advances.
	linearRetry int
	linearText  string

	// remind records why the current dispatch is a re-prompt (a missing
	// end_turn call, or a loop tail that omitted its decision) so
	// buildWorkflowStepPrompt can append the matching reminder. remindNone
	// on a normal first dispatch.
	remind remindKind

	// stepLog accumulates the assistant non-tool text emitted by each
	// completed top-level step so the next step's prompt can include a
	// `Previous step output:` block. A loop contributes only its final
	// iteration's inner outputs here, on exit — intermediate iterations
	// stay scoped to the loop frame so the linear log doesn't balloon.
	stepLog []string

	// currentStep accumulates assistantTextMsg payloads for the
	// in-flight step. Rolled into the appropriate log on
	// workflowRunStepDoneMsg.
	currentStep strings.Builder

	// done flips true once the final step exits cleanly. The banner
	// shows "complete · ctrl+d to close" while the tab stays open.
	done bool

	// failed flips true if any step errors out. The banner shows the
	// error and the chain aborts at this step.
	failed       bool
	failedReason string
}

// loopRunFrame is the per-loop execution cursor, live only while the
// runner is inside a loop step. innerIdx walks the loop's inner steps;
// iteration is 1-based. The three text fields implement the bounded
// context policy: an inner step sees the linear log (frozen at loop
// entry) plus, for the head step, the previous iteration's tail output,
// or for downstream steps, the current iteration's prior outputs.
type loopRunFrame struct {
	innerIdx  int
	iteration int

	// retry counts consecutive re-prompts of the current inner step
	// within this iteration. Bumped each time the step finishes without
	// the end_turn call the runner needs (any step that skips end_turn,
	// or a tail that omits its decision) — the runner re-dispatches it,
	// "hammering" until it registers. Surfaced in the banner; reset when
	// the inner cursor advances, the iteration advances, or the loop
	// exits.
	retry int

	// iterationLog holds the inner-step outputs produced so far in the
	// current iteration, in order. Reset at each iteration boundary;
	// committed to the run's stepLog when the loop exits.
	iterationLog []string

	// prevTail is the last inner step's output from the previous
	// iteration, fed to the head step so a kick-back actually reaches
	// the next pass. Empty on iteration 1.
	prevTail string

	// retryText is the inner step's most recent output when it finished
	// without the end_turn call the runner needed, fed back into the
	// re-prompt so the agent can register without redoing the work.
	retryText string
}

// endTurnSignal is what a step registers via the end_turn MCP tool at
// the close of its turn. summary is the always-required 1-3 sentence
// account of what the step did — it becomes the step's line in the
// workflow log. decision is empty outside a loop (and ignored there);
// inside a loop it is workflowLoopContinue / workflowLoopBreak, required
// from the tail and an exceptional early break from any other inner step.
type endTurnSignal struct {
	decision string
	summary  string
}

// workflowPickerState is the small centered modal that lets the user
// pick a pipeline to run. Always shown — even with one pipeline —
// because `f` / `Ctrl+F` is a destructive action and a confirm step
// prevents mis-fires. Esc closes; Enter dispatches a
// spawnWorkflowTabMsg.
type workflowPickerState struct {
	Items  []workflowDef
	Cursor int
	Source workflowSource
}

type askMode int

const (
	askForMCP askMode = iota
	askForModel
	askForProviderSwitchModel
	askForEffort
)

type pendingAttachment struct {
	data      []byte
	mime      string
	imageID   uint32
	thumbCols int
	thumbRows int
}

const (
	pathBoxHeight   = 10
	pathBoxMinWidth = 32
	boxChromeW      = 4 // rounded border (2) + horizontal padding (2)
)
