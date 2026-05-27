package main

import (
	"fmt"
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
// order in the submenu comes from projectPickerItems. The GitHub
// rows write into projectConfig.MCP.GitHub; the Linear rows write
// into projectConfig.MCP.Linear — both are project-level credential
// slots the matching issue provider piggybacks on.
var projectFieldSpecs = map[string]projectFieldSpec{
	"githubMCPEndpoint": {
		id:       "githubMCPEndpoint",
		title:    "GitHub MCP endpoint",
		helpHint: "blank uses the default (api.githubcopilot.com/mcp); enter to save",
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.GitHub.Endpoint
		},
		save: func(p *projectConfig, v string) { p.MCP.GitHub.Endpoint = v },
	},
	"githubMCPToken": {
		id:       "githubMCPToken",
		title:    "GitHub MCP PAT",
		helpHint: "personal access token with `repo` and `read:org`; enter to save",
		masked:   true,
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.GitHub.Token
		},
		save: func(p *projectConfig, v string) { p.MCP.GitHub.Token = v },
	},
	"linearGraphQLEndpoint": {
		id:       "linearGraphQLEndpoint",
		title:    "Linear GraphQL endpoint",
		helpHint: "blank uses the default (api.linear.app/graphql); enter to save",
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.Linear.Endpoint
		},
		save: func(p *projectConfig, v string) { p.MCP.Linear.Endpoint = v },
	},
	"linearAPIKey": {
		id:       "linearAPIKey",
		title:    "Linear API key",
		helpHint: "personal API key (lin_api_…); enter to save",
		masked:   true,
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.Linear.Token
		},
		save: func(p *projectConfig, v string) { p.MCP.Linear.Token = v },
	},
	"linearTeamKey": {
		id:       "linearTeamKey",
		title:    "Linear team key",
		helpHint: "team identifier (e.g. ENG); enter to save",
		load: func(c askConfig, cwd string) string {
			return loadProjectConfig(c, cwd).MCP.Linear.TeamKey
		},
		save: func(p *projectConfig, v string) { p.MCP.Linear.TeamKey = v },
	},
}

// projectPickerItems builds the row list for the Project Options
// submenu. Backend credential rows (GitHub, Linear) are always
// visible — the project-level credential slot is independent of
// whether the issue provider is configured. Returns []configItem
// so the rows feed straight into the same renderLayeredConfigBox
// helper the Global Options submenu uses (no per-picker styling
// drift).
func (m model) projectPickerItems() []configItem {
	cfg, _ := loadConfig()
	pc := loadProjectConfig(cfg, m.cwd)
	ghEndpoint := pc.MCP.GitHub.Endpoint
	if ghEndpoint == "" {
		ghEndpoint = "(default)"
	}
	lnEndpoint := pc.MCP.Linear.Endpoint
	if lnEndpoint == "" {
		lnEndpoint = "(default)"
	}
	lnTeam := pc.MCP.Linear.TeamKey
	if lnTeam == "" {
		lnTeam = "(unset)"
	}
	rows := []configItem{
		{"GitHub MCP endpoint", ghEndpoint, "githubMCPEndpoint"},
		{"GitHub MCP PAT", maskedSummary(pc.MCP.GitHub.Token), "githubMCPToken"},
		{"Linear GraphQL endpoint", lnEndpoint, "linearGraphQLEndpoint"},
		{"Linear API key", maskedSummary(pc.MCP.Linear.Token), "linearAPIKey"},
		{"Linear team key", lnTeam, "linearTeamKey"},
		{"Issue provider", issueProviderByID(pc.Issues.Provider).DisplayName(), "issueProvider"},
	}
	wfCount := len(pc.Workflows.Items)
	wfDesc := "(none)"
	if wfCount == 1 {
		wfDesc = "1 workflow"
	} else if wfCount > 1 {
		wfDesc = fmt.Sprintf("%d workflows", wfCount)
	}
	rows = append(rows, configItem{"Workflows…", wfDesc, "workflows"})
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
		case "workflows":
			// "Workflows…" jumps to the dedicated builder screen so
			// the per-cwd pipeline list / step editor / multi-line
			// prompt textarea aren't squeezed into the layered
			// /config box. The picker itself stays open behind so
			// Esc pops back to Project Options.
			m = m.closeConfigProjectPicker()
			m.mode = modeInput
			m = m.switchScreen(screenWorkflows)
			if m.workflowsBuilder == nil {
				m.workflowsBuilder = newWorkflowsBuilderState(m.cwd)
			} else {
				m.workflowsBuilder.refreshItems()
			}
			return m, nil
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
	if configTextInputKey(msg) {
		m.configFilter += msg.Text
		m.configProjectCursor = 0
		return m, nil
	}
	return m, nil
}

