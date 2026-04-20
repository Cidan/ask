package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

type qKind int

const (
	qPickOne qKind = iota
	qPickMany
	qPickDiagram
)

type question struct {
	kind     qKind
	prompt   string
	options  []string
	diagrams []string
}

type qAnswer struct {
	picks  map[int]bool
	custom string
	note   string
}

type askEditField int

const (
	askEditNone askEditField = iota
	askEditNote
)

var (
	askBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("13")).
			Padding(1, 2)
	askTabStyle        = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("8"))
	askTabActiveStyle  = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("5")).Bold(true)
	askPromptStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	askOptionSelected  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	askOptionCursorFG  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	askOptionRowStyle  = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	askHelpStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	askConfirmKeyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	askSummaryDimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	askNoteLabelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	askPlaceholder     = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	askCaretStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
)

const askBoxWidth = 100

func (m model) startAsk(qs []question) model {
	m.mode = modeAskQuestion
	m.askQuestions = qs
	m.askAnswers = make([]qAnswer, len(qs))
	for i := range m.askAnswers {
		m.askAnswers[i].picks = map[int]bool{}
	}
	m.askTab = 0
	m.askCursor = 0
	m.askEditing = askEditNone
	m.askNoteBackup = ""
	return m
}

func (m model) clearAsk() model {
	m.mode = modeInput
	m.askQuestions = nil
	m.askAnswers = nil
	m.askTab = 0
	m.askCursor = 0
	m.askEditing = askEditNone
	m.askNoteBackup = ""
	m.askReply = nil
	return m
}

func (m model) isCustomOption(tab int) bool {
	if tab < 0 || tab >= len(m.askQuestions) {
		return false
	}
	q := m.askQuestions[tab]
	if len(q.options) == 0 {
		return false
	}
	return strings.EqualFold(q.options[len(q.options)-1], "Enter your own")
}

func (m model) isOnConfirmTab() bool {
	return m.askTab == len(m.askQuestions)
}

func (m model) updateAsk(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, tea.Quit
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		if m.askReply != nil {
			m.askReply <- askReply{cancelled: true}
		}
		return m.clearAsk(), nil
	}

	if m.askEditing == askEditNote {
		ans := &m.askAnswers[m.askTab]
		switch {
		case msg.Code == tea.KeyEsc:
			ans.note = m.askNoteBackup
			m.askEditing = askEditNone
			m.askNoteBackup = ""
			return m, nil
		case msg.Code == tea.KeyEnter && msg.Mod&tea.ModShift != 0:
			ans.note += "\n"
			return m, nil
		case msg.Code == tea.KeyEnter:
			m.askEditing = askEditNone
			m.askNoteBackup = ""
			return m, nil
		case msg.Code == tea.KeyBackspace:
			if ans.note != "" {
				r := []rune(ans.note)
				ans.note = string(r[:len(r)-1])
			}
			return m, nil
		case msg.Text != "" && msg.Mod&^tea.ModShift == 0:
			ans.note += msg.Text
			return m, nil
		}
		return m, nil
	}

	onCustom := m.cursorOnCustom()

	if onCustom {
		if handled, mm, cmd := m.handleCustomTyping(msg); handled {
			return mm, cmd
		}
	}

	switch {
	case msg.Code == tea.KeyEsc:
		if m.askReply != nil {
			m.askReply <- askReply{cancelled: true}
		}
		return m.clearAsk(), nil

	case msg.Code == tea.KeyTab && msg.Mod&tea.ModShift != 0, msg.Code == tea.KeyLeft:
		m.askTab--
		if m.askTab < 0 {
			m.askTab = 0
		}
		m.askCursor = 0
		return m, nil

	case msg.Code == tea.KeyTab, msg.Code == tea.KeyRight:
		m.askTab++
		maxTab := len(m.askQuestions)
		if m.askTab > maxTab {
			m.askTab = maxTab
		}
		m.askCursor = 0
		return m, nil
	}

	if m.isOnConfirmTab() {
		switch msg.Code {
		case tea.KeyEnter:
			return m.submitAsk(), nil
		}
		return m, nil
	}

	q := m.askQuestions[m.askTab]
	ans := &m.askAnswers[m.askTab]

	switch {
	case msg.Code == tea.KeyUp:
		if m.askCursor > 0 {
			m.askCursor--
		}
		return m, nil
	case msg.Code == tea.KeyDown:
		if m.askCursor < len(q.options)-1 {
			m.askCursor++
		}
		return m, nil
	case msg.Code == 'n' && msg.Mod == 0 && !onCustom:
		m.askEditing = askEditNote
		m.askNoteBackup = ans.note
		return m, nil
	case msg.Code == tea.KeySpace && q.kind == qPickMany && !onCustom:
		if ans.picks[m.askCursor] {
			delete(ans.picks, m.askCursor)
		} else {
			ans.picks[m.askCursor] = true
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		if q.kind == qPickOne || q.kind == qPickDiagram {
			if onCustom && ans.custom == "" {
				return m, nil
			}
			ans.picks = map[int]bool{m.askCursor: true}
			return m.advanceAskTab(), nil
		}
		return m.advanceAskTab(), nil
	}
	return m, nil
}

