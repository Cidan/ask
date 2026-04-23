package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// Ctrl+B opens the quick provider/model switcher. It's a two-layer
// picker: Level 0 lists every registered provider, Level 1 lists the
// selected provider's /model options. Applying both switches the
// current tab's provider (killing the active proc), persists the model
// under that provider's settings, and updates cfg.Provider so new tabs
// also default to the same backend.

// switcherProviderOptions returns provider labels in registry order —
// label is DisplayName so the picker reads nicely, but we key back to
// ID when applying.
func switcherProviderOptions() []string {
	out := make([]string, 0, len(providerRegistry))
	for _, p := range providerRegistry {
		out = append(out, p.DisplayName())
	}
	return out
}

// switcherModelOptions returns the model picker options for the
// provider at provIdx. "Enter your own" is deliberately omitted — the
// quick switcher has no text-input mode yet, so exposing that row
// would silently no-op on Enter. /model still handles custom text
// through the ask-question flow; once we add a text input here, this
// function can append the row and applyProviderSwitch can prompt.
func switcherModelOptions(provIdx int) []string {
	if provIdx < 0 || provIdx >= len(providerRegistry) {
		return nil
	}
	picker := providerRegistry[provIdx].ModelPicker()
	return append([]string{}, picker.Options...)
}

// openProviderSwitch enters the quick switcher. Cursor starts on the
// current provider; Level 0 is always where we enter.
func (m model) openProviderSwitch() model {
	m.mode = modeProviderSwitch
	m.providerSwitchLevel = 0
	m.providerSwitchProvIdx = indexOfProvider(m.provider)
	m.providerSwitchModelIdx = 0
	return m
}

// closeProviderSwitch resets state and pops back to input mode.
func (m model) closeProviderSwitch() model {
	m.mode = modeInput
	m.providerSwitchLevel = 0
	m.providerSwitchProvIdx = 0
	m.providerSwitchModelIdx = 0
	return m
}

func indexOfProvider(p Provider) int {
	if p == nil {
		return 0
	}
	for i, reg := range providerRegistry {
		if reg.ID() == p.ID() {
			return i
		}
	}
	return 0
}

func (m model) updateProviderSwitch(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	switch m.providerSwitchLevel {
	case 0:
		return m.updateProviderSwitchLevel0(msg)
	case 1:
		return m.updateProviderSwitchLevel1(msg)
	}
	return m.closeProviderSwitch(), nil
}

func (m model) updateProviderSwitchLevel0(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	provs := providerRegistry
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		return m.closeProviderSwitch(), nil
	case msg.Code == tea.KeyUp:
		if m.providerSwitchProvIdx > 0 {
			m.providerSwitchProvIdx--
		}
		return m, nil
	case msg.Code == tea.KeyDown:
		if m.providerSwitchProvIdx < len(provs)-1 {
			m.providerSwitchProvIdx++
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		opts := switcherModelOptions(m.providerSwitchProvIdx)
		if len(opts) == 0 {
			// No model picker for this provider; apply the provider
			// switch immediately with whatever model the provider has
			// saved (may be empty → provider default).
			return m.applyProviderSwitch("")
		}
		m.providerSwitchLevel = 1
		m.providerSwitchModelIdx = seedModelCursor(m.providerSwitchProvIdx, opts)
		return m, nil
	}
	return m, nil
}

// seedModelCursor positions the model-list cursor on the saved model
// for the chosen provider so the picker opens on "what you last
// picked" rather than the top of the list.
func seedModelCursor(provIdx int, opts []string) int {
	if provIdx < 0 || provIdx >= len(providerRegistry) {
		return 0
	}
	saved := providerRegistry[provIdx].LoadSettings().Model
	if saved == "" {
		return 0
	}
	if strings.EqualFold(saved, "ollama") {
		for i, opt := range opts {
			if opt == ollamaModelOption {
				return i
			}
		}
	}
	for i, opt := range opts {
		if strings.EqualFold(opt, saved) {
			return i
		}
	}
	// Saved model didn't match any labelled option — cursor on the
	// trailing "Enter your own" row when present.
	if n := len(opts); n > 0 && opts[n-1] == "Enter your own" {
		return n - 1
	}
	return 0
}

func (m model) updateProviderSwitchLevel1(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	opts := switcherModelOptions(m.providerSwitchProvIdx)
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c':
		return m.closeProviderSwitch(), nil
	case msg.Code == tea.KeyEsc:
		m.providerSwitchLevel = 0
		m.providerSwitchModelIdx = 0
		return m, nil
	case msg.Code == tea.KeyUp:
		if m.providerSwitchModelIdx > 0 {
			m.providerSwitchModelIdx--
		}
		return m, nil
	case msg.Code == tea.KeyDown:
		if m.providerSwitchModelIdx < len(opts)-1 {
			m.providerSwitchModelIdx++
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.providerSwitchModelIdx < 0 || m.providerSwitchModelIdx >= len(opts) {
			return m, nil
		}
		picked := switcherModelFromLabel(opts[m.providerSwitchModelIdx])
		return m.applyProviderSwitch(picked)
	}
	return m, nil
}

