package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

func (m *model) clearFinalizedPlan() {
	m.finalizedPlan = ""
	m.finalizedPlanWorkflow = ""
	m.finalizedPlanExplanation = ""
	m.finalizedPlanReply = nil
	m.finalizedPlanCursor = 0
	m.finalizedPlanScrollY = 0
	m.finalizedPlanSelectingWorkflow = false
	m.finalizedPlanWorkflowCursor = 0
	m.finalizedPlanWorkflows = nil
	m.finalizedPlanFocusBottom = false
}

func (m model) finalizedPlanOptions() []string {
	var opts []string
	allWfs := listAllWorkflows(m.cwd)
	workflowExists := func(name string) bool {
		for _, w := range allWfs {
			if w.Name == name {
				return true
			}
		}
		return false
	}
	if m.finalizedPlanWorkflow != "" && workflowExists(m.finalizedPlanWorkflow) {
		opts = append(opts, fmt.Sprintf("Execute in workflow %s", m.finalizedPlanWorkflow))
	}
	if len(allWfs) > 0 {
		opts = append(opts, "Pick a different workflow...")
	}
	opts = append(opts, "Execute without a workflow")
	opts = append(opts, "I want to talk about this some more")
	return opts
}

func (m *model) scrollFinalizedPlan(delta int, linesCount, scrollH int) {
	maxScrollY := linesCount - scrollH
	if maxScrollY < 0 {
		maxScrollY = 0
	}
	m.finalizedPlanScrollY += delta
	if m.finalizedPlanScrollY < 0 {
		m.finalizedPlanScrollY = 0
	}
	if m.finalizedPlanScrollY > maxScrollY {
		m.finalizedPlanScrollY = maxScrollY
	}
}

func (m model) finalizedPlanBounds() (width, height, scrollH int, lines []string) {
	width = m.width - 10
	if width < 40 {
		width = 40
	}
	if width > m.width {
		width = m.width
	}

	height = m.height - 10
	if height < 12 {
		height = 12
	}
	if height > m.height {
		height = m.height
	}

	contentW := width - 6
	lines = m.renderMarkdown(m.finalizedPlan, contentW)

	// Explanation lines count
	linesExpl := len(strings.Split(wrapText(m.finalizedPlanExplanation, contentW), "\n"))

	// Options area lines count
	var linesOptions int
	if m.finalizedPlanSelectingWorkflow {
		linesOptions = 1 + len(m.finalizedPlanWorkflows)
		if linesOptions < 5 {
			linesOptions = 5 // 1 title + 4 empty/option rows
		}
	} else {
		linesOptions = len(m.finalizedPlanOptions())
		if linesOptions < 4 {
			linesOptions = 4
		}
	}

	// Dynamic height math:
	// Border & Padding consumes 4 rows
	// title + \n -> 2
	// planText -> scrollH
	// scrollLine + \n -> 2
	// divider + \n -> 2
	// expl + \n -> linesExpl + 1
	// divider + \n -> 2
	// optionsArea -> linesOptions
	// helpText -> 1
	// Total inner static height = 2 + 2 + 2 + linesExpl + 1 + 2 + linesOptions + 1 = linesExpl + linesOptions + 10
	// Plus 4 rows for border/padding -> linesExpl + linesOptions + 14.
	// Adding 1 for safety to avoid overdraw.
	totalStaticHeight := linesExpl + linesOptions + 15

	scrollH = height - totalStaticHeight
	if scrollH < 3 {
		scrollH = 3
	}

	return width, height, scrollH, lines
}

