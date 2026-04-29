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
	Memory   memoryConfig `json:"memory,omitempty"`
}

// memoryConfig holds the persistent memory toggle and (eventually)
// backend selection. First slice ships only Enabled — a *bool so
// "absent" reads as "user has not opted in yet" and Backend / Local /
// Remotes can be added without breaking JSON round-trip when remotes
// land. Memory is intentionally per-machine (not per-project): the
// integration plan partitions data via the {project: cwd} tenant
// tuple instead, so one global toggle is the right granularity.
type memoryConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
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
	QuietMode   *bool `json:"quietMode,omitempty"`
	CursorBlink *bool `json:"cursorBlink,omitempty"`
	RenderDiffs *bool `json:"renderDiffs,omitempty"`
	// ToolOutput is the tri-state for tool-call rendering:
	// "full" | "short" | "off". Empty string defers to
	// defaultToolOutputMode.
	ToolOutput         string `json:"toolOutput,omitempty"`
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
	migrateLegacyToolOutput(&cfg, data)
	return cfg, nil
}

// migrateLegacyToolOutput folds the deprecated "renderToolOutput" bool
// into the new tri-state "toolOutput" string so users who upgrade don't
// see their tool rendering reset on first launch. Runs only when the
// new key is absent — an explicit new setting always wins.
func migrateLegacyToolOutput(cfg *askConfig, data []byte) {
	if cfg.UI.ToolOutput != "" {
		return
	}
	var legacy struct {
		UI struct {
			RenderToolOutput *bool `json:"renderToolOutput,omitempty"`
		} `json:"ui,omitempty"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return
	}
	if legacy.UI.RenderToolOutput == nil {
		return
	}
	if *legacy.UI.RenderToolOutput {
		cfg.UI.ToolOutput = string(toolOutputShort)
	} else {
		cfg.UI.ToolOutput = string(toolOutputOff)
	}
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
