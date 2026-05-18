package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// openConfigKeybindingsPicker shows the Keybindings submenu. Cursor
// lands on the first action; capture flag starts off so the user is
// navigating rows. Writes happen on capture-mode commit (Enter on a
// row → capture mode, next non-Esc keypress → save), matching the
// "writes through on Enter" pattern used by the memory picker.
func (m model) openConfigKeybindingsPicker() model {
	m.configKeybindingsPickerActive = true
	m.configKeybindingsCursor = 0
	m.configKeybindingsCapturing = false
	m.configKeybindingsError = ""
	return m
}

func (m model) closeConfigKeybindingsPicker() model {
	m.configKeybindingsPickerActive = false
	m.configKeybindingsCursor = 0
	m.configKeybindingsCapturing = false
	m.configKeybindingsError = ""
	return m
}

func (m model) updateConfigKeybindingsPicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.configKeybindingsCapturing {
		return m.updateConfigKeybindingsCapture(msg)
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		m = m.closeConfigKeybindingsPicker()
		return m, nil
	case msg.Code == tea.KeyUp:
		if m.configKeybindingsCursor > 0 {
			m.configKeybindingsCursor--
		}
		m.configKeybindingsError = ""
		return m, nil
	case msg.Code == tea.KeyDown:
		if m.configKeybindingsCursor < len(actionMeta)-1 {
			m.configKeybindingsCursor++
		}
		m.configKeybindingsError = ""
		return m, nil
	case msg.Code == tea.KeyEnter:
		if m.configKeybindingsCursor < 0 || m.configKeybindingsCursor >= len(actionMeta) {
			return m, nil
		}
		m.configKeybindingsCapturing = true
		m.configKeybindingsError = ""
		return m, nil
	case msg.Mod == 0 && msg.Code == 'r':
		// Reset the focused row to its compiled-in default. Without
		// this, a user who accidentally captured the wrong key has no
		// recovery path from inside the picker — they would have to
		// remember the default key to capture it back, or hand-edit
		// ~/.config/ask/ask.json. 'r' is safe to overload here because
		// row-navigation mode treats every other key (besides ↑↓ Enter
		// Esc Ctrl+C) as a no-op; capture mode is the only place where
		// 'r' could be recorded *as* a binding, and we don't shadow
		// that path.
		if m.configKeybindingsCursor < 0 || m.configKeybindingsCursor >= len(actionMeta) {
			return m, nil
		}
		action := actionMeta[m.configKeybindingsCursor].Action
		def := defaultKeyBindings[action]
		if err := persistKeyBinding(action, def); err != nil {
			debugLog("persistKeyBinding %s reset err: %v", action, err)
			m.configKeybindingsError = "reset failed: " + err.Error()
			return m, nil
		}
		invalidateKeyMapCache()
		m.configKeybindingsError = ""
		return m, nil
	}
	return m, nil
}

