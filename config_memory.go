package main

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// memoryPickerRow describes a single row in the /config → Memory
// submenu. The submenu carries the on/off toggle, the Gemini key, and
// the Neo4j connection fields. Each editable row has an `id` that the
// inline editor uses to route the save back to the right cfg field.
type memoryPickerRow struct {
	name string
	key  string
	id   string
}

// memoryFieldSpec captures everything the generic editor needs to know
// to render and persist one field. Centralising these means the input
// handler / view / save path don't carry one switch per field.
type memoryFieldSpec struct {
	id       string
	title    string
	helpHint string
	// password rows never echo their value on the closed picker; the
	// summary shows "(set)" / "(not set)" instead. The editor itself
	// still shows the typed/pasted text so the user can see what they
	// are entering.
	masked bool
	// validate is run on the trimmed draft when the user hits Enter.
	// nil means "always accept".
	validate func(string) error
	// load reads the persisted value to pre-fill the draft when the
	// editor opens. Empty string is fine.
	load func(askConfig) string
	// save mutates cfg with the (already validated, already trimmed)
	// draft. The picker writes cfg back to disk after save returns.
	save func(*askConfig, string)
	// reopenOnSave: when true, a successful save triggers a service
	// re-open (close + open) when memory is currently enabled. Used by
	// the Neo4j connection fields and the Gemini key — anything that
	// changes how memmy dials its dependencies.
	reopenOnSave bool
}

// memoryFieldSpecs is the registry. Order doesn't matter here; the row
// order in the picker comes from memoryPickerItems.
var memoryFieldSpecs = map[string]memoryFieldSpec{
	"geminiKey": {
		id:           "geminiKey",
		title:        "Gemini API key",
		helpHint:     "paste or type the key, then enter to save",
		masked:       true,
		load:         func(c askConfig) string { return c.Memory.GeminiKey },
		save:         func(c *askConfig, v string) { c.Memory.GeminiKey = v },
		reopenOnSave: true,
	},
	"neo4jHost": {
		id:           "neo4jHost",
		title:        "Neo4j host",
		helpHint:     "hostname or IP (no scheme); enter to save",
		validate:     validateNeo4jHost,
		load:         func(c askConfig) string { return neo4jHostOrDefault(c.Memory.Neo4j) },
		save:         func(c *askConfig, v string) { c.Memory.Neo4j.Host = v },
		reopenOnSave: true,
	},
	"neo4jPort": {
		id:           "neo4jPort",
		title:        "Neo4j port",
		helpHint:     "1..65535; enter to save",
		validate:     validateNeo4jPort,
		load:         func(c askConfig) string { return strconv.Itoa(neo4jPortOrDefault(c.Memory.Neo4j)) },
		save: func(c *askConfig, v string) {
			n, _ := strconv.Atoi(v)
			c.Memory.Neo4j.Port = n
		},
		reopenOnSave: true,
	},
	"neo4jUser": {
		id:           "neo4jUser",
		title:        "Neo4j user",
		helpHint:     "blank is allowed; enter to save",
		load:         func(c askConfig) string { return c.Memory.Neo4j.User },
		save:         func(c *askConfig, v string) { c.Memory.Neo4j.User = v },
		reopenOnSave: true,
	},
	"neo4jPassword": {
		id:           "neo4jPassword",
		title:        "Neo4j password",
		helpHint:     "blank is allowed; enter to save",
		masked:       true,
		load:         func(c askConfig) string { return c.Memory.Neo4j.Password },
		save:         func(c *askConfig, v string) { c.Memory.Neo4j.Password = v },
		reopenOnSave: true,
	},
	"neo4jDatabase": {
		id:       "neo4jDatabase",
		title:    "Neo4j database",
		helpHint: "blank uses memmy's default (\"neo4j\"); enter to save",
		load: func(c askConfig) string {
			// Show the raw stored value here, not the default-filled
			// one — a blank means "use default" and the user should
			// see that as blank in the editor too.
			return c.Memory.Neo4j.Database
		},
		save:         func(c *askConfig, v string) { c.Memory.Neo4j.Database = v },
		reopenOnSave: true,
	},
}

