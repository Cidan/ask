package main

import (
	"os/exec"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
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
	proc   *providerProc
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
// fires. We use it as an authoritative cleanup signal: if agent_id
// matches a key in bgTasks (which is the stream-event task_id), we
// drop it, plugging the case where task_notification never arrives.
// For foreground sub-agents the key won't be present and delete is a
// no-op, which is fine.
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
}

type sessionsLoadedMsg struct {
	sessions []sessionEntry
	err      error
}

type historyLoadedMsg struct {
	sessionID string
	entries   []historyEntry
	err       error
	silent    bool
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

type model struct {
	id        int
	cwd       string
	mcpBridge *mcpBridge

	provider Provider

	input     textarea.Model
	viewport  viewport.Model
	spinner   spinner.Model
	renderer  *glamour.TermRenderer
	sessionID string
	resumeCwd string
	busy      bool
	width     int
	height    int

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
	turnBuffer         []string

	lastContentFP string

	fc *frameCache

	mcpPort           int
	providerModel     string
	providerEffort    string
	ollamaHost        string
	ollamaModel       string
	providerSlashCmds []providerSlashEntry

	inputHistory []string
	historyIdx   int
	historyDraft string

	exitArmed bool

	todos []todoItem

	bgTasks map[string]struct{}
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
