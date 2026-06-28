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

func (m model) updateFinalizedPlan(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		if m.finalizedPlanReply != nil {
			m.finalizedPlanReply <- finalizedPlanReply{cancelled: true}
		}
		m.mode = modeInput
		m.clearFinalizedPlan()
		return m, nil
	}

	if m.finalizedPlanSelectingWorkflow {
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
			cmd := func() tea.Msg {
				return spawnWorkflowTabMsg{
					OriginTabID:  m.id,
					Cwd:          m.cwd,
					WorktreeName: m.worktreeName,
					Workflow:     picked,
					Source:       src,
				}
			}
			if m.finalizedPlanReply != nil {
				m.finalizedPlanReply <- finalizedPlanReply{workflowName: picked.Name}
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
	case listNavPrev(msg):
		m.finalizedPlanCursor = listNavWrap(m.finalizedPlanCursor, -1, len(opts))
		return m, nil
	case listNavNext(msg):
		m.finalizedPlanCursor = listNavWrap(m.finalizedPlanCursor, +1, len(opts))
		return m, nil
	case msg.Code == tea.KeyEsc:
		if m.finalizedPlanReply != nil {
			m.finalizedPlanReply <- finalizedPlanReply{cancelled: true}
		}
		m.mode = modeInput
		m.clearFinalizedPlan()
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
			cmd := func() tea.Msg {
				return spawnWorkflowTabMsg{
					OriginTabID:  m.id,
					Cwd:          m.cwd,
					WorktreeName: m.worktreeName,
					Workflow:     pickedDef,
					Source:       src,
				}
			}
			if m.finalizedPlanReply != nil {
				m.finalizedPlanReply <- finalizedPlanReply{workflowName: m.finalizedPlanWorkflow}
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
			m.planningMode = false
			if m.proc != nil {
				if s, ok := m.proc.payload.(*agentSession); ok {
					s.SetPlanningMode(false)
				}
			}
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

	if scrollDelta != 0 {
		width := 84
		if width > m.width-8 {
			width = m.width - 8
		}
		contentW := width - 6
		lines := m.renderMarkdown(m.finalizedPlan, contentW)
		scrollH := (m.height - 4) - 10
		if scrollH < 3 {
			scrollH = 3
		}
		maxScrollY := len(lines) - scrollH
		if maxScrollY < 0 {
			maxScrollY = 0
		}
		m.finalizedPlanScrollY += scrollDelta
		if m.finalizedPlanScrollY < 0 {
			m.finalizedPlanScrollY = 0
		}
		if m.finalizedPlanScrollY > maxScrollY {
			m.finalizedPlanScrollY = maxScrollY
		}
	}

	return m, nil
}

func (m model) viewFinalizedPlan() string {
	width := 84
	if width > m.width-8 {
		width = m.width - 8
	}
	if width < 40 {
		width = 40
	}
	height := 24
	if height > m.height-4 {
		height = m.height - 4
	}
	if height < 12 {
		height = 12
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(activeTheme.accent).
		Padding(1, 2)

	contentW := width - 6

	title := configTitleStyle.Render("Confirm Finalized Plan")
	lines := m.renderMarkdown(m.finalizedPlan, contentW)

	scrollH := height - 10
	if scrollH < 3 {
		scrollH = 3
	}
	maxScrollY := len(lines) - scrollH
	if maxScrollY < 0 {
		maxScrollY = 0
	}
	if m.finalizedPlanScrollY > maxScrollY {
		m.finalizedPlanScrollY = maxScrollY
	}
	if m.finalizedPlanScrollY < 0 {
		m.finalizedPlanScrollY = 0
	}

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

	scrollLine := configHelpStyle.Render(fmt.Sprintf("▲ --- Line %d to %d of %d --- ▼", m.finalizedPlanScrollY+1, endY, len(lines)))
	if len(lines) <= scrollH {
		scrollLine = configHelpStyle.Render("--- Complete Plan ---")
	}

	explTitle := configHelpStyle.Bold(true).Render("Explanation:")
	wrappedExpl := wrapText(m.finalizedPlanExplanation, contentW)

	helpText := configHelpStyle.Render("PgUp/PgDn or Ctrl+U/Ctrl+D: scroll plan · Esc: cancel/exit")
	divider := configHelpStyle.Render(strings.Repeat("─", contentW))

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
				optionsArea += configSelectedRowStyle.Width(contentW).Render(row) + "\n"
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
				optionsArea += configSelectedRowStyle.Width(contentW).Render(row) + "\n"
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
