package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	lipgloss "charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
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

var slashCmds = []slashCmd{
	{"/resume", "resume a previous Claude session"},
	{"/new", "start a new Claude session"},
	{"/clear", "start a new Claude session"},
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
)

type claudeDoneMsg struct {
	res claudeResult
	err error
	raw string
}

type streamStatusMsg struct {
	status string
}

type claudeExitedMsg struct {
	err error
}

type claudeProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

type sessionsLoadedMsg struct {
	sessions []sessionEntry
	err      error
}

type historyLoadedMsg struct {
	sessionID string
	entries   []string
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
	userBarStyle = lipgloss.NewStyle().
			MarginLeft(3).
			Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(lipgloss.Color("13")).
			PaddingLeft(1)
	outputStyle   = lipgloss.NewStyle().MarginLeft(5)
	thinkingStyle = lipgloss.NewStyle().MarginLeft(3)
	pathBoxStyle    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("13")).
			Padding(0, 1)
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

	history []string

	mode      viewMode
	menuIdx   int
	sessions  []sessionEntry
	pickerIdx int

	pathMatches []string
	pathIdx     int

	status         string
	streamCh       chan tea.Msg
	proc           *claudeProc
	queue          int
	pendingPrompts []string
}

const (
	pathBoxHeight   = 10
	pathBoxMinWidth = 32
)

func initialModel() model {
	ta := textarea.New()
	ta.Placeholder = "ask anything (try /resume)"
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.EndOfBufferCharacter = ' '
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.DynamicHeight = true
	ta.MinHeight = 3
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
	)
	ta.SetHeight(3)
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	renderer := newRenderer(100)

	vp := viewport.New()
	vp.Style = lipgloss.NewStyle().PaddingTop(1)
	vp.FillHeight = true
	vp.SoftWrap = true

	m := model{
		mode:     modeInput,
		input:    ta,
		viewport: vp,
		spinner:  sp,
		renderer: renderer,
		width:    100,
		height:   30,
	}
	m.refreshPrompt()
	return m
}

func (m model) Init() tea.Cmd {
	return nil
}

func newRenderer(width int) *glamour.TermRenderer {
	style := *styles.DefaultStyles[styles.DarkStyle]
	zero := uint(0)
	style.Document.Margin = &zero
	wrap := width - 5
	if wrap < 20 {
		wrap = 20
	}
	r, _ := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(wrap),
	)
	return r
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(msg.Width)
		m.renderer = newRenderer(msg.Width)
		m.layout()
		return m, nil

	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.layout()
		return m, cmd

	case streamStatusMsg:
		m.status = msg.status
		m.layout()
		if m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		return m, nil

	case claudeExitedMsg:
		m.busy = false
		m.status = ""
		m.queue = 0
		m.pendingPrompts = nil
		m.streamCh = nil
		m.proc = nil
		if msg.err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render("claude exited: " + msg.err.Error())))
		}
		return m, nil

	case claudeDoneMsg:
		if m.queue > 0 {
			m.queue--
		}
		stillBusy := m.queue > 0
		m.busy = stillBusy
		if stillBusy {
			m.status = "thinking…"
		} else {
			m.status = ""
		}
		var out string
		if msg.err != nil {
			out = errStyle.Render(fmt.Sprintf("error: %v", msg.err))
			if msg.raw != "" {
				out += "\n" + dimStyle.Render(msg.raw)
			}
		} else if msg.res.IsError {
			out = errStyle.Render("error: " + msg.res.Result)
		} else {
			if msg.res.SessionID != "" {
				m.sessionID = msg.res.SessionID
			}
			rendered, err := m.renderer.Render(msg.res.Result)
			if err != nil {
				out = msg.res.Result
			} else {
				out = strings.TrimRight(rendered, "\n")
			}
		}
		m.appendHistory(outputStyle.Render(out))
		if len(m.pendingPrompts) > 0 {
			next := m.pendingPrompts[0]
			m.pendingPrompts = m.pendingPrompts[1:]
			m.appendHistory(userBarStyle.Render(next))
		}
		if stillBusy && m.streamCh != nil {
			return m, nextStreamCmd(m.streamCh)
		}
		m.refreshPathMatches()
		m.layout()
		return m, nil

	case historyLoadedMsg:
		if msg.sessionID != m.sessionID {
			return m, nil
		}
		if msg.err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render(
				"could not load session history: " + msg.err.Error())))
			return m, nil
		}
		m.history = append(msg.entries,
			outputStyle.Render(promptStyle.Render(
				fmt.Sprintf("✓ resumed session %s", short(m.sessionID)))))
		m.layout()
		m.viewport.GotoBottom()
		return m, nil

	case sessionsLoadedMsg:
		if msg.err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render(fmt.Sprintf("could not load sessions: %v", msg.err))))
			return m, nil
		}
		if len(msg.sessions) == 0 {
			m.appendHistory(outputStyle.Render(dimStyle.Render("no prior sessions for this project")))
			return m, nil
		}
		m.sessions = msg.sessions
		m.pickerIdx = 0
		m.mode = modeSessionPicker
		return m, nil

	case tea.PasteMsg:
		if m.mode == modeInput && !m.busy {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			m.refreshPathMatches()
			m.layout()
			return m, cmd
		}
		return m, nil

	case tea.KeyPressMsg:
		switch m.mode {
		case modeSessionPicker:
			return m.updatePicker(msg)
		default:
			return m.updateInput(msg)
		}
	}
	return m, nil
}

