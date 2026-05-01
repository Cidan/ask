package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// projectFieldSpec captures everything the inline editor needs to
// know to render and persist one editable text field on the
// Project Options submenu. Mirrors memoryFieldSpec (the memory
// picker is the prior art for this exact pattern).
type projectFieldSpec struct {
	id       string
	title    string
	helpHint string
	masked   bool
	// load reads the current value for the given (config, cwd)
	// pair; the editor pre-fills the draft with this so the user
	// editing an existing entry sees their previous text rather
	// than a blank line.
	load func(askConfig, string) string
	// save mutates cfg's project entry for cwd with the validated
	// draft. The picker handles loadProjectConfig / save /
	// upsertProjectConfig wrap-up; this only has to set the field.
	save func(*projectConfig, string)
}

// projectFieldSpecs is the registry. Order doesn't matter; row
// order in the submenu comes from projectPickerItems.
var projectFieldSpecs = map[string]projectFieldSpec{
	"githubEndpoint": {
		id:       "githubEndpoint",
		title:    "GitHub MCP endpoint",
		helpHint: "blank uses the default (api.githubcopilot.com/mcp); enter to save",
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).Issues.GitHub.Endpoint
		},
		save: func(p *projectConfig, v string) { p.Issues.GitHub.Endpoint = v },
	},
	"githubToken": {
		id:       "githubToken",
		title:    "GitHub PAT",
		helpHint: "personal access token with `repo` and `read:org`; enter to save",
		masked:   true,
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).Issues.GitHub.Token
		},
		save: func(p *projectConfig, v string) { p.Issues.GitHub.Token = v },
	},
}

// projectPickerItems builds the row list for the Project Options
// submenu. Rows are dynamic — when the issue provider is "none",
// the GitHub-specific rows are hidden so the user isn't asked to
// configure things that don't apply. Returns []configItem so the
// rows feed straight into the same renderLayeredConfigBox helper
// the Global Options submenu uses (no per-picker styling drift).
func (m model) projectPickerItems() []configItem {
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, m.cwd)
	rows := []configItem{
		{"Issue provider", issueProviderByID(pc.Issues.Provider).DisplayName(), "issueProvider"},
	}
	if pc.Issues.Provider == "github" {
		endpoint := pc.Issues.GitHub.Endpoint
		if endpoint == "" {
			endpoint = "(default)"
		}
		rows = append(rows,
			configItem{"GitHub endpoint", endpoint, "githubEndpoint"},
			configItem{"GitHub PAT", maskedSummary(pc.Issues.GitHub.Token), "githubToken"},
		)
	}
	return rows
}

// filteredProjectPickerItems applies the shared configFilter to the
// project rows so the filter input row in the layered box behaves
// identically to the Global submenu's. Same case-folded substring
// match against item name.
func (m model) filteredProjectPickerItems() []configItem {
	all := m.projectPickerItems()
	if m.configFilter == "" {
		return all
	}
	q := strings.ToLower(m.configFilter)
	out := make([]configItem, 0, len(all))
	for _, it := range all {
		if strings.Contains(strings.ToLower(it.name), q) {
			out = append(out, it)
		}
	}
	return out
}

func (m model) openConfigProjectPicker() model {
	m.configProjectPickerActive = true
	m.configProjectCursor = 0
	// Filter starts empty on entry — same shape as
	// openConfigGlobalPicker so the two submenus behave identically
	// from the user's keyboard.
	m.configFilter = ""
	return m
}

func (m model) closeConfigProjectPicker() model {
	m.configProjectPickerActive = false
	m.configProjectCursor = 0
	m.configProjectFieldEditing = ""
	m.configProjectFieldDraft = ""
	m.configFilter = ""
	return m
}

func (m model) openConfigProjectFieldEditor(id string) model {
	spec, ok := projectFieldSpecs[id]
	if !ok {
		return m
	}
	cfg, _ := loadConfig()
	m.configProjectFieldEditing = id
	m.configProjectFieldDraft = spec.load(cfg, m.cwd)
	return m
}

func (m model) closeConfigProjectFieldEditor() model {
	m.configProjectFieldEditing = ""
	m.configProjectFieldDraft = ""
	return m
}

// updateConfigProjectPicker handles key presses while the Project
// Options submenu is active. Routes to the inline editor when one
// is active; otherwise drives the row cursor and accepts filter
// keystrokes (same shape as updateConfigGlobalPicker so the two
// submenus feel identical to the user's keyboard).
func (m model) updateConfigProjectPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configProjectFieldEditing != "" {
		return m.updateConfigProjectFieldInput(msg)
	}
	rows := m.filteredProjectPickerItems()
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigProjectPicker()
		return m, nil
	case msg.Code == tea.KeyUp:
		if m.configProjectCursor > 0 {
			m.configProjectCursor--
		}
		return m, nil
	case msg.Code == tea.KeyDown:
		if m.configProjectCursor < len(rows)-1 {
			m.configProjectCursor++
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configProjectCursor < 0 || m.configProjectCursor >= len(rows) {
			return m, nil
		}
		id := rows[m.configProjectCursor].id
		switch id {
		case "issueProvider":
			return m.cycleIssueProvider()
		default:
			if _, ok := projectFieldSpecs[id]; ok {
				m = m.openConfigProjectFieldEditor(id)
				return m, nil
			}
		}
		return m, nil
	case msg.Code == tea.KeyBackspace:
		if m.configFilter != "" {
			r := []rune(m.configFilter)
			m.configFilter = string(r[:len(r)-1])
			m.configProjectCursor = 0
		}
		return m, nil
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		m.configFilter += msg.Text
		m.configProjectCursor = 0
		return m, nil
	}
	return m, nil
}