func (m model) memoryPickerItems() []memoryPickerRow {
	cfg, _ := loadConfig()
	enabled := "off"
	switch {
	case memoryServiceOpen():
		enabled = "on"
	case memoryConfigEnabled(cfg):
		// Persisted-on but not actually open: startup-open failed (no
		// Gemini key, Neo4j unreachable, bad creds, etc.). Surface
		// that to the user so the row is honest about live state.
		enabled = "off (open failed)"
	}
	rows := []memoryPickerRow{
		{"Enabled", enabled, "enabled"},
		{"Gemini API key", maskedSummary(cfg.Memory.GeminiKey), "geminiKey"},
		{"Neo4j host", neo4jHostOrDefault(cfg.Memory.Neo4j), "neo4jHost"},
		{"Neo4j port", strconv.Itoa(neo4jPortOrDefault(cfg.Memory.Neo4j)), "neo4jPort"},
		{"Neo4j user", plainSummary(cfg.Memory.Neo4j.User), "neo4jUser"},
		{"Neo4j password", maskedSummary(cfg.Memory.Neo4j.Password), "neo4jPassword"},
		{"Neo4j database", neo4jDatabaseOrDefault(cfg.Memory.Neo4j), "neo4jDatabase"},
	}
	return rows
}

// maskedSummary describes a never-echoed field. Display only "set" /
// "not set" — never the actual value, even on a 0600 config file: the
// picker sits inside a TUI and someone might be screen-sharing.
func maskedSummary(v string) string {
	if v == "" {
		return "(not set)"
	}
	return "configured"
}

// plainSummary describes an echoed field. Empty becomes "(not set)" so
// the row visibly distinguishes "no value" from "empty string".
func plainSummary(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
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
	m.configMemoryFieldEditing = ""
	m.configMemoryFieldDraft = ""
	return m
}

// openConfigMemoryFieldEditor enters the inline text input for the
// named field. Pre-fills the draft from the on-disk value so a user
// editing an existing entry sees the current text (which they can
// clear with backspace) rather than a blank line.
func (m model) openConfigMemoryFieldEditor(id string) model {
	if _, ok := memoryFieldSpecs[id]; !ok {
		return m
	}
	cfg, _ := loadConfig()
	m.configMemoryFieldEditing = id
	m.configMemoryFieldDraft = memoryFieldSpecs[id].load(cfg)
	return m
}

func (m model) closeConfigMemoryFieldEditor() model {
	m.configMemoryFieldEditing = ""
	m.configMemoryFieldDraft = ""
	return m
}

// updateConfigMemoryPicker handles key presses while the Memory submenu
// is active. Routes to the inline editor when active; otherwise drives
// the row cursor.
func (m model) updateConfigMemoryPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configMemoryFieldEditing != "" {
		return m.updateConfigMemoryFieldInput(msg)
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
		id := rows[m.configMemoryCursor].id
		if id == "enabled" {
			return m.toggleMemoryEnabled()
		}
		if _, ok := memoryFieldSpecs[id]; ok {
			m = m.openConfigMemoryFieldEditor(id)
			return m, nil
		}
		return m, nil
	}
	return m, nil
}

// updateConfigMemoryFieldInput accumulates keystrokes into the draft.
// Enter saves and (when applicable) re-opens the service so a
// previously-failing open immediately retries against the new value.
// Esc discards.
func (m model) updateConfigMemoryFieldInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigMemoryFieldEditor()
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigMemoryField()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configMemoryFieldDraft); len(r) > 0 {
			m.configMemoryFieldDraft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		m.configMemoryFieldDraft += msg.Text
		return m, nil
	}
	return m, nil
}

// applyConfigMemoryPaste appends pasted text to the field draft.
// Called from the top-level update.go PasteMsg dispatcher when an
// editor is the active focus.
func (m model) applyConfigMemoryPaste(text string) (tea.Model, tea.Cmd) {
	m.configMemoryFieldDraft += text
	return m, nil
}

