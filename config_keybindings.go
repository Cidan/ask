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
	case listNavPrev(msg):
		if m.configKeybindingsCursor > 0 {
			m.configKeybindingsCursor--
		}
		m.configKeybindingsError = ""
		return m, nil
	case listNavNext(msg):
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
	case msg.Mod == 0 && msg.Code == 'u':
		// Unbind the focused row — persists the zero-value KeyBinding,
		// which Matches() correctly treats as "never matches" so the
		// action is silently disabled. Peer of 'r'; same overload
		// argument applies (row-nav mode doesn't capture characters).
		if m.configKeybindingsCursor < 0 || m.configKeybindingsCursor >= len(actionMeta) {
			return m, nil
		}
		action := actionMeta[m.configKeybindingsCursor].Action
		if err := persistKeyBinding(action, KeyBinding{}); err != nil {
			debugLog("persistKeyBinding %s unbind err: %v", action, err)
			m.configKeybindingsError = "unbind failed: " + err.Error()
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
// title, grouped rows, help line — so the /config aesthetic stays
// consistent. Capture mode swaps the body for a prompt. Groups carry
// an inline heading and have a blank row between them; cursor math
// still walks the flat actionMeta so Up/Down skip the headings
// without special-casing.
func (m model) viewConfigKeybindingsPicker() string {
	km := currentKeyMap()
	rows := make([][2]string, 0, len(actionMeta))
	for _, am := range actionMeta {
		rows = append(rows, [2]string{am.Label, displayBinding(km.Binding(am.Action))})
	}
	innerW := keybindingsPickerInnerWidth(m.width, rows)

	var capturePrompt string
	if m.configKeybindingsCapturing && m.configKeybindingsCursor >= 0 && m.configKeybindingsCursor < len(actionMeta) {
		capturePrompt = "Press the new key combination for " +
			actionMeta[m.configKeybindingsCursor].Label +
			" (currently " + displayBinding(km.Binding(actionMeta[m.configKeybindingsCursor].Action)) + ")."
	}
	const helpFooter = "↑↓ navigate · enter rebind · r reset · u unbind · esc close"
	for _, g := range actionGroups {
		innerW = keybindingsExpandInnerWidth(m.width, innerW, g.Heading)
	}
	for _, line := range []string{capturePrompt, helpFooter} {
		if line == "" {
			continue
		}
		innerW = keybindingsExpandInnerWidth(m.width, innerW, line)
	}

	title := themePickerTitleStyle.Render(truncateForRow("Keybindings", innerW))
	body := []string{title, ""}

	if capturePrompt != "" {
		body = append(body,
			configHelpStyle.Render(truncateForRow(capturePrompt, innerW)),
			"",
			configHelpStyle.Render(truncateForRow("esc cancels without saving.", innerW)),
		)
	} else {
		flatIdx := 0
		for gi, g := range actionGroups {
			if gi > 0 {
				body = append(body, "")
			}
			body = append(body, configKeybindingsGroupHeadingStyle().Render(truncateForRow(g.Heading, innerW)))
			for _, item := range g.Items {
				binding := displayBinding(km.Binding(item.Action))
				body = append(body, renderKeybindingPickerRow(item.Label, binding, innerW, flatIdx == m.configKeybindingsCursor))
				flatIdx++
			}
		}
	}

	if m.configKeybindingsError != "" {
		body = append(body, "", themePickerHelpStyle.Render(truncateForRow("✗ "+m.configKeybindingsError, innerW)))
	}

	body = append(body,
		"",
		themePickerHelpStyle.Render(truncateForRow(helpFooter, innerW)),
	)

	return themePickerBoxStyle.Render(strings.Join(body, "\n"))
}

// displayBinding stringifies a binding for picker rows. The zero-value
// (unbound) binding stringifies to "" — we substitute "(unbound)" so
// the row stays legible; every other binding round-trips through its
// canonical String() form.
func displayBinding(b KeyBinding) string {
	if s := b.String(); s != "" {
		return s
	}
	return "(unbound)"
}

// configKeybindingsGroupHeadingStyle is the inline heading style for
// group rows in the /config keybindings picker. Bold over the dim
// foreground gives it visual lift without competing with the title.
func configKeybindingsGroupHeadingStyle() lipgloss.Style {
	return dimStyle.Bold(true)
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

// keybindingsExpandInnerWidth grows innerW to fit a single content line
// (capture prompt, help footer, etc.) that may exceed the row-list
// width, capped at the screen width minus the box frame. Mirrors the
// growth pattern viewConfigMemoryFieldInput uses for its field editor.
func keybindingsExpandInnerWidth(screenWidth, current int, line string) int {
	want := lipgloss.Width(line)
	if want < current {
		return current
	}
	if screenWidth > 0 {
		maxInnerW := screenWidth - themePickerBoxStyle.GetHorizontalFrameSize()
		if maxInnerW < 1 {
			maxInnerW = 1
		}
		if want > maxInnerW {
			want = maxInnerW
		}
	}
	return want
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