// switcherModelFromLabel maps the label shown in the model list back to
// the model string we store. Mirrors applyModelPick's decoding.
func switcherModelFromLabel(label string) string {
	switch {
	case label == ollamaModelOption:
		return "ollama"
	case strings.EqualFold(label, "default"):
		return ""
	default:
		return label
	}
}

// applyProviderSwitch swaps the current tab to providerRegistry[provIdx]
// with the given model, kills the active proc, reloads per-provider
// settings, and saves cfg.Provider as the new default. Same-provider
// swaps (model-only changes) preserve sessionID/resumeCwd so the next
// ensureProc picks up where the conversation left off.
func (m model) applyProviderSwitch(model string) (tea.Model, tea.Cmd) {
	if m.providerSwitchProvIdx < 0 || m.providerSwitchProvIdx >= len(providerRegistry) {
		return m.closeProviderSwitch(), nil
	}
	newProv := providerRegistry[m.providerSwitchProvIdx]
	newSettings := newProv.LoadSettings()
	sameProvider := m.provider != nil && m.provider.ID() == newProv.ID()

	// Kill the active proc — even on a same-provider swap, the new
	// model flag only takes effect after a fresh fork.
	m.killProc()

	m.provider = newProv
	m.providerModel = model
	m.providerEffort = newSettings.Effort
	m.providerSlashCmds = newSettings.SlashCommands
	if !sameProvider {
		// Cross-provider swaps drop the old session id (session ids
		// are provider-specific), but the worktree is shared: every
		// backend in this tab runs inside the same
		// .claude/worktrees/ask-… directory so they collaborate on
		// the same branch.
		m.sessionID = ""
		m.resumeCwd = ""
	}

	// Persist the model selection under the new provider, and pin the
	// default provider for new tabs.
	newSettings.Model = model
	if err := newProv.SaveSettings(newSettings); err != nil {
		debugLog("SaveSettings err: %v", err)
	}
	cfg, _ := loadConfig()
	cfg.Provider = newProv.ID()
	if err := saveConfig(cfg); err != nil {
		debugLog("saveConfig err: %v", err)
	}

	var msg string
	switch {
	case sameProvider && model != "":
		msg = "✓ " + newProv.DisplayName() + " model → " + model
	case sameProvider:
		msg = "✓ " + newProv.DisplayName() + " model cleared (provider default)"
	case model != "":
		msg = "✓ switched to " + newProv.DisplayName() + " (" + model + ")"
	default:
		msg = "✓ switched to " + newProv.DisplayName()
	}
	m.appendHistory(outputStyle.Render(promptStyle.Render(msg)))
	m = m.closeProviderSwitch()
	// Refresh slash commands from the new provider so /model, /resume,
	// etc. match. Same-provider swaps still re-probe so any cached
	// commands reflect whatever the new model unlocks.
	return m, newProv.ProbeInit(m.sessionArgs())
}

func (m model) viewProviderSwitch() string {
	titleText := "Provider"
	if m.providerSwitchLevel == 1 {
		if m.providerSwitchProvIdx >= 0 && m.providerSwitchProvIdx < len(providerRegistry) {
			titleText = providerRegistry[m.providerSwitchProvIdx].DisplayName() + " · Model"
		} else {
			titleText = "Model"
		}
	}
	title := themePickerTitleStyle.Render(titleText)

	var rows []string
	var innerW int
	switch m.providerSwitchLevel {
	case 0:
		opts := switcherProviderOptions()
		for _, o := range opts {
			if w := lipgloss.Width(o); w > innerW {
				innerW = w
			}
		}
		innerW += 4
		if innerW < 24 {
			innerW = 24
		}
		rows = renderSwitcherRows(opts, m.providerSwitchProvIdx, innerW)
	case 1:
		opts := switcherModelOptions(m.providerSwitchProvIdx)
		for _, o := range opts {
			if w := lipgloss.Width(o); w > innerW {
				innerW = w
			}
		}
		innerW += 4
		if innerW < 24 {
			innerW = 24
		}
		rows = renderSwitcherRows(opts, m.providerSwitchModelIdx, innerW)
	}

	var helpText string
	switch m.providerSwitchLevel {
	case 0:
		helpText = "↑↓ navigate · enter pick model · esc cancel"
	case 1:
		helpText = "↑↓ navigate · enter switch · esc back"
	}
	help := themePickerHelpStyle.Render(helpText)

	body := strings.Join([]string{
		title,
		"",
		strings.Join(rows, "\n"),
		"",
		help,
	}, "\n")
	return themePickerBoxStyle.Render(body)
}

func renderSwitcherRows(opts []string, cursor, width int) []string {
	rows := make([]string, 0, len(opts))
	for i, o := range opts {
		line := "  " + o
		if i == cursor {
			line = "▸ " + o
			pad := width - lipgloss.Width(line)
			if pad < 0 {
				pad = 0
			}
			line += strings.Repeat(" ", pad)
			line = themePickerRowStyle.Render(line)
		} else {
			pad := width - lipgloss.Width(line)
			if pad > 0 {
				line += strings.Repeat(" ", pad)
			}
		}
		rows = append(rows, line)
	}
	if len(rows) == 0 {
		rows = append(rows, themePickerRowStyle.Render("  (no options)"))
	}
	return rows
}