func (m *model) layout() {
	inputH := m.input.Height()
	vpH := m.height - 1 - inputH
	if vpH < 1 {
		vpH = 1
	}
	m.viewport.SetWidth(m.width)
	m.viewport.SetHeight(vpH)
	m.viewport.SetContent(m.viewportContent())
	m.viewport.GotoBottom()
}

func (m model) viewportContent() string {
	parts := append([]string(nil), m.history...)
	if m.busy {
		s := m.status
		if s == "" {
			s = "thinking…"
		}
		if m.queue > 1 {
			s = fmt.Sprintf("%s  (+%d queued)", s, m.queue-1)
		}
		parts = append(parts, thinkingStyle.Render(m.spinner.View()+dimStyle.Render(s)))
	}
	return strings.Join(parts, "\n\n")
}

func (m *model) appendHistory(entry string) {
	m.history = append(m.history, entry)
	m.layout()
}

func (m model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && (msg.Code == 'c' || msg.Code == 'd') {
		return m, tea.Quit
	}

	items := m.filterSlashCmds()
	menuOpen := !m.busy && len(items) > 0
	pickOpen := !m.busy && m.pathPickerActive() && len(m.pathMatches) > 0

	if msg.Mod == 0 {
		switch msg.Code {
		case tea.KeyUp:
			if pickOpen {
				if m.pathIdx > 0 {
					m.pathIdx--
				}
				return m, nil
			}
			if menuOpen {
				if m.menuIdx > 0 {
					m.menuIdx--
				}
				return m, nil
			}
		case tea.KeyDown:
			if pickOpen {
				if m.pathIdx < len(m.pathMatches)-1 {
					m.pathIdx++
				}
				return m, nil
			}
			if menuOpen {
				if m.menuIdx < len(items)-1 {
					m.menuIdx++
				}
				return m, nil
			}
		case tea.KeyTab:
			if pickOpen {
				pick := m.pathMatches[m.pathIdx]
				m.input.SetValue(m.pathPickerCmd() + " " + pick + "/")
				m.refreshPathMatches()
				m.layout()
				return m, nil
			}
			if menuOpen {
				pick := items[m.menuIdx].name
				m.input.SetValue(pick)
				m.layout()
				return m, nil
			}
		case tea.KeyPgUp:
			m.viewport.ScrollUp(m.viewport.Height() / 2)
			return m, nil
		case tea.KeyPgDown:
			m.viewport.ScrollDown(m.viewport.Height() / 2)
			return m, nil
		case tea.KeyEnter:
			val := m.input.Value()
			line := strings.TrimSpace(val)
			if line == "" {
				return m, nil
			}
			if m.busy {
				if strings.HasPrefix(line, "/") || m.pathPickerCmd() != "" || bareCommand(line) != "" {
					return m, nil
				}
				m.input.Reset()
				m.layout()
				return m.queueToClaude(val)
			}
			if cmd := m.pathPickerCmd(); cmd != "" {
				target := strings.TrimSpace(m.pathQuery())
				if len(m.pathMatches) > 0 {
					target = m.pathMatches[m.pathIdx]
				}
				m.input.Reset()
				m.refreshPathMatches()
				m.layout()
				return m.runPathCommand(cmd, target)
			}
			if cmd := bareCommand(line); cmd != "" {
				m.input.Reset()
				m.layout()
				return m.runPathCommand(cmd, "")
			}
			m.input.Reset()
			m.menuIdx = 0
			if strings.HasPrefix(line, "/") {
				m.layout()
				return m.handleCommand(line)
			}
			return m.sendToClaude(val)
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if items := m.filterSlashCmds(); m.menuIdx >= len(items) {
		m.menuIdx = 0
	}
	if !m.busy {
		m.refreshPathMatches()
	} else {
		m.pathMatches = nil
		m.pathIdx = 0
	}
	m.layout()
	return m, cmd
}

func (m model) runPathCommand(cmd, target string) (tea.Model, tea.Cmd) {
	switch cmd {
	case "cd":
		return m.doCd(target)
	case "ls":
		return m.doLs(target)
	}
	return m, nil
}

func hasGlob(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

func resolvePath(p string) string {
	if p == "" {
		p = "."
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func (m model) doLs(target string) (tea.Model, tea.Cmd) {
	resolved := resolvePath(target)

	var paths []string
	if hasGlob(resolved) {
		matches, err := filepath.Glob(resolved)
		if err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render("ls: " + err.Error())))
			return m, nil
		}
		if len(matches) == 0 {
			m.appendHistory(outputStyle.Render(dimStyle.Render("ls: no matches for " + target)))
			return m, nil
		}
		paths = matches
	} else {
		info, err := os.Lstat(resolved)
		if err != nil {
			m.appendHistory(outputStyle.Render(errStyle.Render("ls: " + err.Error())))
			return m, nil
		}
		if info.IsDir() {
			entries, err := os.ReadDir(resolved)
			if err != nil {
				m.appendHistory(outputStyle.Render(errStyle.Render("ls: " + err.Error())))
				return m, nil
			}
			for _, e := range entries {
				paths = append(paths, filepath.Join(resolved, e.Name()))
			}
		} else {
			paths = []string{resolved}
		}
	}

	out := renderLsOutput(target, paths)
	m.appendHistory(outputStyle.Render(out))
	return m, nil
}

type lsRow struct {
	name  string
	info  os.FileInfo
	isDir bool
	isExe bool
	isLnk bool
	link  string
}

func renderLsOutput(target string, paths []string) string {
	rows := make([]lsRow, 0, len(paths))
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		mode := info.Mode()
		row := lsRow{
			name:  filepath.Base(p),
			info:  info,
			isDir: info.IsDir(),
			isExe: !info.IsDir() && mode&0o111 != 0,
			isLnk: mode&os.ModeSymlink != 0,
		}
		if row.isLnk {
			if t, err := os.Readlink(p); err == nil {
				row.link = t
			}
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].isDir != rows[j].isDir {
			return rows[i].isDir
		}
		return rows[i].name < rows[j].name
	})

	var b strings.Builder
	header := promptStyle.Render(target) + "  " + dimStyle.Render(fmt.Sprintf("(%d items)", len(rows)))
	b.WriteString(header)
	b.WriteString("\n")

	if len(rows) == 0 {
		b.WriteString(dimStyle.Render("  (empty)"))
		return b.String()
	}

	sizeW, timeW := 0, 0
	for _, r := range rows {
		if w := lipgloss.Width(formatLsSize(r)); w > sizeW {
			sizeW = w
		}
		if w := lipgloss.Width(formatLsTime(r.info.ModTime())); w > timeW {
			timeW = w
		}
	}

	for _, r := range rows {
		line := fmt.Sprintf("%s  %s  %s  %s",
			dimStyle.Render(r.info.Mode().String()),
			padRight(formatLsSize(r), sizeW),
			padRight(dimStyle.Render(formatLsTime(r.info.ModTime())), timeW),
			formatLsName(r),
		)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatLsName(r lsRow) string {
	switch {
	case r.isLnk:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Render(r.name) +
			dimStyle.Render(" → "+r.link)
	case r.isDir:
		return cwdStyle.Render(r.name + "/")
	case r.isExe:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render(r.name + "*")
	default:
		return r.name
	}
}

func formatLsSize(r lsRow) string {
	if r.isDir {
		return "-"
	}
	return humanBytes(r.info.Size())
}

func formatLsTime(t time.Time) string {
	return humanDuration(time.Since(t)) + " ago"
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(b)/float64(div), "KMGTPE"[exp])
}

func padRight(s string, w int) string {
	pad := w - lipgloss.Width(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

func (m model) doCd(target string) (tea.Model, tea.Cmd) {
	abs, err := resolveDir(target)
	if err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("cd: " + err.Error())))
		return m, nil
	}
	if cwd, err := os.Getwd(); err == nil && cwd == abs {
		return m, nil
	}
	if err := os.Chdir(abs); err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("cd: " + err.Error())))
		return m, nil
	}
	m.killProc()
	m.sessionID = ""
	m.history = nil
	cwd, _ := os.Getwd()
	m.refreshPrompt()
	m.appendHistory(outputStyle.Render(
		promptStyle.Render("✓ cd "+cwd) + "  " + dimStyle.Render("(session cleared)"),
	))
	return m, nil
}

func shortCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	home, _ := os.UserHomeDir()
	p := cwd
	if home != "" && (cwd == home || strings.HasPrefix(cwd, home+string(os.PathSeparator))) {
		p = "~" + strings.TrimPrefix(cwd, home)
	}
	if p == "~" || p == string(os.PathSeparator) {
		return p
	}
	parts := strings.Split(p, string(os.PathSeparator))
	last := len(parts) - 1
	for i, part := range parts {
		if i == last || part == "" || part == "~" {
			continue
		}
		r := []rune(part)
		parts[i] = string(r[:1])
	}
	return strings.Join(parts, string(os.PathSeparator))
}

func (m *model) refreshPrompt() {
	cwd := shortCwd()
	indent := "   "
	line0 := indent + cwd + " > "
	width := lipgloss.Width(line0)
	contRaw := indent + "::: "
	contPad := width - lipgloss.Width(contRaw)
	if contPad < 0 {
		contPad = 0
	}
	cont := contRaw + strings.Repeat(" ", contPad)
	m.input.SetPromptFunc(width, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return promptArrowStyle.Render(indent) +
				cwdStyle.Render(cwd) +
				promptArrowStyle.Render(" > ")
		}
		return promptDotStyle.Render(cont)
	})
	if m.width > 0 {
		m.input.SetWidth(m.width)
	}
}

