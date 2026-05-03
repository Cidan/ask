package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// openWorkflowPicker stages the small centred modal the issues screen
// pops on `f` (and the chat screen pops on Ctrl+F). Always shows the
// picker, even with one workflow — the trigger is destructive (spawns
// a new tab + agent run) and the picker doubles as a confirm step.
// Caller should also dispatch the toast and bail when the underlying
// workflow list is empty (the picker renders an "(empty)" row but
// Enter does nothing useful in that state).
func (m model) openWorkflowPicker(items []workflowDef, source workflowSource) model {
	m.workflowPicker = &workflowPickerState{
		Items:  append([]workflowDef(nil), items...),
		Cursor: 0,
		Source: source,
	}
	return m
}

func (m model) closeWorkflowPicker() model {
	m.workflowPicker = nil
	return m
}

// updateWorkflowPicker handles keypresses while the picker overlay
// owns the keyboard. Esc closes; Enter emits a spawnWorkflowTabMsg
// the app layer turns into a fresh tab. Up/Down move the cursor.
// All other keys are absorbed so a stray press can't bleed into the
// kanban behind the modal.
func (m model) updateWorkflowPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	pkr := m.workflowPicker
	if pkr == nil {
		return m, nil
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeWorkflowPicker()
		return m, nil
	case msg.Code == tea.KeyUp:
		if pkr.Cursor > 0 {
			pkr.Cursor--
		}
		return m, nil
	case msg.Code == tea.KeyDown:
		if pkr.Cursor < len(pkr.Items)-1 {
			pkr.Cursor++
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		if len(pkr.Items) == 0 {
			return m, nil
		}
		if pkr.Cursor < 0 || pkr.Cursor >= len(pkr.Items) {
			return m, nil
		}
		picked := pkr.Items[pkr.Cursor]
		source := pkr.Source
		cwd := m.cwd
		originTab := m.id
		m = m.closeWorkflowPicker()
		// Spawning a tab is an app-level concern; emit the message
		// and let app.Update create the tab and dispatch step 0.
		return m, func() tea.Msg {
			return spawnWorkflowTabMsg{
				OriginTabID: originTab,
				Cwd:         cwd,
				Workflow:    picked,
				Source:      source,
			}
		}
	}
	return m, nil
}

// renderWorkflowPicker draws the centred modal. Width is min(60,
// screen-8); height fits the rows + chrome. Empty Items renders a
// dim "no workflows" row with a hint to open the builder.
func (m model) renderWorkflowPicker() string {
	pkr := m.workflowPicker
	if pkr == nil {
		return ""
	}
	width := 60
	if width > m.width-8 {
		width = m.width - 8
	}
	if width < 30 {
		width = 30
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(activeTheme.accent).
		Padding(1, 2)

	title := configTitleStyle.Render("Run workflow on " + pkr.Source.Display())

	rows := make([]string, 0, max(1, len(pkr.Items)))
	if len(pkr.Items) == 0 {
		rows = append(rows, dimStyle.Render("(no workflows configured — ctrl+w opens the builder)"))
	} else {
		for i, w := range pkr.Items {
			label := w.Name
			meta := ""
			switch len(w.Steps) {
			case 0:
				meta = "no steps"
			case 1:
				meta = "1 step"
			default:
				meta = lipgloss.NewStyle().Render(stepsCount(len(w.Steps)))
			}
			line := label + "    " + dimStyle.Render(meta)
			if i == pkr.Cursor {
				line = "▸ " + label + "    " + dimStyle.Render(meta)
				line = themePickerRowStyle.Render(line)
			} else {
				line = "  " + line
			}
			rows = append(rows, line)
		}
	}

	help := dimStyle.Render("↑/↓ choose · enter run · esc cancel")
	body := strings.Join([]string{
		title,
		"",
		strings.Join(rows, "\n"),
		"",
		help,
	}, "\n")
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box.Width(width).Render(body))
}

// stepsCount renders the human-readable step count suffix used by the
// workflow picker. 0 is "no steps", 1 is "1 step", N>=2 is "N steps".
func stepsCount(n int) string {
	switch n {
	case 0:
		return "no steps"
	case 1:
		return "1 step"
	}
	return itoa(n) + " steps"
}
