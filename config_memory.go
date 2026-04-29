package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// memoryPickerRow describes a single row in the /config → Memory
// submenu. The submenu currently has two rows (Enabled, Gemini API
// key) but is modeled as a list so future rows (Backend, remote
// selection, DB path display) can drop in by appending here without
// restructuring the picker state machine.
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
		// contention, disk full, no Gemini key, etc.). Surface that to
		// the user so the row is honest about the live state.
		enabled = "off (open failed)"
	}
	keyState := "(not set)"
	if cfg.Memory.GeminiKey != "" {
		// Display only "configured" — never echo the key. Plain-text
		// storage at 0600 is one thing; surfacing it to a TUI someone
		// might be screen-sharing is another.
		keyState = "configured"
	}
	return []memoryPickerRow{
		{"Enabled", enabled, "enabled"},
		{"Gemini API key", keyState, "geminiKey"},
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
	m.configMemoryKeyEditing = false
	m.configMemoryKeyDraft = ""
	return m
}

// openConfigMemoryKeyEditor enters the inline text input for the
// Gemini API key. Pre-fills the draft from the on-disk key so a
// user editing an existing entry sees the current value (which they
// can clear with backspace) rather than a blank line.
func (m model) openConfigMemoryKeyEditor() model {
	cfg, _ := loadConfig()
	m.configMemoryKeyEditing = true
	m.configMemoryKeyDraft = cfg.Memory.GeminiKey
	return m
}

func (m model) closeConfigMemoryKeyEditor() model {
	m.configMemoryKeyEditing = false
	m.configMemoryKeyDraft = ""
	return m
}

// updateConfigMemoryPicker handles key presses while the Memory submenu
// is active. Routes to the key editor when active; otherwise drives
// the row cursor.
func (m model) updateConfigMemoryPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configMemoryKeyEditing {
		return m.updateConfigMemoryKeyInput(msg)
	}
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
		case "geminiKey":
			m = m.openConfigMemoryKeyEditor()
			return m, nil
		}
		return m, nil
	}
	return m, nil
}

// updateConfigMemoryKeyInput accumulates keystrokes into the draft.
// Enter saves and re-opens the service if memory is enabled (so a
// previously-failing open immediately retries). Esc discards.
func (m model) updateConfigMemoryKeyInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigMemoryKeyEditor()
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigMemoryKey()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configMemoryKeyDraft); len(r) > 0 {
			m.configMemoryKeyDraft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		m.configMemoryKeyDraft += msg.Text
		return m, nil
	}
	return m, nil
}

// applyConfigMemoryPaste appends pasted text to the key draft. Called
// from the top-level update.go PasteMsg dispatcher when the editor is
// the active focus.
func (m model) applyConfigMemoryPaste(text string) (tea.Model, tea.Cmd) {
	m.configMemoryKeyDraft += text
	return m, nil
}

// commitConfigMemoryKey persists the draft and re-opens the service
// when memory is currently flagged enabled. Re-opening here means a
// user who toggled Enabled before having a key, saw "off (open
// failed)", then pasted a key, gets the service to actually start —
// no second toggle needed.
func (m model) commitConfigMemoryKey() (tea.Model, tea.Cmd) {
	cfg, _ := loadConfig()
	cfg.Memory.GeminiKey = strings.TrimSpace(m.configMemoryKeyDraft)
	if err := saveConfig(cfg); err != nil {
		debugLog("memory key saveConfig: %v", err)
		m = m.closeConfigMemoryKeyEditor()
		return m, m.toast.show("memory: save key: " + err.Error())
	}
	m = m.closeConfigMemoryKeyEditor()

	// Toast wording mirrors what the user just did — saved a key, or
	// cleared one.
	if cfg.Memory.GeminiKey == "" {
		// If the service was open with the prior key, close it: the
		// user has just removed their credential and the live service
		// is no longer authorized to talk to Gemini.
		_ = closeMemoryService()
		return m, m.toast.show("memory: gemini key cleared")
	}

	if !memoryConfigEnabled(cfg) {
		return m, m.toast.show("memory: gemini key saved (toggle Enabled to use it)")
	}
	// Force-reopen against the new key. closeMemoryService is
	// idempotent; openMemoryService(cfg) builds a fresh embedder.
	_ = closeMemoryService()
	if err := openMemoryService(cfg); err != nil {
		debugLog("memory reopen after key save: %v", err)
		return m, m.toast.show("memory: " + err.Error())
	}
	summary := "memory on (key applied)"
	if line := memoryStatsLine(); line != "" {
		summary = "memory on — " + line
	}
	return m, m.toast.show(summary)
}

// toggleMemoryEnabled flips cfg.Memory.Enabled, persists it, and brings
// the live singleton in line. The persisted Enabled flag always
// reflects user intent — open failures (no Gemini key, lock
// contention) surface as toasts but never revert the flag, so a
// follow-up key paste can retry the open.
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
		if err := openMemoryService(cfg); err != nil {
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
// outer chrome as the theme / provider pickers. When the key editor
// is active, the body is replaced with a single text-input prompt;
// otherwise the body is the row list.
func (m model) viewConfigMemoryPicker() string {
	if m.configMemoryKeyEditing {
		return m.viewConfigMemoryKeyInput()
	}
	rows := m.memoryPickerItems()
	innerW := 0
	for _, r := range rows {
		// 4 cells of separator between name and key, plus a small margin.
		w := lipgloss.Width(r.name) + lipgloss.Width(r.key) + 4
		if w > innerW {
			innerW = w
		}
	}
	dbPath, _ := memoryDBPath(false)
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
		themePickerHelpStyle.Render("↑↓ navigate · enter open/toggle · esc close"),
	)

	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

// viewConfigMemoryKeyInput renders the inline editor used for pasting
// or typing a Gemini API key. Echoes the draft verbatim — masking is
// not the threat model here (the file is 0600), but we deliberately
// do not show the saved key on the closed picker view.
func (m model) viewConfigMemoryKeyInput() string {
	innerW := 60
	title := themePickerTitleStyle.Render("Gemini API key")
	body := []string{
		title,
		"",
		configHelpStyle.Render("paste or type the key, then enter to save"),
		"",
		configPromptStyle.Render("> ") + m.configMemoryKeyDraft + configCaretStyle.Render("▏"),
		"",
		themePickerHelpStyle.Render("enter save · esc cancel"),
	}
	for i, line := range body {
		if w := lipgloss.Width(line); w > innerW {
			innerW = w
		}
		_ = i
	}
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
