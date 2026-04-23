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
// commandExecution/mcpToolCall item). The UI renders it only when
// `Render Tool Output` is on and quiet mode is off.
type toolCallMsg struct {
	name  string
	input map[string]any
	proc  *providerProc
}

// toolResultMsg carries a tool's output back to the UI. Rendered with
// the same gate as toolCallMsg.
type toolResultMsg struct {
	name    string
	output  string
	isError bool
	proc    *providerProc
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

	// Ctrl+B multi-layer switcher state. Level 0 picks provider; Level 1
	// picks a model from that provider's picker; Enter at Level 1
	// applies both to the current tab and saves cfg.Provider as the new
	// default. Esc at Level 1 pops to Level 0; Esc at Level 0 cancels.
	// When the cursor lands on the trailing "Enter your own" row and
	// the user hits Enter, providerSwitchCustomActive flips true and
	// providerSwitchCustomText starts collecting keystrokes. Enter
	// then applies the typed value; Esc pops back to the list without
	// losing what was typed so far.
	providerSwitchLevel        int
	providerSwitchProvIdx      int
	providerSwitchModelIdx     int
	providerSwitchCustomActive bool
	providerSwitchCustomText   string
	// providerSwitchModelOpts is the snapshot of the target
	// provider's model picker options captured at Level-0 → Level-1
	// descent. Caching here keeps the switcher's view renderer off
	// ModelPicker() (which for codex costs a forked app-server RPC)
	// on every keystroke.
	providerSwitchModelOpts []string

	themeName string

	quietMode          bool
	cursorBlink        bool
	renderDiffs        bool
	renderToolOutput   bool
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
