package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type askConfig struct {
	// Provider is the agent CLI backend ID ("claude", "codex", …). Empty
	// means "use the first registered provider" — currently Claude.
	Provider string       `json:"provider,omitempty"`
	Claude   claudeConfig `json:"claude"`
	Codex    codexConfig  `json:"codex,omitempty"`
	UI       uiConfig     `json:"ui,omitempty"`
}

type claudeConfig struct {
	SlashCommands []providerSlashEntry `json:"slashCommands,omitempty"`
	Model         string               `json:"model,omitempty"`
	Effort        string               `json:"effort,omitempty"`
	Ollama        ollamaConfig         `json:"ollama,omitempty"`
}

type codexConfig struct {
	SlashCommands []providerSlashEntry `json:"slashCommands,omitempty"`
	Model         string               `json:"model,omitempty"`
	Effort        string               `json:"effort,omitempty"`
}

type ollamaConfig struct {
	Host  string `json:"host,omitempty"`
	Model string `json:"model,omitempty"`
}

type uiConfig struct {
	QuietMode          *bool  `json:"quietMode,omitempty"`
	CursorBlink        *bool  `json:"cursorBlink,omitempty"`
	RenderDiffs        *bool  `json:"renderDiffs,omitempty"`
	RenderToolOutput   *bool  `json:"renderToolOutput,omitempty"`
	SkipAllPermissions *bool  `json:"skipAllPermissions,omitempty"`
	Worktree           *bool  `json:"worktree,omitempty"`
	Theme              string `json:"theme,omitempty"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "ask.json"), nil
}

func loadConfig() (askConfig, error) {
	var cfg askConfig
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg, nil
}

func saveConfig(cfg askConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