func resolveDir(p string) (string, error) {
	if p == "" {
		return os.UserHomeDir()
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			p = home
		} else {
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.Abs(filepath.Clean(p))
}

func (m model) updatePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		return m, tea.Quit
	}
	switch msg.Code {
	case tea.KeyEsc:
		m.mode = modeInput
		return m, nil
	case tea.KeyUp:
		if m.pickerIdx > 0 {
			m.pickerIdx--
		}
	case tea.KeyDown:
		if m.pickerIdx < len(m.sessions)-1 {
			m.pickerIdx++
		}
	case tea.KeyEnter:
		if len(m.sessions) > 0 {
			m.killProc()
			sid := m.sessions[m.pickerIdx].id
			m.sessionID = sid
			m.mode = modeInput
			m.history = nil
			m.appendHistory(outputStyle.Render(dimStyle.Render(
				fmt.Sprintf("loading session %s…", short(sid)))))
			return m, loadHistoryCmd(sid, m.renderer)
		}
	}
	return m, nil
}

func (m model) handleCommand(line string) (tea.Model, tea.Cmd) {
	cmd, _, _ := strings.Cut(line, " ")
	switch cmd {
	case "/resume":
		return m, loadSessionsCmd()
	case "/new", "/clear":
		m.killProc()
		m.sessionID = ""
		m.history = nil
		m.appendHistory(outputStyle.Render(promptStyle.Render("✓ new session")))
		return m, nil
	default:
		m.appendHistory(outputStyle.Render(errStyle.Render("unknown command: " + cmd)))
		return m, nil
	}
}

