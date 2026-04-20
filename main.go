package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
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

type sessionsLoadedMsg struct {
	sessions []sessionEntry
	err      error
}

var (
	selectedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	dimStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	promptStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	promptArrowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("51")).MarginLeft(3)
	promptDotStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("36")).MarginLeft(1)
	errStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	userBarStyle = lipgloss.NewStyle().
			MarginLeft(3).
			Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(lipgloss.Color("212")).
			PaddingLeft(1)
	outputStyle   = lipgloss.NewStyle().MarginLeft(5)
	thinkingStyle = lipgloss.NewStyle().MarginLeft(3)
	cdBoxStyle    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("212")).
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

	cdMatches []string
	cdIdx     int
}

const (
	cdBoxHeight   = 10
	cdBoxMinWidth = 32
)

func initialModel() model {
	ta := textarea.New()
	ta.Placeholder = "ask anything (try /resume)"
	ta.Prompt = ""
	ta.SetPromptFunc(5, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return promptArrowStyle.Render("> ")
		}
		return promptDotStyle.Render("::: ")
	})
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

	style := styles.DarkStyleConfig
	zero := uint(0)
	style.Document.Margin = &zero
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	renderer, _ := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(0),
	)

	vp := viewport.New()
	vp.Style = lipgloss.NewStyle().PaddingTop(1)
	vp.FillHeight = true

	return model{
		mode:     modeInput,
		input:    ta,
		viewport: vp,
		spinner:  sp,
		renderer: renderer,
		width:    100,
		height:   30,
	}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(msg.Width)
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

	case claudeDoneMsg:
		m.busy = false
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
			m.refreshCdMatches()
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
	below := 1 + inputH
	if items := m.filterSlashCmds(); len(items) > 0 {
		below += 1 + 1
	}
	vpH := m.height - below
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
		parts = append(parts, thinkingStyle.Render(m.spinner.View()+dimStyle.Render("thinking…")))
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
	if m.busy {
		return m, nil
	}

	items := m.filterSlashCmds()
	menuOpen := len(items) > 0
	cdOpen := m.cdActive() && len(m.cdMatches) > 0

	if msg.Mod == 0 {
		switch msg.Code {
		case tea.KeyUp:
			if cdOpen {
				if m.cdIdx > 0 {
					m.cdIdx--
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
			if cdOpen {
				if m.cdIdx < len(m.cdMatches)-1 {
					m.cdIdx++
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
			if cdOpen {
				pick := m.cdMatches[m.cdIdx]
				m.input.SetValue("cd " + pick + "/")
				m.refreshCdMatches()
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
			if m.cdActive() {
				target := strings.TrimSpace(m.cdQuery())
				if len(m.cdMatches) > 0 {
					target = m.cdMatches[m.cdIdx]
				}
				m.input.Reset()
				m.refreshCdMatches()
				m.layout()
				return m.doCd(target)
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
	m.refreshCdMatches()
	m.layout()
	return m, cmd
}

func (m model) doCd(target string) (tea.Model, tea.Cmd) {
	abs, err := resolveDir(target)
	if err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("cd: " + err.Error())))
		return m, nil
	}
	if err := os.Chdir(abs); err != nil {
		m.appendHistory(outputStyle.Render(errStyle.Render("cd: " + err.Error())))
		return m, nil
	}
	m.sessionID = ""
	m.history = nil
	cwd, _ := os.Getwd()
	m.appendHistory(outputStyle.Render(
		promptStyle.Render("✓ cd "+cwd) + "  " + dimStyle.Render("(session cleared)"),
	))
	return m, nil
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
			m.sessionID = m.sessions[m.pickerIdx].id
			m.mode = modeInput
			m.appendHistory(outputStyle.Render(promptStyle.Render(
				fmt.Sprintf("✓ resumed session %s", short(m.sessionID)))))
			return m, nil
		}
	}
	return m, nil
}

func (m model) handleCommand(line string) (tea.Model, tea.Cmd) {
	cmd, _, _ := strings.Cut(line, " ")
	switch cmd {
	case "/resume":
		return m, loadSessionsCmd()
	default:
		m.appendHistory(outputStyle.Render(errStyle.Render("unknown command: " + cmd)))
		return m, nil
	}
}

func (m model) sendToClaude(line string) (tea.Model, tea.Cmd) {
	m.busy = true
	echo := userBarStyle.Render(line)
	m.appendHistory(echo)
	return m, tea.Batch(
		m.spinner.Tick,
		runClaudeCmd(line, m.sessionID),
	)
}

func (m model) View() tea.View {
	var v tea.View
	v.AltScreen = true

	body := m.viewBody()

	if m.mode == modeInput && m.cdActive() && m.width > 0 && m.height > 0 {
		canvas := uv.NewScreenBuffer(m.width, m.height)
		uv.NewStyledString(body).Draw(canvas, image.Rectangle{
			Min: image.Pt(0, 0),
			Max: image.Pt(m.width, m.height),
		})
		box := m.renderCdBox()
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
	if items := m.filterSlashCmds(); len(items) > 0 {
		b.WriteString("\n")
		var parts []string
		for i, it := range items {
			label := it.name
			if i == m.menuIdx {
				label = selectedStyle.Render("▸ " + it.name)
			}
			parts = append(parts, label+" "+dimStyle.Render(it.desc))
		}
		b.WriteString(strings.Join(parts, "  ·  "))
	}
	return b.String()
}

func (m model) renderCdBox() string {
	matches := m.cdMatches
	contentW := cdBoxMinWidth
	for _, mt := range matches {
		if w := lipgloss.Width(mt) + 2; w > contentW {
			contentW = w
		}
	}

	rows := make([]string, cdBoxHeight)

	if len(matches) == 0 {
		searched, _ := expandTilde(m.cdQuery())
		dir, _ := filepath.Split(searched)
		if dir == "" {
			dir = "."
		}
		rows[0] = dimStyle.Render("(no matches in " + dir + ")")
	} else {
		start := 0
		if m.cdIdx >= cdBoxHeight {
			start = m.cdIdx - cdBoxHeight + 1
		}
		end := start + cdBoxHeight
		if end > len(matches) {
			end = len(matches)
		}
		for i := start; i < end; i++ {
			marker := "  "
			entry := matches[i]
			if i == m.cdIdx {
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

	return cdBoxStyle.Render(strings.Join(rows, "\n"))
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

func (m model) cdActive() bool {
	val := m.input.Value()
	return strings.HasPrefix(val, "cd ") && !strings.Contains(val, "\n")
}

func (m model) cdQuery() string {
	return strings.TrimPrefix(m.input.Value(), "cd ")
}

func (m *model) refreshCdMatches() {
	if !m.cdActive() {
		m.cdMatches = nil
		m.cdIdx = 0
		return
	}
	matches := cdComplete(m.cdQuery())
	m.cdMatches = matches
	if m.cdIdx >= len(matches) {
		m.cdIdx = 0
	}
}

func cdComplete(query string) []string {
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

func runClaudeCmd(prompt, sessionID string) tea.Cmd {
	return func() tea.Msg {
		args := []string{"-p", prompt, "--output-format", "json", "--dangerously-skip-permissions"}
		if sessionID != "" {
			args = append(args, "--resume", sessionID)
		}
		c := exec.Command("claude", args...)
		var stdout, stderr bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = &stderr
		if err := c.Run(); err != nil {
			return claudeDoneMsg{err: err, raw: stderr.String()}
		}
		var res claudeResult
		if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
			return claudeDoneMsg{err: err, raw: stdout.String()}
		}
		return claudeDoneMsg{res: res}
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
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ask:", err)
		os.Exit(1)
	}
}