// updateConfigKeybindingsCapture eats the next keypress and records it
// as the binding for the row under the cursor. Esc cancels without
// persisting; Ctrl+C exits the picker entirely (matches other modals).
// All other keys — including Enter, Tab, Space, and the function keys
// — are valid bindings, so we don't filter further; the user gets
// whatever they pressed.
func (m model) updateConfigKeybindingsCapture(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Code == tea.KeyEsc {
		m.configKeybindingsCapturing = false
		m.configKeybindingsError = ""
		return m, nil
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'c' {
		m = m.closeConfigKeybindingsPicker()
		return m, nil
	}
	if m.configKeybindingsCursor < 0 || m.configKeybindingsCursor >= len(actionMeta) {
		m.configKeybindingsCapturing = false
		return m, nil
	}
	mod := stripKeyLockModifiers(msg.Mod)
	binding := KeyBinding{Mod: mod, Code: msg.Code}
	if binding.String() == "" {
		m.configKeybindingsError = "could not record that key — try a different combination"
		return m, nil
	}
	action := actionMeta[m.configKeybindingsCursor].Action
	if err := persistKeyBinding(action, binding); err != nil {
		debugLog("persistKeyBinding %s err: %v", action, err)
		m.configKeybindingsError = "save failed: " + err.Error()
		return m, nil
	}
	invalidateKeyMapCache()
	m.configKeybindingsCapturing = false
	m.configKeybindingsError = ""
	return m, nil
}

// persistKeyBinding loads the current config, writes the new binding
// (or omits it when it matches the default), and saves. Wrapped in
// withConfigLock so concurrent /config edits across goroutines stay
// safe — same discipline as the other config writers.
func persistKeyBinding(action Action, binding KeyBinding) error {
	return withConfigLock(func() error {
		cfg, _ := loadConfig()
		if cfg.Keybindings == nil {
			cfg.Keybindings = map[string]string{}
		}
		def, hasDefault := defaultKeyBindings[action]
		if hasDefault && binding == def {
			delete(cfg.Keybindings, string(action))
		} else {
			cfg.Keybindings[string(action)] = binding.String()
		}
		if len(cfg.Keybindings) == 0 {
			cfg.Keybindings = nil
		}
		return saveConfig(cfg)
	})
}

// viewConfigKeybindingsPicker mirrors the memory picker's chrome —
// title, table of rows, help line — so the /config aesthetic stays
// consistent. Capture mode swaps the body for a prompt.
func (m model) viewConfigKeybindingsPicker() string {
	km := currentKeyMap()
	rows := make([][2]string, 0, len(actionMeta))
	for _, am := range actionMeta {
		label := am.Label
		binding := km.Binding(am.Action).String()
		if binding == "" {
			binding = "(unbound)"
		}
		rows = append(rows, [2]string{label, binding})
	}
	innerW := keybindingsPickerInnerWidth(m.width, rows)

	title := themePickerTitleStyle.Render(truncateForRow("Keybindings", innerW))
	body := []string{title, ""}

	if m.configKeybindingsCapturing && m.configKeybindingsCursor >= 0 && m.configKeybindingsCursor < len(actionMeta) {
		current := km.Binding(actionMeta[m.configKeybindingsCursor].Action).String()
		if current == "" {
			current = "(unbound)"
		}
		prompt := "Press the new key combination for " +
			actionMeta[m.configKeybindingsCursor].Label +
			" (currently " + current + ")."
		body = append(body,
			configHelpStyle.Render(truncateForRow(prompt, innerW)),
			"",
			configHelpStyle.Render(truncateForRow("esc cancels without saving.", innerW)),
		)
	} else {
		for i, r := range rows {
			body = append(body, renderKeybindingPickerRow(r[0], r[1], innerW, i == m.configKeybindingsCursor))
		}
	}

	if m.configKeybindingsError != "" {
		body = append(body, "", themePickerHelpStyle.Render(truncateForRow("✗ "+m.configKeybindingsError, innerW)))
	}

	body = append(body,
		"",
		themePickerHelpStyle.Render(truncateForRow("↑↓ navigate · enter rebind · r reset · esc close", innerW)),
	)

	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

func renderKeybindingPickerRow(label, binding string, width int, selected bool) string {
	if width < 1 {
		return ""
	}
	label, binding = fitKeybindingPickerRowParts(label, binding, width)
	labelW := lipgloss.Width(label)
	bindingW := lipgloss.Width(binding)
	pad := width - labelW - bindingW
	minPad := 0
	if binding != "" {
		minPad = 1
	}
	if pad < minPad {
		pad = minPad
	}
	if selected {
		plain := label + strings.Repeat(" ", pad) + binding
		if w := lipgloss.Width(plain); w < width {
			plain += strings.Repeat(" ", width-w)
		}
		return configSelectedRowStyle.Render(plain)
	}
	line := label + strings.Repeat(" ", pad) + configKeyDimStyle.Render(binding)
	return padRight(line, width)
}

func keybindingsPickerInnerWidth(screenWidth int, rows [][2]string) int {
	innerW := 0
	for _, r := range rows {
		if w := lipgloss.Width(r[0]) + lipgloss.Width(r[1]) + 4; w > innerW {
			innerW = w
		}
	}
	if innerW < 36 {
		innerW = 36
	}
	if screenWidth > 0 {
		if maxInnerW := screenWidth - themePickerBoxStyle.GetHorizontalFrameSize(); maxInnerW < innerW {
			if maxInnerW < 1 {
				maxInnerW = 1
			}
			innerW = maxInnerW
		}
	}
	return innerW
}

func fitKeybindingPickerRowParts(label, binding string, width int) (string, string) {
	if width < 1 {
		return "", ""
	}
	labelW := lipgloss.Width(label)
	bindingW := lipgloss.Width(binding)
	if labelW+bindingW+1 <= width {
		return label, binding
	}
	bindingLimit := min(bindingW, max(1, width/2))
	binding = truncateForRow(binding, bindingLimit)
	bindingW = lipgloss.Width(binding)
	labelLimit := width - bindingW - 1
	if labelLimit < 1 {
		return truncateForRow(label, width), ""
	}
	return truncateForRow(label, labelLimit), binding
}
