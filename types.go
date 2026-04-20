package main

import (
	"io"
	"os/exec"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	lipgloss "charm.land/lipgloss/v2"
)

type claudeResult struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

type slashCmd struct {
	name string
	desc string
}

var builtinSlashCmds = []slashCmd{
	{"/resume", "resume a previous Claude session"},
	{"/new", "start a new Claude session"},
	{"/clear", "start a new Claude session"},
	{"/model", "select the Claude model"},
}

type sessionEntry struct {
	id      string
	preview string
	modTime time.Time
}

type viewMode int

const (
	modeInput viewMode = iota
	modeSessionPicker
	modeAskQuestion
)

type claudeDoneMsg struct {
	res  claudeResult
	err  error
	raw  string
	proc *claudeProc
}

type streamStatusMsg struct {
	status string
	proc   *claudeProc
}

type claudeExitedMsg struct {
	err  error
	proc *claudeProc
}

type claudeInitLoadedMsg struct {
	slashCmds []claudeSlashEntry
	err       error
}

type todoItem struct {
	Content    string
	ActiveForm string
	Status     string
}

type todoUpdatedMsg struct {
	todos []todoItem
	proc  *claudeProc
}

type claudeSlashEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type claudeProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *stderrBuf
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
}

var (
	selectedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	dimStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	promptStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	promptArrowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	promptDotStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	cwdStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	errStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	userBarStyle     = lipgloss.NewStyle().
				MarginLeft(3).
				Border(lipgloss.NormalBorder(), false, false, false, true).
				BorderForeground(lipgloss.Color("13")).
				PaddingLeft(1)
	outputStyle   = lipgloss.NewStyle().MarginLeft(5)
	thinkingStyle = lipgloss.NewStyle().MarginLeft(3)
	chipStyle        = lipgloss.NewStyle().MarginLeft(3).Foreground(lipgloss.Color("13")).Bold(true)
	scrollThumbStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	scrollTrackStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("0"))
	thumbBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	pathBoxStyle  = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("13")).
			Padding(0, 1)
	todoBoxStyle = lipgloss.NewStyle().
			MarginLeft(3).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("6")).
			Padding(0, 1)
	todoPendingStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	todoProgressStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	todoCompletedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Strikethrough(true)
)

type model struct {
	input     textarea.Model
	viewport  viewport.Model
	spinner   spinner.Model
	renderer  *glamour.TermRenderer
	sessionID string
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
	proc     *claudeProc

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

	cancelTurnConfirming bool
	cancelTurnChoice     int

	mcpPort         int
	claudeModel     string
	claudeSlashCmds []claudeSlashEntry

	inputHistory []string
	historyIdx   int

	exitArmed bool

	todos []todoItem
}

type askMode int

const (
	askForMCP askMode = iota
	askForModel
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
