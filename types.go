package main

import (
	"os/exec"
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
	id        int
	cwd       string
	mcpBridge *mcpBridge

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
	// new turn). buildCopyText/selectionContains/selectionRange consume
	// these.
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

	askOllamaActive bool
	askOllamaField  int
	askOllamaHost   string
	askOllamaModel  string
	askOllamaErr    string

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

	mcpPort       int
	providerModel string
	providerEffort    string
	ollamaHost        string
	ollamaModel       string
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

	// usageCache is the parsed ~/.claude/.usage-cache.json snapshot.
	// Refreshed at startup and after every providerDoneMsg. Nil means
	// the file is absent or unreadable — the chip omits the 5h/wk
	// segments in that case.
	usageCache *usageCache

	// lastUsageTokens is the running context size reported by the
	// most recent assistant event's message.usage block. Divided by
	// modelContextLimit(modelForContext) for the ctx chip segment.
	lastUsageTokens int

	// modelForContext is the model id from claude's system/init event,
	// preferred over providerModel for the context-limit denominator
	// because claude resolves aliases ("opus[1m]") to fully-qualified
	// ids. Falls back to providerModel before the init event lands.
	modelForContext string

	// codexUsage holds the latest rate-limit snapshot codex streamed
	// on this session (from account/rateLimits/updated) plus the
	// current thread-level token count (from thread/tokenUsage/updated).
	// hasRateLimits gates the pr/sc chip segments; context fields gate
	// the ctx segment. Cleared on every provider switch.
	codexUsage codexUsage
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