func (m model) cursorOnCustom() bool {
	if m.isOnConfirmTab() {
		return false
	}
	if !m.isCustomOption(m.askTab) {
		return false
	}
	return m.askCursor == len(m.askQuestions[m.askTab].options)-1
}

func (m model) handleCustomTyping(msg tea.KeyPressMsg) (bool, tea.Model, tea.Cmd) {
	ans := &m.askAnswers[m.askTab]
	customIdx := len(m.askQuestions[m.askTab].options) - 1
	kind := m.askQuestions[m.askTab].kind

	syncSelection := func() {
		if kind == qPickMany {
			if ans.custom != "" {
				ans.picks[customIdx] = true
			} else {
				delete(ans.picks, customIdx)
			}
		}
	}

	switch {
	case msg.Code == tea.KeyEnter && msg.Mod&tea.ModShift != 0:
		ans.custom += "\n"
		syncSelection()
		return true, m, nil
	case msg.Code == tea.KeyBackspace:
		if ans.custom != "" {
			r := []rune(ans.custom)
			ans.custom = string(r[:len(r)-1])
			syncSelection()
		}
		return true, m, nil
	case msg.Text != "" && msg.Mod&^tea.ModShift == 0:
		ans.custom += msg.Text
		syncSelection()
		return true, m, nil
	}
	return false, m, nil
}

func (m model) advanceAskTab() model {
	m.askTab++
	m.askCursor = 0
	if m.askTab > len(m.askQuestions) {
		m.askTab = len(m.askQuestions)
	}
	return m
}

func (m model) submitAsk() model {
	if m.askReply != nil {
		m.askReply <- askReply{answers: m.askAnswers}
	}
	return m.clearAsk()
}

func renderAnswerSummary(q question, ans qAnswer) string {
	if len(ans.picks) == 0 {
		return askSummaryDimStyle.Render("(no answer)")
	}
	picked := make([]string, 0, len(ans.picks))
	customIdx := len(q.options) - 1
	isCustom := strings.EqualFold(q.options[customIdx], "Enter your own")
	for i, opt := range q.options {
		if !ans.picks[i] {
			continue
		}
		if isCustom && i == customIdx {
			if ans.custom != "" {
				picked = append(picked, fmt.Sprintf("%q", ans.custom))
			} else {
				picked = append(picked, askSummaryDimStyle.Render("(custom, empty)"))
			}
		} else {
			picked = append(picked, opt)
		}
	}
	return strings.Join(picked, ", ")
}

func (m model) viewAsk() string {
	if len(m.askQuestions) == 0 {
		return ""
	}
	tabsLine := m.renderAskTabs()
	var content string
	if m.isOnConfirmTab() {
		content = m.renderAskConfirm()
	} else {
		content = m.renderAskQuestion(m.askTab)
	}
	help := m.renderAskHelp()
	body := strings.Join([]string{tabsLine, "", content, "", help}, "\n")
	innerW := askBoxWidth - 6
	if innerW > m.width-6 {
		innerW = m.width - 6
	}
	return askBoxStyle.Width(innerW).Render(body)
}