// commitConfigMemoryField validates, persists, and (when applicable)
// re-opens the service for the currently-edited field.
func (m model) commitConfigMemoryField() (tea.Model, tea.Cmd) {
	id := m.configMemoryFieldEditing
	spec, ok := memoryFieldSpecs[id]
	if !ok {
		m = m.closeConfigMemoryFieldEditor()
		return m, nil
	}
	draft := strings.TrimSpace(m.configMemoryFieldDraft)
	if spec.validate != nil {
		if err := spec.validate(draft); err != nil {
			// Keep the editor open so the user can correct without
			// retyping. Toast carries the reason.
			return m, m.toast.show("memory: " + spec.title + ": " + err.Error())
		}
	}
	cfg, _ := loadConfig()
	prevEnabled := memoryConfigEnabled(cfg)
	prevValue := spec.load(cfg)
	spec.save(&cfg, draft)
	if err := saveConfig(cfg); err != nil {
		debugLog("memory %s saveConfig: %v", id, err)
		m = m.closeConfigMemoryFieldEditor()
		return m, m.toast.show("memory: save: " + err.Error())
	}
	m = m.closeConfigMemoryFieldEditor()

	// Special case: clearing the Gemini key should also bring the
	// service down — leaving it dialing the prior key would be
	// surprising. Same for clearing all of host/port/user/password,
	// but the closer is uniform: any reopenOnSave field with a now-
	// empty Gemini key tears down.
	if cfg.Memory.GeminiKey == "" && id == "geminiKey" {
		_ = closeMemoryService()
		return m, m.toast.show("memory: gemini key cleared")
	}

	if !spec.reopenOnSave {
		return m, m.toast.show("memory: " + spec.title + " saved")
	}
	if !prevEnabled {
		return m, m.toast.show("memory: " + spec.title + " saved (toggle Enabled to use it)")
	}
	// Force-reopen against the new value. closeMemoryService is
	// idempotent; openMemoryService(cfg) builds a fresh embedder and
	// dials Neo4j at the new endpoint.
	_ = closeMemoryService()
	if err := openMemoryService(cfg); err != nil {
		debugLog("memory reopen after %s save (prev=%q): %v", id, prevValue, err)
		return m, m.toast.show("memory: " + err.Error())
	}
	summary := "memory on (" + spec.title + " applied)"
	if line := memoryStatsLine(); line != "" {
		summary = "memory on — " + line
	}
	return m, m.toast.show(summary)
}

// toggleMemoryEnabled flips cfg.Memory.Enabled, persists it, and brings
// the live singleton in line. The persisted Enabled flag always
// reflects user intent — open failures (no Gemini key, Neo4j down)
// surface as toasts but never revert the flag, so a follow-up edit
// can retry the open.
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
// outer chrome as the theme / provider pickers. When a field editor
// is active, the body is replaced with a single text-input prompt;
// otherwise the body is the row list.
func (m model) viewConfigMemoryPicker() string {
	if m.configMemoryFieldEditing != "" {
		return m.viewConfigMemoryFieldInput()
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
	cfg, _ := loadConfig()
	endpoint := fmt.Sprintf("URI: %s/%s", neo4jBoltURI(cfg.Memory.Neo4j), neo4jDatabaseOrDefault(cfg.Memory.Neo4j))
	if w := lipgloss.Width(endpoint); w > innerW {
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
		configHelpStyle.Render(endpoint),
		"",
		themePickerHelpStyle.Render("↑↓ navigate · enter open/toggle · esc close"),
	)

	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

// viewConfigMemoryFieldInput renders the inline editor used for the
// active field. Echoes the draft verbatim — masking is not the threat
// model here (the file is 0600, and the user often pastes into this),
// but we deliberately do not show stored secrets on the closed picker.
func (m model) viewConfigMemoryFieldInput() string {
	spec, ok := memoryFieldSpecs[m.configMemoryFieldEditing]
	if !ok {
		return ""
	}
	innerW := 60
	title := themePickerTitleStyle.Render(spec.title)
	body := []string{
		title,
		"",
		configHelpStyle.Render(spec.helpHint),
		"",
		configPromptStyle.Render("> ") + m.configMemoryFieldDraft + configCaretStyle.Render("▏"),
		"",
		themePickerHelpStyle.Render("enter save · esc cancel"),
	}
	for _, line := range body {
		if w := lipgloss.Width(line); w > innerW {
			innerW = w
		}
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