// cycleIssueProvider advances Issues.Provider to the next reachable
// entry in issueProviderRegistry, persists to disk, and shows a
// toast. "Reachable" means the provider's per-project credentials
// are populated — github needs MCP.GitHub.Token; linear needs both
// MCP.Linear.Token and MCP.Linear.TeamKey. Providers that fail the
// gate are skipped so the cycle always makes progress: noneIssue-
// Provider is unconditional, so the wrap is guaranteed to terminate.
//
// The skip-on-gate behaviour means a user with no credentials at all
// can still cycle without getting stuck — the cycle wraps from any
// real provider back to "none" even when nothing else is configured,
// matching the user's mental model that "cycle" should always do
// something visible. When providers were skipped, the toast lists
// them so the user understands why they didn't land on the next
// natural entry.
func (m model) cycleIssueProvider() (tea.Model, tea.Cmd) {
	var (
		next    IssueProvider
		skipped []string
		saveErr error
	)
	if err := withConfigLock(func() error {
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
		n := len(issueProviderRegistry)
		for step := 1; step <= n; step++ {
			cand := issueProviderRegistry[(curIdx+step)%n]
			if !providerActivationReady(cand, pc) {
				skipped = append(skipped, cand.DisplayName())
				continue
			}
			next = cand
			break
		}
		if next == nil {
			return nil
		}
		pc.Issues.Provider = next.ID()
		cfg = upsertProjectConfig(cfg, m.cwd, pc)
		return saveConfig(cfg)
	}); err != nil {
		saveErr = err
	}
	if saveErr != nil {
		debugLog("project provider saveConfig: %v", saveErr)
		return m, m.toast.show("config: " + saveErr.Error())
	}
	if next == nil {
		return m, nil
	}
	msg := "issues: provider → " + next.DisplayName()
	if len(skipped) > 0 {
		msg += " (skipped: " + strings.Join(skipped, ", ") + ")"
	}
	return m, m.toast.show(msg)
}

// providerActivationReady reports whether the project carries the
// credentials a given provider needs before it can dispatch a real
// network call. None ("") is always ready. New backends extend the
// switch as they land.
func providerActivationReady(p IssueProvider, pc projectConfig) bool {
	switch p.ID() {
	case "":
		return true
	case "github":
		return pc.MCP.GitHub.Token != ""
	case "linear":
		return pc.MCP.Linear.Token != "" && pc.MCP.Linear.TeamKey != ""
	}
	return true
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
	if configTextInputKey(msg) {
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
	if err := withConfigLock(func() error {
		cfg, _ := loadConfig()
		pc := loadProjectConfig(cfg, m.cwd)
		spec.save(&pc, draft)
		cfg = upsertProjectConfig(cfg, m.cwd, pc)
		return saveConfig(cfg)
	}); err != nil {
		debugLog("project %s saveConfig: %v", id, err)
		m = m.closeConfigProjectFieldEditor()
		return m, m.toast.show("config: save: " + err.Error())
	}
	m = m.closeConfigProjectFieldEditor()
	// Project MCP credentials get baked into the chat agent's
	// --mcp-config at fork time. A live proc is still holding the
	// pre-edit token/endpoint — kill it so the next user input
	// respawns with the freshly saved values. All current project
	// fields feed the chat agent's MCP roster, so unconditional is
	// correct; when a non-MCP field type lands later, gate this on
	// spec.
	m.killProc()
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
		width:      m.width,
		height:     m.height,
		title:      "Project Options",
		promptLine: filterPromptLine(m.configFilter, "Type to filter"),
		items:      m.filteredProjectPickerItems(),
		cursor:     m.configProjectCursor,
		helpText:   "↑/↓ choose · enter open/cycle · esc back",
	})
}

// viewConfigProjectFieldInput renders the inline editor for an
// editable project field (GitHub MCP PAT, GitHub MCP endpoint)
// through the same renderLayeredConfigBox as the pickers, so
// dropping into a field editor is visually a peer-level swap
// rather than a "different window" pop. The picker's "filter"
// row slot becomes the editor's input prompt; the picker's row
// list slot stays blank (no items), so the chrome is
// pixel-identical.
func (m model) viewConfigProjectFieldInput() string {
	spec, ok := projectFieldSpecs[m.configProjectFieldEditing]
	if !ok {
		return ""
	}
	display := m.configProjectFieldDraft
	if spec.masked && display != "" {
		display = strings.Repeat("•", len([]rune(display)))
	}
	return renderLayeredConfigBox(layeredConfigBoxArgs{
		width:      m.width,
		height:     m.height,
		title:      spec.title,
		promptLine: filterPromptLine(display, spec.helpHint),
		items:      nil, // editor has no list — the row area pads to blank
		cursor:     0,
		helpText:   "enter save · esc cancel",
	})
}
