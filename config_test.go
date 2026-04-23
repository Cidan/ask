package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_MissingReturnsZero(t *testing.T) {
	isolateHome(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Provider != "" || cfg.Claude.Model != "" || cfg.Claude.Effort != "" ||
		len(cfg.Claude.SlashCommands) != 0 || cfg.Claude.Ollama.Host != "" ||
		cfg.UI.Theme != "" || cfg.UI.QuietMode != nil {
		t.Errorf("missing file should yield zero askConfig, got %+v", cfg)
	}
}

func TestSaveConfig_RoundTrip(t *testing.T) {
	home := isolateHome(t)
	qmTrue := true
	diffsTrue := true
	toolOutTrue := true
	worktreeTrue := true
	want := askConfig{
		Provider: "claude",
		Claude: claudeConfig{
			Model:  "opus",
			Effort: "high",
			SlashCommands: []providerSlashEntry{
				{Name: "extra", Description: "demo"},
			},
			Ollama: ollamaConfig{Host: "localhost:11434", Model: "llama3"},
		},
		UI: uiConfig{
			QuietMode:        &qmTrue,
			RenderDiffs:      &diffsTrue,
			RenderToolOutput: &toolOutTrue,
			Worktree:         &worktreeTrue,
			Theme:            "catppuccin-mocha",
		},
	}
	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.Provider != want.Provider {
		t.Errorf("Provider=%q want %q", got.Provider, want.Provider)
	}
	if got.Claude.Model != want.Claude.Model || got.Claude.Effort != want.Claude.Effort {
		t.Errorf("claude model/effort lost in roundtrip: %+v", got.Claude)
	}
	if len(got.Claude.SlashCommands) != 1 || got.Claude.SlashCommands[0].Name != "extra" {
		t.Errorf("slash commands: %+v", got.Claude.SlashCommands)
	}
	if got.Claude.Ollama != want.Claude.Ollama {
		t.Errorf("ollama lost: %+v", got.Claude.Ollama)
	}
	if got.UI.QuietMode == nil || *got.UI.QuietMode != true {
		t.Errorf("quietMode lost: %+v", got.UI.QuietMode)
	}
	if got.UI.RenderToolOutput == nil || *got.UI.RenderToolOutput != true {
		t.Errorf("renderToolOutput lost: %+v", got.UI.RenderToolOutput)
	}
	if got.UI.Theme != "catppuccin-mocha" {
		t.Errorf("theme lost: %q", got.UI.Theme)
	}

	// Permissions 0600 per saveConfig contract.
	path := filepath.Join(home, ".config", "ask", "ask.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config perm=%o want 0600", info.Mode().Perm())
	}
}

func TestSaveConfig_EmitsTrailingNewline(t *testing.T) {
	home := isolateHome(t)
	if err := saveConfig(askConfig{}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	path := filepath.Join(home, ".config", "ask", "ask.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Errorf("saveConfig should end with newline; last byte=%v", data[len(data)-1])
	}
}

func TestSaveConfig_FormatsJSONIndented(t *testing.T) {
	home := isolateHome(t)
	_ = saveConfig(askConfig{Provider: "claude"})
	path := filepath.Join(home, ".config", "ask", "ask.json")
	data, _ := os.ReadFile(path)
	var back askConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Errorf("config not parseable JSON: %v; data=%s", err, data)
	}
}

func TestValidateOllamaHost(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty", "", true},
		{"missing port", "localhost", true},
		{"host:port", "localhost:11434", false},
		{"bad port alpha", "localhost:abc", true},
		{"out of range", "localhost:99999", true},
		{"http scheme", "http://localhost:11434", false},
		{"https scheme", "https://example.com", false},
		{"https with port", "https://example.com:443", false},
		{"broken url", "http://", true},
	}
	for _, c := range cases {
		err := validateOllamaHost(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateOllamaHost(%q) err=%v wantErr=%v",
				c.name, c.in, err, c.wantErr)
		}
	}
}

func TestClaudeProviderSettings_RoundTrip(t *testing.T) {
	isolateHome(t)
	var p claudeProvider
	initial := p.LoadSettings()
	if initial.Model != "" || initial.Effort != "" || len(initial.SlashCommands) != 0 {
		t.Errorf("fresh settings not zero-valued: %+v", initial)
	}
	updated := ProviderSettings{
		Model:         "sonnet[1m]",
		Effort:        "xhigh",
		SlashCommands: []providerSlashEntry{{Name: "foo", Description: "bar"}},
	}
	if err := p.SaveSettings(updated); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := p.LoadSettings()
	if got.Model != updated.Model || got.Effort != updated.Effort {
		t.Errorf("Model/Effort lost: %+v", got)
	}
	if len(got.SlashCommands) != 1 || got.SlashCommands[0].Name != "foo" {
		t.Errorf("slash commands lost: %+v", got.SlashCommands)
	}
}

func TestConfigPath_UnderHome(t *testing.T) {
	home := isolateHome(t)
	path, err := configPath()
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	want := filepath.Join(home, ".config", "ask", "ask.json")
	if path != want {
		t.Errorf("configPath=%q want %q", path, want)
	}
}

func TestClaudeProviderSettings_PreservesOtherFields(t *testing.T) {
	isolateHome(t)
	// Seed unrelated fields in the on-disk config; SaveSettings must not nuke them.
	boolT := true
	cfg := askConfig{
		Provider: "claude",
		UI:       uiConfig{QuietMode: &boolT, Theme: "keep-me"},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}

	var p claudeProvider
	if err := p.SaveSettings(ProviderSettings{Model: "opus"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, _ := loadConfig()
	if got.UI.Theme != "keep-me" {
		t.Errorf("theme was overwritten: %q", got.UI.Theme)
	}
	if got.UI.QuietMode == nil || *got.UI.QuietMode != true {
		t.Errorf("quietMode pointer lost: %+v", got.UI.QuietMode)
	}
	if got.Claude.Model != "opus" {
		t.Errorf("model not persisted: %+v", got.Claude)
	}
}
