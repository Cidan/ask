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

// switcherFetchModelOptions calls the provider's ModelPicker once —
// the codex implementation does a live RPC — and returns the options
// with the optional "Enter your own" row appended. Callers stash the
// result on the model so per-keystroke renders don't re-trigger the
// RPC fork.
func switcherFetchModelOptions(provIdx int) []string {
	if provIdx < 0 || provIdx >= len(providerRegistry) {
		return nil
	}
	picker := providerRegistry[provIdx].ModelPicker()
	out := append([]string{}, picker.Options...)
	if picker.AllowCustom {
		out = append(out, switcherCustomRowLabel)
	}
	return out
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
	m.providerSwitchModelIdx = 0
	m.providerSwitchCustomActive = false
	m.providerSwitchCustomText = ""
	m.providerSwitchModelOpts = nil
	return m
}

// closeProviderSwitch resets state and pops back to input mode.
func (m model) closeProviderSwitch() model {
	m.mode = modeInput
	m.providerSwitchLevel = 0
	m.providerSwitchProvIdx = 0
	m.providerSwitchModelIdx = 0
	m.providerSwitchCustomActive = false
	m.providerSwitchCustomText = ""
	m.providerSwitchModelOpts = nil
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
		// Fork the picker source once — for codex this does a live
		// model/list RPC — and cache the snapshot for the duration
		// of this Level 1 session.
		opts := switcherFetchModelOptions(m.providerSwitchProvIdx)
		if len(opts) == 0 {
			// No model picker for this provider; apply the provider
			// switch immediately with whatever model the provider has
			// saved (may be empty → provider default).
			return m.applyProviderSwitch("")
		}
		m.providerSwitchLevel = 1
		m.providerSwitchModelOpts = opts
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
	if m.providerSwitchCustomActive {
		return m.updateProviderSwitchCustom(msg)
	}
	opts := m.providerSwitchModelOpts
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
		label := opts[m.providerSwitchModelIdx]
		if label == switcherCustomRowLabel {
			m.providerSwitchCustomActive = true
			m.providerSwitchCustomText = providerRegistry[m.providerSwitchProvIdx].LoadSettings().Model
			return m, nil
		}
		return m.applyProviderSwitch(switcherModelFromLabel(label))
	}
	return m, nil
}

// updateProviderSwitchCustom handles typing a custom model id once
// the user has selected the "Enter your own" row. Enter applies the
// typed value (when non-empty); Esc pops back to the list while
// preserving typed text so a misclick doesn't lose it.
func (m model) updateProviderSwitchCustom(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c':
		return m.closeProviderSwitch(), nil
	case msg.Code == tea.KeyEsc:
		m.providerSwitchCustomActive = false
		return m, nil
	case msg.Code == tea.KeyBackspace:
		if m.providerSwitchCustomText != "" {
			r := []rune(m.providerSwitchCustomText)
			m.providerSwitchCustomText = string(r[:len(r)-1])
		}
		return m, nil
	case msg.Code == tea.KeyEnter:
		picked := strings.TrimSpace(m.providerSwitchCustomText)
		if picked == "" {
			// Empty submit is a no-op so users can't accidentally
			// clear their model on a blank press.
			return m, nil
		}
		return m.applyProviderSwitch(picked)
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		m.providerSwitchCustomText += msg.Text
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
		opts := m.providerSwitchModelOpts
		for _, o := range opts {
			if w := lipgloss.Width(o); w > innerW {
				innerW = w
			}
		}
		innerW += 4
		if innerW < 32 {
			innerW = 32
		}
		if m.providerSwitchCustomActive {
			rows = []string{renderSwitcherCustomRow(m.providerSwitchCustomText, innerW)}
		} else {
			rows = renderSwitcherRows(opts, m.providerSwitchModelIdx, innerW)
		}
	}

	var helpText string
	switch {
	case m.providerSwitchLevel == 1 && m.providerSwitchCustomActive:
		helpText = "type model id · enter apply · esc back"
	case m.providerSwitchLevel == 0:
		helpText = "↑↓ navigate · enter pick model · esc cancel"
	case m.providerSwitchLevel == 1:
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

// renderSwitcherCustomRow draws the typing surface that replaces the
// option list when the user lands on "Enter your own". A trailing
// caret shows where input lands; empty text renders a dim
// placeholder so the row doesn't collapse to just the cursor.
func renderSwitcherCustomRow(text string, width int) string {
	label := "▸ "
	caret := askCaretStyle.Render("▏")
	body := text
	if body == "" {
		body = askPlaceholder.Render("type model id") + caret
	} else {
		body = body + caret
	}
	line := label + body
	pad := width - lipgloss.Width(line)
	if pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return themePickerRowStyle.Render(line)
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
