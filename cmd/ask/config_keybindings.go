package main

import (
	lipgloss "charm.land/lipgloss/v2"
)

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

// displayBinding stringifies a binding for /config rows. The zero-value
// (unbound) binding stringifies to "" — "(unbound)" keeps the row legible;
// every other binding round-trips through its canonical String() form.
func displayBinding(b KeyBinding) string {
	if s := b.String(); s != "" {
		return s
	}
	return "(unbound)"
}

// configKeybindingsGroupHeadingStyle is the heading style of a /config
// group: bold over the dim foreground lifts it without competing with the
// section title.
func configKeybindingsGroupHeadingStyle() lipgloss.Style {
	return dimStyle.Bold(true)
}
