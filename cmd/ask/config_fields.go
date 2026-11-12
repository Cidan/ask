package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/providers"
)

// webSearchFields is the Web search section: one secret, the Brave key,
// read per call by the web_search tool.
var webSearchFields = []providers.SettingField{{
	Key:    "braveApiKey",
	Title:  "Brave API key",
	Hint:   "Brave Search key for web_search on providers without first-party search",
	Secret: true,
	EnvKey: braveEnvAPIKey,
}}

// memoryConfigSummary is the extraction model row's value: the configured
// pair, or what the extractor falls back to.
func memoryConfigSummary(cfg askConfig) string {
	switch {
	case cfg.Memory.Provider != "" && cfg.Memory.Model != "":
		return cfg.Memory.Provider + "/" + cfg.Memory.Model
	case cfg.Memory.Provider != "":
		return cfg.Memory.Provider + " (cheapest)"
	case cfg.Memory.Model != "":
		return cfg.Memory.Model
	}
	return "session provider (cheapest)"
}

// deslopConfigSummary is the deslop model row's value.
func deslopConfigSummary(cfg askConfig) string {
	switch {
	case cfg.Deslop.Provider != "" && cfg.Deslop.Model != "":
		return cfg.Deslop.Provider + "/" + cfg.Deslop.Model
	case cfg.Deslop.Provider != "":
		return cfg.Deslop.Provider + " (cheapest)"
	}
	return "(not set)"
}

// openDeslopModelPicker opens the shared model picker retargeted at the deslop
// rewrite model: a pick writes cfg.Deslop.Provider/Model instead of switching
// the live tab. Opened from /config → Session → Deslop model; on close the
// picker returns to /config.
func (m model) openDeslopModelPicker() (model, tea.Cmd) {
	cfg, _ := loadConfig()
	s := buildModelPickerState(cfg)
	s.deslopTarget = true
	s.seedCursor(cfg.Deslop.Provider, cfg.Deslop.Model)
	m.modelPicker = s
	m.mode = modeModelPicker
	return m, m.modelPickerLoadCmd(false)
}

// applyModelPickerToDeslop is the deslop-targeted terminal action for the
// shared picker: it writes the chosen provider+model onto cfg.Deslop, persists,
// and returns to /config. Called from applyModelPickerEntry when
// modelPicker.deslopTarget is set.
func (m model) applyModelPickerToDeslop(entry modelPickerEntry) (tea.Model, tea.Cmd) {
	provider := entry.providerID
	modelID := entry.modelID
	if strings.EqualFold(modelID, "default") {
		modelID = ""
	}
	saved, err := saveConfigWith(func(c *askConfig) {
		c.Deslop.Provider = provider
		c.Deslop.Model = modelID
	})
	m = m.closeModelPickerToConfig()
	if err != nil {
		return m.configSaveFailed(err)
	}
	m.config.say("✓ Deslop model → "+deslopConfigSummary(saved), false)
	return m, nil
}

// openMemoryModelPicker opens the shared full-frame model picker
// (model_picker.go) retargeted at the memory extraction override: a pick
// writes cfg.Memory.Provider/Model instead of switching the live tab, and the
// list carries a synthetic "Automatic" row (injectMemoryAutoRow) that clears
// the override back to the session provider + cheapest model. Opened from
// /config → Memory; the cursor is seeded from the stored pair, and on close
// the picker returns to /config.
func (m model) openMemoryModelPicker() (model, tea.Cmd) {
	cfg, _ := loadConfig()
	s := buildModelPickerState(cfg)
	s.memoryTarget = true
	s.injectMemoryAutoRow()
	s.seedCursor(cfg.Memory.Provider, cfg.Memory.Model)
	m.modelPicker = s
	m.mode = modeModelPicker
	return m, m.modelPickerLoadCmd(false)
}

// applyModelPickerToMemory is the memory-targeted terminal action for the
// shared picker: it writes the chosen provider+model onto cfg.Memory (both
// cleared for the synthetic Automatic row, whose providerID is empty),
// persists, and returns to /config. Called from applyModelPickerEntry when
// modelPicker.memoryTarget is set.
func (m model) applyModelPickerToMemory(entry modelPickerEntry) (tea.Model, tea.Cmd) {
	provider := entry.providerID
	modelID := entry.modelID
	if strings.EqualFold(modelID, "default") {
		modelID = ""
	}
	saved, err := saveConfigWith(func(c *askConfig) {
		c.Memory.Provider = provider
		c.Memory.Model = modelID
	})
	m = m.closeModelPickerToConfig()
	if err != nil {
		return m.configSaveFailed(err)
	}
	m.config.say("✓ Extraction model → "+memoryConfigSummary(saved), false)
	return m, nil
}

// closeModelPickerToConfig dismisses the model picker back to /config (not
// the chat input), used when the picker was opened from a /config row. The
// screen's state was kept while the picker was up, so the user lands on the
// row they opened it from.
func (m model) closeModelPickerToConfig() model {
	m.mode = modeConfig
	m.modelPicker = nil
	if m.config == nil {
		m.config = &configScreenState{}
	}
	return m
}
