package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// Ctrl+B opens the quick provider/model switcher. Level 0 lists every
// registered provider; once a provider is chosen, Level 1 reuses the
// shared /model ask modal for that provider instead of a separate
// switcher-specific picker. Applying both switches the current tab's
// provider/model in memory only and leaves persisted defaults alone.

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

// switcherFetchModelOptions calls the provider's ModelPicker once —
// the codex implementation does a live RPC — and returns the options
// with the optional "Enter your own" row appended.
func switcherFetchModelOptions(provIdx int) []string {
	if provIdx < 0 || provIdx >= len(providerRegistry) {
		return nil
	}
	return modelPickerOptions(providerRegistry[provIdx].ModelPicker())
}

// switcherCustomRowLabel is the single-source-of-truth label for the
// "Enter your own" row; stored once so the picker renderer and the
// cursor-detection logic stay in sync.
const switcherCustomRowLabel = "Enter your own"

// openProviderSwitch enters the quick switcher. Cursor starts on the
// current provider; Level 0 is always where we enter.
func (m model) openProviderSwitch() model {
	m.mode = modeProviderSwitch
	m.providerSwitchLevel = 0
	m.providerSwitchProvIdx = indexOfProvider(m.provider)
	return m
}

// closeProviderSwitch resets state and pops back to input mode.
func (m model) closeProviderSwitch() model {
	m.mode = modeInput
	m.providerSwitchLevel = 0
	m.providerSwitchProvIdx = 0
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
	return m.updateProviderSwitchLevel0(msg)
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
		// Fork the picker source once — for codex this does a live
		// model/list RPC — and hand the snapshot to the shared
		// /model modal.
		picker := providerRegistry[m.providerSwitchProvIdx].ModelPicker()
		opts := modelPickerOptions(picker)
		if len(opts) == 0 {
			// No model picker for this provider; apply the provider
			// switch immediately using the provider default model.
			return m.applyProviderSwitch("")
		}
		return m.startProviderSwitchModelPicker(picker), nil
	}
	return m, nil
}

func (m model) startProviderSwitchModelPicker(picker ProviderPicker) model {
	if m.providerSwitchProvIdx < 0 || m.providerSwitchProvIdx >= len(providerRegistry) {
		return m.closeProviderSwitch()
	}
	prov := providerRegistry[m.providerSwitchProvIdx]
	selectedModel := prov.LoadSettings().Model
	if m.provider != nil && m.provider.ID() == prov.ID() {
		selectedModel = m.providerModel
	}
	m.providerSwitchLevel = 1
	return m.startModelPickerWith(prov, picker, selectedModel, askForProviderSwitchModel)
}

func (m model) cancelProviderSwitchModelPicker() model {
	m = m.clearAsk()
	m.mode = modeProviderSwitch
	m.providerSwitchLevel = 0
	return m
}

// applyProviderSwitch swaps the current tab to providerRegistry[provIdx]
// with the given model, kills the active proc, and reloads the target
// provider's saved effort/slash-command defaults. Same-provider swaps
// (model-only changes) preserve sessionID/resumeCwd so the next
// ensureProc picks up where the conversation left off. No on-disk
// config is changed here; Ctrl+B is tab-local only.
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
	title := themePickerTitleStyle.Render("Provider")

	var innerW int
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
	rows := renderSwitcherRows(opts, m.providerSwitchProvIdx, innerW)

	help := themePickerHelpStyle.Render("↑↓ navigate · enter pick model · esc cancel")

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