func (m model) updateFinalizedPlan(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		if m.finalizedPlanReply != nil {
			m.finalizedPlanReply <- finalizedPlanReply{cancelled: true}
		}
		m.mode = modeInput
		m.clearFinalizedPlan()
		return m, nil
	}

	// Tab / Shift+Tab swaps focus between top and bottom pane
	if msg.Code == tea.KeyTab {
		m.finalizedPlanFocusBottom = !m.finalizedPlanFocusBottom
		return m, nil
	}

	if m.finalizedPlanSelectingWorkflow {
		if !m.finalizedPlanFocusBottom {
			// Top focused: arrow keys scroll plan text, Enter is ignored
			scrollDelta := 0
			switch {
			case msg.Code == tea.KeyPgUp || (msg.Mod == tea.ModCtrl && msg.Code == 'u'):
				scrollDelta = -10
			case msg.Code == tea.KeyPgDown || (msg.Mod == tea.ModCtrl && msg.Code == 'd'):
				scrollDelta = 10
			case listNavPrev(msg):
				scrollDelta = -1
			case listNavNext(msg):
				scrollDelta = 1
			case msg.Code == tea.KeyEsc:
				m.finalizedPlanSelectingWorkflow = false
				return m, nil
			}
			if scrollDelta != 0 {
				_, _, scrollH, lines := m.finalizedPlanBounds()
				(&m).scrollFinalizedPlan(scrollDelta, len(lines), scrollH)
			}
			return m, nil
		}

		// Bottom focused: navigate workflow picker
		switch {
		case msg.Code == tea.KeyEsc:
			m.finalizedPlanSelectingWorkflow = false
			return m, nil
		case listNavPrev(msg):
			m.finalizedPlanWorkflowCursor = listNavWrap(m.finalizedPlanWorkflowCursor, -1, len(m.finalizedPlanWorkflows))
			return m, nil
		case listNavNext(msg):
			m.finalizedPlanWorkflowCursor = listNavWrap(m.finalizedPlanWorkflowCursor, +1, len(m.finalizedPlanWorkflows))
			return m, nil
		case msg.Code == tea.KeyEnter:
			if len(m.finalizedPlanWorkflows) == 0 {
				return m, nil
			}
			picked := m.finalizedPlanWorkflows[m.finalizedPlanWorkflowCursor]
			src := chatWorkflowSource(m.id, m.history)
			if m.finalizedPlanReply != nil {
				m.finalizedPlanReply <- finalizedPlanReply{workflowName: picked.Name, source: src}
				m.mode = modeInput
				m.clearFinalizedPlan()
				return m, nil
			}
			cmd := func() tea.Msg {
				return spawnWorkflowTabMsg{
					OriginTabID:  m.id,
					Cwd:          m.cwd,
					WorktreeName: m.worktreeName,
					Workflow:     picked,
					Source:       src,
				}
			}
			m.mode = modeInput
			m.clearFinalizedPlan()
			return m, cmd
		}
		return m, nil
	}

	opts := m.finalizedPlanOptions()
	scrollDelta := 0
	switch {
	case msg.Code == tea.KeyPgUp || (msg.Mod == tea.ModCtrl && msg.Code == 'u'):
		scrollDelta = -10
	case msg.Code == tea.KeyPgDown || (msg.Mod == tea.ModCtrl && msg.Code == 'd'):
		scrollDelta = 10
	case msg.Code == tea.KeyEsc:
		if m.finalizedPlanReply != nil {
			m.finalizedPlanReply <- finalizedPlanReply{cancelled: true}
		}
		m.mode = modeInput
		m.clearFinalizedPlan()
		return m, nil
	}

	if !m.finalizedPlanFocusBottom {
		// Top focused: arrow keys scroll plan text, Enter is ignored
		switch {
		case listNavPrev(msg):
			scrollDelta = -1
		case listNavNext(msg):
			scrollDelta = 1
		}
	} else {
		// Bottom focused: arrow keys navigate option picker, Enter executes
		switch {
		case listNavPrev(msg):
			m.finalizedPlanCursor = listNavWrap(m.finalizedPlanCursor, -1, len(opts))
			return m, nil
		case listNavNext(msg):
			m.finalizedPlanCursor = listNavWrap(m.finalizedPlanCursor, +1, len(opts))
			return m, nil
		case msg.Code == tea.KeyEnter:
			if len(opts) == 0 {
				return m, nil
			}
			pickedOpt := opts[m.finalizedPlanCursor]
			switch {
			case strings.HasPrefix(pickedOpt, "Execute in workflow"):
				var pickedDef workflowDef
				for _, w := range listAllWorkflows(m.cwd) {
					if w.Name == m.finalizedPlanWorkflow {
						pickedDef = w
						break
					}
				}
				src := chatWorkflowSource(m.id, m.history)
				if m.finalizedPlanReply != nil {
					m.finalizedPlanReply <- finalizedPlanReply{workflowName: m.finalizedPlanWorkflow, source: src}
					m.mode = modeInput
					m.clearFinalizedPlan()
					return m, nil
				}
				cmd := func() tea.Msg {
					return spawnWorkflowTabMsg{
						OriginTabID:  m.id,
						Cwd:          m.cwd,
						WorktreeName: m.worktreeName,
						Workflow:     pickedDef,
						Source:       src,
					}
				}
				m.mode = modeInput
				m.clearFinalizedPlan()
				return m, cmd

			case pickedOpt == "Pick a different workflow...":
				m.finalizedPlanWorkflows = listAllWorkflows(m.cwd)
				m.finalizedPlanWorkflowCursor = 0
				m.finalizedPlanSelectingWorkflow = true
				return m, nil

			case pickedOpt == "Execute without a workflow":
				if m.finalizedPlanReply != nil {
					m.finalizedPlanReply <- finalizedPlanReply{executeInline: true}
				}
				m.mode = modeInput
				m.clearFinalizedPlan()
				return m, nil

			case pickedOpt == "I want to talk about this some more":
				if m.finalizedPlanReply != nil {
					m.finalizedPlanReply <- finalizedPlanReply{talkMore: true}
				}
				m.mode = modeInput
				m.clearFinalizedPlan()
				return m, nil
			}
		}
	}

	if scrollDelta != 0 {
		_, _, scrollH, lines := m.finalizedPlanBounds()
		(&m).scrollFinalizedPlan(scrollDelta, len(lines), scrollH)
	}

	return m, nil
}

func (m model) updateFinalizedPlanMouseWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	if !m.finalizedPlanFocusBottom {
		_, _, scrollH, lines := m.finalizedPlanBounds()
		delta := 0
		if msg.Button == tea.MouseWheelUp {
			delta = -3
		} else if msg.Button == tea.MouseWheelDown {
			delta = 3
		}
		if delta != 0 {
			(&m).scrollFinalizedPlan(delta, len(lines), scrollH)
		}
	}
	return m, nil
}

func (m model) viewFinalizedPlan() string {
	width, height, scrollH, lines := m.finalizedPlanBounds()

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(activeTheme.accent).
		Padding(1, 2)

	contentW := width - 6

	title := configTitleStyle.Render("Confirm Finalized Plan")

	endY := m.finalizedPlanScrollY + scrollH
	if endY > len(lines) {
		endY = len(lines)
	}

	var planText string
	if len(lines) > 0 {
		planText = strings.Join(lines[m.finalizedPlanScrollY:endY], "\n")
	} else {
		planText = m.finalizedPlan
	}

	linesRenderedCount := endY - m.finalizedPlanScrollY
	if linesRenderedCount < scrollH {
		planText += strings.Repeat("\n", scrollH-linesRenderedCount)
	}

	var scrollLine string
	scrollProgressStr := fmt.Sprintf("▲ --- Line %d to %d of %d --- ▼", m.finalizedPlanScrollY+1, endY, len(lines))
	if len(lines) <= scrollH {
		scrollProgressStr = "--- Complete Plan ---"
	}
	if !m.finalizedPlanFocusBottom {
		scrollLine = lipgloss.NewStyle().Foreground(activeTheme.accent).Bold(true).Render(scrollProgressStr)
	} else {
		scrollLine = configHelpStyle.Render(scrollProgressStr)
	}

	explTitle := configHelpStyle.Bold(true).Render("Explanation:")
	wrappedExpl := wrapText(m.finalizedPlanExplanation, contentW)

	helpText := configHelpStyle.Render("Tab/Shift+Tab: swap focus · PgUp/PgDn: scroll · Esc: cancel/exit")
	divider := configHelpStyle.Render(strings.Repeat("─", contentW))

	unfocusedSelectedStyle := lipgloss.NewStyle().
		Foreground(activeTheme.darkFG).
		Background(activeTheme.dim).
		Bold(true)

	var optionsArea string
	if m.finalizedPlanSelectingWorkflow {
		optionsArea = configTitleStyle.Render("Select a workflow to run:") + "\n"
		for i, wf := range m.finalizedPlanWorkflows {
			row := "  " + wf.Name
			if wf.Description != "" {
				row += " - " + wf.Description
			}
			row = truncateForRow(row, contentW)
			if i == m.finalizedPlanWorkflowCursor {
				if m.finalizedPlanFocusBottom {
					optionsArea += configSelectedRowStyle.Width(contentW).Render(row) + "\n"
				} else {
					optionsArea += unfocusedSelectedStyle.Width(contentW).Render(row) + "\n"
				}
			} else {
				optionsArea += row + "\n"
			}
		}
		for i := len(m.finalizedPlanWorkflows); i < 4; i++ {
			optionsArea += "\n"
		}
	} else {
		opts := m.finalizedPlanOptions()
		for i, opt := range opts {
			row := "  " + opt
			if i == m.finalizedPlanCursor {
				if m.finalizedPlanFocusBottom {
					optionsArea += configSelectedRowStyle.Width(contentW).Render(row) + "\n"
				} else {
					optionsArea += unfocusedSelectedStyle.Width(contentW).Render(row) + "\n"
				}
			} else {
				optionsArea += row + "\n"
			}
		}
		for i := len(opts); i < 4; i++ {
			optionsArea += "\n"
		}
	}

	boxContent := title + "\n" +
		planText + "\n" +
		scrollLine + "\n" +
		divider + "\n" +
		explTitle + " " + wrappedExpl + "\n" +
		divider + "\n" +
		optionsArea +
		helpText

	return box.Width(width).Height(height).Render(boxContent)
}

func (m model) renderMarkdown(raw string, width int) []string {
	r := newRenderer(width)
	rendered, err := r.Render(raw)
	if err != nil {
		return strings.Split(raw, "\n")
	}
	return strings.Split(strings.TrimRight(rendered, "\n"), "\n")
}

func wrapText(text string, width int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	currentLine := words[0]
	for _, word := range words[1:] {
		if len(currentLine)+1+len(word) > width {
			lines = append(lines, currentLine)
			currentLine = word
		} else {
			currentLine += " " + word
		}
	}
	lines = append(lines, currentLine)
	return strings.Join(lines, "\n")
}