func (m model) renderAskTabs() string {
	tabs := make([]string, 0, len(m.askQuestions)+1)
	for i := range m.askQuestions {
		label := fmt.Sprintf("%d", i+1)
		if m.askAnswers[i].note != "" {
			label += "•"
		}
		if len(m.askAnswers[i].picks) > 0 {
			label = "✓" + label
		}
		if i == m.askTab {
			tabs = append(tabs, askTabActiveStyle.Render(label))
		} else {
			tabs = append(tabs, askTabStyle.Render(label))
		}
	}
	label := "confirm"
	if m.isOnConfirmTab() {
		tabs = append(tabs, askTabActiveStyle.Render(label))
	} else {
		tabs = append(tabs, askTabStyle.Render(label))
	}
	return strings.Join(tabs, "")
}

func (m model) renderAskQuestion(idx int) string {
	q := m.askQuestions[idx]
	ans := m.askAnswers[idx]
	var b strings.Builder
	b.WriteString(askPromptStyle.Render(q.prompt))
	b.WriteString("\n\n")
	if q.kind == qPickDiagram {
		b.WriteString(m.renderDiagramGrid(idx))
		b.WriteString("\n")
		// note rendering at the end reuses the existing flow
		if m.askEditing == askEditNote {
			b.WriteString("\n")
			b.WriteString(renderNoteMulti(ans.note, "note › ", true))
		} else if ans.note != "" {
			b.WriteString("\n")
			b.WriteString(renderNoteMulti(ans.note, "note: ", false))
		}
		return b.String()
	}
	customIdx := len(q.options) - 1
	isCustom := m.isCustomOption(idx)
	contIndent := strings.Repeat(" ", 6)
	for i, opt := range q.options {
		isCursor := i == m.askCursor
		isPicked := ans.picks[i]
		isCustomRow := isCustom && i == customIdx
		marker := askRenderMarker(q.kind, isPicked, isCursor)
		labelLines := askOptionLabelLines(opt, isCustomRow, ans.custom, isCursor)
		for j, ln := range labelLines {
			if j == 0 {
				b.WriteString("  ")
				b.WriteString(marker)
				b.WriteString(" ")
			} else {
				b.WriteString(contIndent)
			}
			if isCursor && !isCustomRow {
				b.WriteString(askOptionRowStyle.Render(ln))
			} else {
				b.WriteString(ln)
			}
			b.WriteString("\n")
		}
	}
	if m.askEditing == askEditNote {
		b.WriteString("\n")
		b.WriteString(renderNoteMulti(ans.note, "note › ", true))
	} else if ans.note != "" {
		b.WriteString("\n")
		b.WriteString(renderNoteMulti(ans.note, "note: ", false))
	}
	return b.String()
}

func renderNoteMulti(note, label string, caret bool) string {
	lines := strings.Split(note, "\n")
	if caret {
		lines[len(lines)-1] += askCaretStyle.Render("▏")
	}
	cont := strings.Repeat(" ", len([]rune(label)))
	var b strings.Builder
	for i, ln := range lines {
		if i == 0 {
			b.WriteString(askNoteLabelStyle.Render(label))
		} else {
			b.WriteString("\n")
			b.WriteString(cont)
		}
		b.WriteString(ln)
	}
	return b.String()
}

func (m model) renderDiagramGrid(idx int) string {
	q := m.askQuestions[idx]
	ans := m.askAnswers[idx]
	diagrams := padDiagrams(normalizeDiagrams(q.diagrams))

	previewStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("14")).
		Padding(0, 1)

	var previewContent string
	if m.askCursor >= 0 && m.askCursor < len(diagrams) {
		previewContent = diagrams[m.askCursor]
	}
	preview := previewStyle.Render(previewContent)

	var listLines []string
	for i, opt := range q.options {
		isCursor := i == m.askCursor
		isPicked := ans.picks[i]
		marker := askRenderMarker(qPickOne, isPicked, isCursor)
		var label string
		if isCursor {
			label = askOptionRowStyle.Render(opt)
		} else {
			label = opt
		}
		listLines = append(listLines, "  "+marker+" "+label)
	}
	list := strings.Join(listLines, "\n")

	return lipgloss.JoinHorizontal(lipgloss.Top, list, "   ", preview)
}

func padDiagrams(diagrams []string) []string {
	w, h := diagramExtent(diagrams)
	out := make([]string, len(diagrams))
	for i, d := range diagrams {
		if d == "" {
			out[i] = strings.Repeat(strings.Repeat(" ", w)+"\n", h-1) + strings.Repeat(" ", w)
			continue
		}
		lines := strings.Split(d, "\n")
		for j, ln := range lines {
			pad := w - lipgloss.Width(ln)
			if pad > 0 {
				lines[j] = ln + strings.Repeat(" ", pad)
			}
		}
		for len(lines) < h {
			lines = append(lines, strings.Repeat(" ", w))
		}
		out[i] = strings.Join(lines, "\n")
	}
	return out
}

