package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

var configFileMu sync.Mutex

// WithConfigLock holds configFileMu around fn for atomic read-modify-write sequences.
func WithConfigLock(fn func() error) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()
	return fn()
}

// Config represents the user-global ask configuration (~/.config/ask/ask.json).
type Config struct {
	Provider     string                     `json:"provider,omitempty"`
	Effort       string                     `json:"effort,omitempty"`
	DeepSeek     APIProviderConfig          `json:"deepseek,omitempty"`
	Moonshot     APIProviderConfig          `json:"kimi,omitempty"`
	Anthropic    APIProviderConfig          `json:"anthropic,omitempty"`
	OpenAI       APIProviderConfig          `json:"openai,omitempty"`
	MiniMax      APIProviderConfig          `json:"minimax,omitempty"`
	GoogleAI     APIProviderConfig          `json:"googleai,omitempty"`
	Vertex       VertexConfig               `json:"vertex,omitempty"`
	UI           UIConfig                   `json:"ui,omitempty"`
	WebSearch    WebSearchConfig            `json:"webSearch,omitempty"`
	MCPServers   map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	Keybindings  map[string]string          `json:"keybindings,omitempty"`
	RecentModels []RecentModelRef           `json:"recentModels,omitempty"`
	Projects     map[string]ProjectConfig   `json:"projects,omitempty"`
}

type APIProviderConfig struct {
	APIKey string `json:"apiKey,omitempty"`
	Model  string `json:"model,omitempty"`
}

type VertexConfig struct {
	Project        string `json:"project,omitempty"`
	Location       string `json:"location,omitempty"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	Model          string `json:"model,omitempty"`
}

type UIConfig struct {
	GateTodosBeforeMutate bool        `json:"gateTodosBeforeMutate,omitempty"`
	Worktree              bool        `json:"worktree,omitempty"`
	Theme                 string      `json:"theme,omitempty"`
	Retry                 RetryConfig `json:"retry,omitempty"`
}

type RetryConfig struct {
	MaxRetries    int     `json:"maxRetries,omitempty"`
	InitialDelay  int     `json:"initialDelayMs,omitempty"`
	BackoffFactor float64 `json:"backoffFactor,omitempty"`
}

type WebSearchConfig struct {
	BraveAPIKey string `json:"braveApiKey,omitempty"`
}

type MCPServerConfig struct {
	Type         string            `json:"type,omitempty"`
	Command      string            `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	URL          string            `json:"url,omitempty"`
	OAuth        bool              `json:"oauth,omitempty"`
	Timeout      int               `json:"timeout,omitempty"`
	EnabledTools []string          `json:"enabledTools,omitempty"`
	DisabledTools []string         `json:"disabledTools,omitempty"`
	Disabled     bool              `json:"disabled,omitempty"`
}

type RecentModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type ProjectConfig struct {
	Issues     IssuesConfig               `json:"issues,omitempty"`
	Workflows  WorkflowsConfig            `json:"workflows,omitempty"`
	MCPServers map[string]MCPServerConfig `json:"mcpServers,omitempty"`
}

type IssuesConfig struct {
	Tracker string `json:"tracker,omitempty"` // "github" or "linear"
	Repo    string `json:"repo,omitempty"`    // owner/repo for GitHub
	Project string `json:"project,omitempty"` // Linear project slug/ID
	Team    string `json:"team,omitempty"`    // Linear team key
	Cycle   string `json:"cycle,omitempty"`   // Linear cycle name/number
}

type WorkflowsConfig struct {
	ActiveWorkflow string `json:"activeWorkflow,omitempty"`
}

// ConfigPath returns the standard configuration file path.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "ask.json"), nil
}

// Load reads and unmarshals the configuration file.
func Load() (Config, error) {
	p, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save marshals and writes the configuration file atomically with 0600 permissions.
func Save(cfg Config) error {
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadProject returns the project configuration for the canonical cwd.
func LoadProject(cwd string) (ProjectConfig, error) {
	cfg, err := Load()
	if err != nil {
		return ProjectConfig{}, err
	}
	if cfg.Projects == nil {
		return ProjectConfig{}, nil
	}
	canonical, err := filepath.Abs(cwd)
	if err != nil {
		canonical = cwd
	}
	return cfg.Projects[canonical], nil
}

// SaveProject updates the per-project settings bag for cwd.
func SaveProject(cwd string, pc ProjectConfig) error {
	return WithConfigLock(func() error {
		cfg, err := Load()
		if err != nil {
			return err
		}
		if cfg.Projects == nil {
			cfg.Projects = make(map[string]ProjectConfig)
		}
		canonical, err := filepath.Abs(cwd)
		if err != nil {
			canonical = cwd
		}
		cfg.Projects[canonical] = pc
		return Save(cfg)
	})
}