func (m model) sendToClaude(line string) (tea.Model, tea.Cmd) {
	echo := userBarStyle.Render(line)
	m.appendHistory(echo)
	if err := m.ensureProc(); err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("could not start claude: " + err.Error())))
		return m, nil
	}
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": line,
		},
	}
	b, _ := json.Marshal(payload)
	if _, err := m.proc.stdin.Write(append(b, '\n')); err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("write to claude failed: " + err.Error())))
		m.killProc()
		return m, nil
	}
	m.busy = true
	m.status = "thinking…"
	m.queue = 1
	return m, tea.Batch(
		m.spinner.Tick,
		nextStreamCmd(m.streamCh),
	)
}

func (m model) queueToClaude(line string) (tea.Model, tea.Cmd) {
	if m.proc == nil {
		return m, nil
	}
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": line,
		},
	}
	b, _ := json.Marshal(payload)
	if _, err := m.proc.stdin.Write(append(b, '\n')); err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("queue write failed: " + err.Error())))
		return m, nil
	}
	m.queue++
	m.pendingPrompts = append(m.pendingPrompts, line)
	m.layout()
	return m, nil
}

func (m *model) ensureProc() error {
	if m.proc != nil {
		return nil
	}
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if m.sessionID != "" {
		args = append(args, "--resume", m.sessionID)
	}
	cmd := exec.Command("claude", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	ch := make(chan tea.Msg, 32)
	m.proc = &claudeProc{cmd: cmd, stdin: stdin}
	m.streamCh = ch
	go readClaudeStream(stdout, cmd, ch)
	return nil
}

func (m *model) killProc() {
	if m.proc == nil {
		return
	}
	_ = m.proc.stdin.Close()
	_ = m.proc.cmd.Process.Kill()
	_ = m.proc.cmd.Wait()
	m.proc = nil
	m.streamCh = nil
	m.queue = 0
	m.pendingPrompts = nil
	m.busy = false
	m.status = ""
}

func (m model) View() tea.View {
	var v tea.View
	v.AltScreen = true

	body := m.viewBody()

	var box string
	if m.mode == modeInput && !m.busy {
		switch {
		case m.pathPickerActive():
			box = m.renderPathBox()
		case len(m.filterSlashCmds()) > 0:
			box = m.renderSlashBox()
		}
	}
	if box != "" && m.width > 0 && m.height > 0 {
		canvas := uv.NewScreenBuffer(m.width, m.height)
		uv.NewStyledString(body).Draw(canvas, image.Rectangle{
			Min: image.Pt(0, 0),
			Max: image.Pt(m.width, m.height),
		})
		boxW := lipgloss.Width(box)
		boxH := lipgloss.Height(box)
		inputTopY := m.height - m.input.Height()
		boxY := inputTopY - boxH
		if boxY < 0 {
			boxY = 0
		}
		boxX := 4
		if c := m.input.Cursor(); c != nil {
			boxX = c.X
		}
		if boxX+boxW > m.width {
			boxX = m.width - boxW
		}
		if boxX < 0 {
			boxX = 0
		}
		uv.NewStyledString(box).Draw(canvas, image.Rectangle{
			Min: image.Pt(boxX, boxY),
			Max: image.Pt(boxX+boxW, boxY+boxH),
		})
		v.Content = canvas.Render()
	} else {
		v.Content = body
	}

	if m.mode == modeInput {
		if c := m.input.Cursor(); c != nil {
			v.Cursor = c
		}
	}
	return v
}

func (m model) viewBody() string {
	if m.mode == modeSessionPicker {
		return m.viewPicker()
	}
	var b strings.Builder
	b.WriteString(m.viewport.View())
	b.WriteString("\n\n")
	b.WriteString(m.input.View())
	return b.String()
}

func (m model) renderSlashBox() string {
	items := m.filterSlashCmds()
	if len(items) == 0 {
		return ""
	}
	nameW := 0
	for _, it := range items {
		if w := lipgloss.Width(it.name); w > nameW {
			nameW = w
		}
	}
	var lines []string
	for i, it := range items {
		marker := "  "
		name := it.name
		if i == m.menuIdx {
			marker = selectedStyle.Render("▸ ")
			name = selectedStyle.Render(it.name)
		}
		lines = append(lines, marker+padRight(name, nameW)+"  "+dimStyle.Render(it.desc))
	}
	return pathBoxStyle.Render(strings.Join(lines, "\n"))
}

func (m model) renderPathBox() string {
	matches := m.pathMatches
	contentW := pathBoxMinWidth
	for _, mt := range matches {
		if w := lipgloss.Width(mt) + 2; w > contentW {
			contentW = w
		}
	}

	rows := make([]string, pathBoxHeight)

	if len(matches) == 0 {
		searched, _ := expandTilde(m.pathQuery())
		dir, _ := filepath.Split(searched)
		if dir == "" {
			dir = "."
		}
		rows[0] = dimStyle.Render("(no matches in " + dir + ")")
	} else {
		start := 0
		if m.pathIdx >= pathBoxHeight {
			start = m.pathIdx - pathBoxHeight + 1
		}
		end := start + pathBoxHeight
		if end > len(matches) {
			end = len(matches)
		}
		for i := start; i < end; i++ {
			marker := "  "
			entry := matches[i]
			if i == m.pathIdx {
				marker = selectedStyle.Render("▸ ")
				entry = selectedStyle.Render(entry)
			}
			rows[i-start] = marker + entry
		}
	}

	for i, r := range rows {
		pad := contentW - lipgloss.Width(r)
		if pad > 0 {
			rows[i] = r + strings.Repeat(" ", pad)
		}
	}

	return pathBoxStyle.Render(strings.Join(rows, "\n"))
}

func (m model) viewPicker() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("select a session"))
	b.WriteString(dimStyle.Render("  (↑/↓ navigate · enter to resume · esc to cancel)"))
	b.WriteString("\n\n")
	for i, s := range m.sessions {
		preview := s.preview
		if preview == "" {
			preview = "(no preview)"
		}
		runes := []rune(preview)
		maxLen := m.width - 30
		if maxLen < 20 {
			maxLen = 20
		}
		if len(runes) > maxLen {
			preview = string(runes[:maxLen-1]) + "…"
		}
		row := fmt.Sprintf("  %s  %s  %s",
			dimStyle.Render(short(s.id)),
			dimStyle.Render(fmt.Sprintf("%6s ago", humanDuration(time.Since(s.modTime)))),
			preview,
		)
		if i == m.pickerIdx {
			row = selectedStyle.Render("▸ "+short(s.id)) +
				"  " + dimStyle.Render(fmt.Sprintf("%6s ago", humanDuration(time.Since(s.modTime)))) +
				"  " + selectedStyle.Render(preview)
		}
		b.WriteString(row)
		b.WriteString("\n")
	}
	return b.String()
}