func normalizeDiagrams(diagrams []string) []string {
	out := make([]string, len(diagrams))
	for i, d := range diagrams {
		if d == "" {
			out[i] = d
			continue
		}
		lines := strings.Split(d, "\n")
		minIndent := -1
		for _, ln := range lines {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			indent := len(ln) - len(strings.TrimLeft(ln, " "))
			if minIndent < 0 || indent < minIndent {
				minIndent = indent
			}
		}
		if minIndent > 0 {
			for j, ln := range lines {
				if len(ln) >= minIndent {
					lines[j] = ln[minIndent:]
				}
			}
		}
		out[i] = strings.Join(lines, "\n")
	}
	return out
}

func diagramExtent(diagrams []string) (w, h int) {
	for _, d := range diagrams {
		if d == "" {
			continue
		}
		lines := strings.Split(d, "\n")
		if len(lines) > h {
			h = len(lines)
		}
		for _, ln := range lines {
			if cw := lipgloss.Width(ln); cw > w {
				w = cw
			}
		}
	}
	if h == 0 {
		h = 4
	}
	if w == 0 {
		w = 16
	}
	return
}

func askRenderMarker(k qKind, picked, cursor bool) string {
	if k == qPickMany {
		switch {
		case picked:
			return askOptionSelected.Render("[x]")
		case cursor:
			return askOptionCursorFG.Render("[·]")
		default:
			return "[ ]"
		}
	}
	switch {
	case picked:
		return askOptionSelected.Render("(•)")
	case cursor:
		return askOptionCursorFG.Render("(·)")
	default:
		return "( )"
	}
}

func askOptionLabelLines(opt string, isCustomOpt bool, custom string, cursor bool) []string {
	if !isCustomOpt {
		return []string{opt}
	}
	if custom == "" {
		if cursor {
			return []string{askPlaceholder.Render("start typing…") + askCaretStyle.Render("▏")}
		}
		return []string{askPlaceholder.Render("Enter your own")}
	}
	lines := strings.Split(custom, "\n")
	if cursor {
		lines[len(lines)-1] += askCaretStyle.Render("▏")
	}
	return lines
}

func (m model) renderAskConfirm() string {
	var b strings.Builder
	b.WriteString(askPromptStyle.Render("Confirm your answers"))
	b.WriteString("\n\n")
	for i, q := range m.askQuestions {
		b.WriteString(dimStyle.Render(fmt.Sprintf("%d. ", i+1)))
		b.WriteString(q.prompt)
		b.WriteString("\n   ")
		b.WriteString(renderAnswerSummary(q, m.askAnswers[i]))
		if note := m.askAnswers[i].note; note != "" {
			b.WriteString("\n   ")
			noteLines := strings.Split(note, "\n")
			for j, ln := range noteLines {
				if j == 0 {
					b.WriteString(askNoteLabelStyle.Render("note: "))
				} else {
					b.WriteString("\n         ")
				}
				b.WriteString(ln)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(askConfirmKeyStyle.Render("enter"))
	b.WriteString(" to submit · ")
	b.WriteString(askConfirmKeyStyle.Render("←"))
	b.WriteString(" to go back · ")
	b.WriteString(askConfirmKeyStyle.Render("esc"))
	b.WriteString(" to cancel")
	return b.String()
}

func (m model) renderAskHelp() string {
	if m.askEditing == askEditNote {
		return askHelpStyle.Render("typing note · enter save · esc cancel")
	}
	if m.cursorOnCustom() {
		return askHelpStyle.Render("type answer · shift+enter newline · enter confirm · ←→ tab · esc cancel")
	}
	if m.isOnConfirmTab() {
		return askHelpStyle.Render("←→ switch tab · enter submit · esc cancel")
	}
	q := m.askQuestions[m.askTab]
	pick := "enter select"
	if q.kind == qPickMany {
		pick = "space toggle · enter next"
	}
	return askHelpStyle.Render("↑↓ navigate · " + pick + " · ←→ tab · n note · esc cancel")
}
