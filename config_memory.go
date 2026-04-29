package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// memoryPickerRow describes a single row in the /config → Memory
// submenu. The submenu currently has only one row (Enabled) but is
// modeled as a list so future rows (Backend, remote selection, DB
// path display) can drop in by appending here without restructuring
// the picker state machine.
type memoryPickerRow struct {
	name string
	key  string
	id   string
}

func (m model) memoryPickerItems() []memoryPickerRow {
	cfg, _ := loadConfig()
	enabled := "off"
	switch {
	case memoryServiceOpen():
		enabled = "on"
	case memoryConfigEnabled(cfg):
		// Persisted-on but not actually open: startup-open failed (lock
		// contention, disk full, etc.). Surface that to the user so the
		// Memory row is honest about the live state.
		enabled = "off (open failed)"
	}
	return []memoryPickerRow{
		{"Enabled", enabled, "enabled"},
	}
}

// openConfigMemoryPicker shows the Memory submenu. The cursor lands on
// the first row; closing via Esc just clears the active flag — there
// is nothing to "back out of" because the picker writes through to
// disk on each Enter (matching the theme/provider pickers).
func (m model) openConfigMemoryPicker() model {
	m.configMemoryPickerActive = true
	m.configMemoryCursor = 0
	return m
}

func (m model) closeConfigMemoryPicker() model {
	m.configMemoryPickerActive = false
	m.configMemoryCursor = 0
	return m
}

// updateConfigMemoryPicker handles key presses while the Memory submenu
// is active. Toggling Enabled drives both the persisted config flag and
// the live singleton: enabling opens the bbolt-backed service, disabling
// closes it, both at the moment of toggle (no restart). Open/close
// errors are reported as toasts and DO NOT revert the persisted flag —
// the user's intent wins, and a transient lock-contention failure can
// be retried by toggling off and on without losing the desired state.
func (m model) updateConfigMemoryPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	rows := m.memoryPickerItems()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigMemoryPicker()
		return m, nil
	case msg.Code == tea.KeyUp:
		if m.configMemoryCursor > 0 {
			m.configMemoryCursor--
		}
		return m, nil
	case msg.Code == tea.KeyDown:
		if m.configMemoryCursor < len(rows)-1 {
			m.configMemoryCursor++
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configMemoryCursor < 0 || m.configMemoryCursor >= len(rows) {
			return m, nil
		}
		switch rows[m.configMemoryCursor].id {
		case "enabled":
			return m.toggleMemoryEnabled()
		}
		return m, nil
	}
	return m, nil
}

// toggleMemoryEnabled flips cfg.Memory.Enabled, persists it, and brings
// the live singleton in line. Returns a toast cmd describing the result
// — success messages include the live stats line so the user sees real
// proof the bbolt file is open and reachable, not just a "✓ enabled".
func (m model) toggleMemoryEnabled() (tea.Model, tea.Cmd) {
	cfg, _ := loadConfig()
	curr := memoryConfigEnabled(cfg)
	next := !curr
	v := next
	cfg.Memory.Enabled = &v
	if err := saveConfig(cfg); err != nil {
		debugLog("memory toggle saveConfig: %v", err)
		return m, m.toast.show("memory: save config: " + err.Error())
	}
	if next {
		if err := openMemoryService(); err != nil {
			debugLog("memory open: %v", err)
			return m, m.toast.show("memory: " + err.Error())
		}
		summary := "memory on"
		if line := memoryStatsLine(); line != "" {
			summary = "memory on — " + line
		}
		return m, m.toast.show(summary)
	}
	if err := closeMemoryService(); err != nil {
		debugLog("memory close: %v", err)
		return m, m.toast.show("memory: close: " + err.Error())
	}
	return m, m.toast.show("memory off")
}

// viewConfigMemoryPicker renders the Memory submenu using the same
// outer chrome as the theme / provider pickers. The body is a small
// list with a footer line displaying the on-disk DB path (informational
// only — the path is fixed in the first slice, per the integration
// plan; power users who need to relocate can edit ask.json once the
// schema grows that field).
func (m model) viewConfigMemoryPicker() string {
	rows := m.memoryPickerItems()
	innerW := 0
	for _, r := range rows {
		// 4 cells of separator between name and key, plus a small margin.
		w := lipgloss.Width(r.name) + lipgloss.Width(r.key) + 4
		if w > innerW {
			innerW = w
		}
	}
	dbPath, _ := memoryDBPath()
	if w := lipgloss.Width("DB: " + dbPath); w > innerW {
		innerW = w
	}
	if innerW < 30 {
		innerW = 30
	}

	title := themePickerTitleStyle.Render("Memory")

	body := make([]string, 0, len(rows)+4)
	body = append(body, title, "")
	for i, r := range rows {
		body = append(body, renderMemoryPickerRow(r, innerW, i == m.configMemoryCursor))
	}
	body = append(body,
		"",
		configHelpStyle.Render("DB: "+dbPath),
		"",
		themePickerHelpStyle.Render("↑↓ navigate · enter toggle · esc close"),
	)

	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

func renderMemoryPickerRow(r memoryPickerRow, width int, selected bool) string {
	nameW := lipgloss.Width(r.name)
	keyW := lipgloss.Width(r.key)
	pad := width - nameW - keyW
	if pad < 1 {
		pad = 1
	}
	if selected {
		plain := r.name + strings.Repeat(" ", pad) + r.key
		if w := lipgloss.Width(plain); w < width {
			plain += strings.Repeat(" ", width-w)
		}
		return configSelectedRowStyle.Render(plain)
	}
	line := r.name + strings.Repeat(" ", pad)
	if r.key != "" {
		line += configKeyDimStyle.Render(r.key)
	}
	return padRight(line, width)
}