func (m model) filterSlashCmds() []slashCmd {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") {
		return nil
	}
	var out []slashCmd
	for _, c := range slashCmds {
		if strings.HasPrefix(c.name, val) {
			out = append(out, c)
		}
	}
	return out
}

var pathPickerPrefixes = []string{"cd ", "ls "}

func bareCommand(line string) string {
	for _, p := range pathPickerPrefixes {
		cmd := strings.TrimSpace(p)
		if line == cmd {
			return cmd
		}
	}
	return ""
}

func (m model) pathPickerCmd() string {
	val := m.input.Value()
	if strings.Contains(val, "\n") {
		return ""
	}
	for _, p := range pathPickerPrefixes {
		if strings.HasPrefix(val, p) {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

func (m model) pathPickerActive() bool {
	return m.pathPickerCmd() != ""
}

func (m model) pathQuery() string {
	val := m.input.Value()
	for _, p := range pathPickerPrefixes {
		if strings.HasPrefix(val, p) {
			return strings.TrimPrefix(val, p)
		}
	}
	return ""
}

func (m *model) refreshPathMatches() {
	if !m.pathPickerActive() {
		m.pathMatches = nil
		m.pathIdx = 0
		return
	}
	matches := pathComplete(m.pathQuery())
	m.pathMatches = matches
	if m.pathIdx >= len(matches) {
		m.pathIdx = 0
	}
}

func pathComplete(query string) []string {
	expanded, tildeStripped := expandTilde(query)
	dir, prefix := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	showHidden := strings.HasPrefix(prefix, ".")
	prefixLower := strings.ToLower(prefix)
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(name), prefixLower) {
			continue
		}
		full := filepath.Join(dir, name)
		if tildeStripped {
			if home, err := os.UserHomeDir(); err == nil {
				if rel, err := filepath.Rel(home, full); err == nil && !strings.HasPrefix(rel, "..") {
					full = "~/" + rel
				}
			}
		} else if dir == "." {
			full = name
		}
		out = append(out, full)
	}
	sort.Strings(out)
	if prefix == "" {
		parent := strings.TrimRight(query, "/")
		if parent != "" {
			out = append([]string{parent}, out...)
		}
	}
	switch strings.TrimSpace(query) {
	case ".":
		out = append([]string{".", ".."}, out...)
	case "..":
		out = append([]string{".."}, out...)
	}
	return out
}

func expandTilde(p string) (string, bool) {
	if !strings.HasPrefix(p, "~") {
		return p, false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p, false
	}
	if p == "~" {
		return home, true
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), true
	}
	return p, false
}