// cycleIssueProvider advances Issues.Provider to the next entry in
// issueProviderRegistry, persists to disk, and shows a toast. The
// cycle is tab-aware: existing tabs continue with whatever their
// state was; the change applies to all future issue-screen loads
// project-wide.
func (m model) cycleIssueProvider() (tea.Model, tea.Cmd) {
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, m.cwd)
	curIdx := -1
	for i, p := range issueProviderRegistry {
		if p.ID() == pc.Issues.Provider {
			curIdx = i
			break
		}
	}
	if curIdx == -1 {
		curIdx = 0
	}
	next := issueProviderRegistry[(curIdx+1)%len(issueProviderRegistry)]
	pc.Issues.Provider = next.ID()
	cfg = upsertProjectConfig(cfg, m.cwd, pc)
	if err := saveConfig(cfg); err != nil {
		debugLog("project provider saveConfig: %v", err)
		return m, m.toast.show("config: " + err.Error())
	}
	return m, m.toast.show("issues: provider → " + next.DisplayName())
}

// updateConfigProjectFieldInput accumulates keystrokes in the
// inline editor's draft. Enter validates+saves; Esc discards.
func (m model) updateConfigProjectFieldInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigProjectFieldEditor()
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.commitConfigProjectField()
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.configProjectFieldDraft); len(r) > 0 {
			m.configProjectFieldDraft = string(r[:len(r)-1])
		}
		return m, nil
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		m.configProjectFieldDraft += msg.Text
		return m, nil
	}
	return m, nil
}

// applyConfigProjectPaste appends pasted text to the field draft.
// Routed from update.go's PasteMsg dispatcher when an editor is
// the active focus.
func (m model) applyConfigProjectPaste(text string) (tea.Model, tea.Cmd) {
	m.configProjectFieldDraft += text
	return m, nil
}

func (m model) commitConfigProjectField() (tea.Model, tea.Cmd) {
	id := m.configProjectFieldEditing
	spec, ok := projectFieldSpecs[id]
	if !ok {
		m = m.closeConfigProjectFieldEditor()
		return m, nil
	}
	draft := strings.TrimSpace(m.configProjectFieldDraft)
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, m.cwd)
	spec.save(&pc, draft)
	cfg = upsertProjectConfig(cfg, m.cwd, pc)
	if err := saveConfig(cfg); err != nil {
		debugLog("project %s saveConfig: %v", id, err)
		m = m.closeConfigProjectFieldEditor()
		return m, m.toast.show("config: save: " + err.Error())
	}
	m = m.closeConfigProjectFieldEditor()
	return m, m.toast.show(spec.title + " saved")
}

// viewConfigProjectPicker renders Project Options through the same
// renderLayeredConfigBox helper viewConfigGlobalPicker uses — so
// the two windows are byte-identical except for the title, items,
// and help line. Field editor takes precedence when one is open.
func (m model) viewConfigProjectPicker() string {
	if m.configProjectFieldEditing != "" {
		return m.viewConfigProjectFieldInput()
	}
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:    m.width,
		height:   m.height,
		title:    "Project Options",
		filter:   m.configFilter,
		items:    m.filteredProjectPickerItems(),
		cursor:   m.configProjectCursor,
		helpText: "↑/↓ choose · enter open/cycle · esc back",
	})
}

// viewConfigProjectFieldInput renders the inline editor inside the
// same configBoxStyle frame as the picker so the user doesn't see
// the modal resize when they enter / leave the editor. The row
// list is replaced by the prompt + masked draft.
func (m model) viewConfigProjectFieldInput() string {
	spec, ok := projectFieldSpecs[m.configProjectFieldEditing]
	if !ok {
		return ""
	}
	boxW := 72
	if boxW > m.width-4 {
		boxW = m.width - 4
	}
	boxW = max(44, boxW)
	innerW := max(40, boxW-4)
	boxH := 22
	if boxH > m.height-4 {
		boxH = m.height - 4
	}
	boxH = max(14, boxH)

	display := m.configProjectFieldDraft
	if spec.masked && display != "" {
		display = strings.Repeat("•", len([]rune(display)))
	}
	title := configTitleStyle.Render(spec.title)
	hint := configHelpStyle.Render(spec.helpHint)
	prompt := configPromptStyle.Render("> ") + display + configCaretStyle.Render("▏")
	footer := configHelpStyle.Render("enter save · esc cancel")
	bodyH := max(1, boxH-8)
	pad := strings.Repeat("\n"+strings.Repeat(" ", innerW), bodyH)
	body := strings.Join([]string{
		title,
		"",
		hint,
		"",
		prompt,
		pad,
		footer,
	}, "\n")
	return configBoxStyle.Render(body)
}