func readClaudeStream(stdout io.Reader, cmd *exec.Cmd, ch chan tea.Msg) {
	defer close(ch)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	var pending claudeResult
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch t, _ := ev["type"].(string); t {
		case "assistant":
			if status := assistantStatus(ev); status != "" {
				ch <- streamStatusMsg{status: status}
			}
		case "result":
			pending = claudeResult{Type: "result"}
			if r, _ := ev["result"].(string); r != "" {
				pending.Result = r
			}
			if id, _ := ev["session_id"].(string); id != "" {
				pending.SessionID = id
			}
			pending.IsError, _ = ev["is_error"].(bool)
			ch <- claudeDoneMsg{res: pending}
		}
	}
	err := cmd.Wait()
	ch <- claudeExitedMsg{err: err}
}

func assistantStatus(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, item := range content {
		m, _ := item.(map[string]any)
		switch m["type"] {
		case "thinking":
			return "thinking…"
		case "tool_use":
			name, _ := m["name"].(string)
			input, _ := m["input"].(map[string]any)
			return formatToolStatus(name, input)
		}
	}
	return ""
}

func formatToolStatus(name string, input map[string]any) string {
	switch name {
	case "Bash":
		if d, _ := input["description"].(string); d != "" {
			return name + ": " + d
		}
		if c, _ := input["command"].(string); c != "" {
			return name + ": " + truncate(c, 60)
		}
	case "Read", "Edit", "Write", "NotebookEdit":
		if p, _ := input["file_path"].(string); p != "" {
			return name + ": " + filepath.Base(p)
		}
	case "Glob", "Grep":
		if p, _ := input["pattern"].(string); p != "" {
			return name + ": " + truncate(p, 60)
		}
	case "WebFetch", "WebSearch":
		if u, _ := input["url"].(string); u != "" {
			return name + ": " + truncate(u, 60)
		}
		if q, _ := input["query"].(string); q != "" {
			return name + ": " + truncate(q, 60)
		}
	case "Task":
		if p, _ := input["subagent_type"].(string); p != "" {
			return name + ": " + p
		}
	}
	return name
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func nextStreamCmd(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func sessionPath(sessionID string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"))
	return filepath.Join(dir, sessionID+".jsonl"), nil
}

func loadHistoryCmd(sessionID string, renderer *glamour.TermRenderer) tea.Cmd {
	return func() tea.Msg {
		path, err := sessionPath(sessionID)
		if err != nil {
			return historyLoadedMsg{sessionID: sessionID, err: err}
		}
		f, err := os.Open(path)
		if err != nil {
			return historyLoadedMsg{sessionID: sessionID, err: err}
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		var entries []string
		for sc.Scan() {
			var rec map[string]any
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			t, _ := rec["type"].(string)
			msg, _ := rec["message"].(map[string]any)
			switch t {
			case "user":
				if msg == nil {
					continue
				}
				if s, ok := msg["content"].(string); ok && strings.TrimSpace(s) != "" {
					entries = append(entries, userBarStyle.Render(s))
				}
			case "assistant":
				if msg == nil {
					continue
				}
				arr, ok := msg["content"].([]any)
				if !ok {
					continue
				}
				var b strings.Builder
				for _, item := range arr {
					im, _ := item.(map[string]any)
					if im["type"] != "text" {
						continue
					}
					if txt, _ := im["text"].(string); txt != "" {
						if b.Len() > 0 {
							b.WriteString("\n\n")
						}
						b.WriteString(txt)
					}
				}
				if b.Len() == 0 {
					continue
				}
				rendered, err := renderer.Render(b.String())
				if err != nil {
					rendered = b.String()
				}
				entries = append(entries, outputStyle.Render(strings.TrimRight(rendered, "\n")))
			}
		}
		return historyLoadedMsg{sessionID: sessionID, entries: entries}
	}
}

func loadSessionsCmd() tea.Cmd {
	return func() tea.Msg {
		cwd, err := os.Getwd()
		if err != nil {
			return sessionsLoadedMsg{err: err}
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return sessionsLoadedMsg{err: err}
		}
		dir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"))
		entries, err := os.ReadDir(dir)
		if err != nil {
			return sessionsLoadedMsg{err: err}
		}
		var sessions []sessionEntry
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			full := filepath.Join(dir, e.Name())
			sessions = append(sessions, sessionEntry{
				id:      strings.TrimSuffix(e.Name(), ".jsonl"),
				preview: readSessionPreview(full),
				modTime: info.ModTime(),
			})
		}
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].modTime.After(sessions[j].modTime)
		})
		return sessionsLoadedMsg{sessions: sessions}
	}
}

func readSessionPreview(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if t, _ := rec["type"].(string); t == "queue-operation" {
			if op, _ := rec["operation"].(string); op == "enqueue" {
				if c, _ := rec["content"].(string); c != "" {
					return strings.ReplaceAll(c, "\n", " ")
				}
			}
		}
		if msg, ok := rec["message"].(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "user" {
				if s, ok := msg["content"].(string); ok && s != "" {
					return strings.ReplaceAll(s, "\n", " ")
				}
				if arr, ok := msg["content"].([]any); ok {
					for _, item := range arr {
						if im, ok := item.(map[string]any); ok {
							if txt, _ := im["text"].(string); txt != "" {
								return strings.ReplaceAll(txt, "\n", " ")
							}
						}
					}
				}
			}
		}
	}
	return ""
}

func short(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func main() {
	p := tea.NewProgram(initialModel())
	final, err := p.Run()
	if m, ok := final.(model); ok {
		m.killProc()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ask:", err)
		os.Exit(1)
	}
}
