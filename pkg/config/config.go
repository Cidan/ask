package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

type ProviderSlashEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type APIProviderConfig struct {
	APIKey        string               `json:"apiKey,omitempty"`
	Model         string               `json:"model,omitempty"`
	BaseURL       string               `json:"baseURL,omitempty"`
	SlashCommands []ProviderSlashEntry `json:"slashCommands,omitempty"`
}

type VertexConfig struct {
	Project           string               `json:"project,omitempty"`
	Location          string               `json:"location,omitempty"`
	ServiceAccountKey string               `json:"serviceAccountKey,omitempty"`
	ServiceAccount    string               `json:"serviceAccount,omitempty"`
	Model             string               `json:"model,omitempty"`
	BaseURL           string               `json:"baseURL,omitempty"`
	SlashCommands     []ProviderSlashEntry `json:"slashCommands,omitempty"`
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
	ActiveWorkflow string                     `json:"activeWorkflow,omitempty"`
	Items          []json.RawMessage          `json:"items,omitempty"`
	Sessions       map[string]WorkflowSession `json:"sessions,omitempty"`
}

type WorkflowSession struct {
	Workflow  string    `json:"workflow"`
	StepIndex int       `json:"step_index"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

// ResolveBraveAPIKey returns the Brave Search API key from config or environment.
func ResolveBraveAPIKey(c WebSearchConfig) string {
	if k := strings.TrimSpace(c.BraveAPIKey); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv("BRAVE_API_KEY"))
}

// ProjectRoot returns the repository root for cwd, or the cleaned cwd if not in a repository.
func ProjectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return filepath.Clean(cwd)
	}
	abs = filepath.Clean(abs)
	dir := abs
	for {
		gitPath := filepath.Join(dir, ".git")
		if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		dir = parent
	}
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
		return SaveProjectLocked(cwd, pc)
	})
}

// SaveProjectLocked updates the per-project settings bag for cwd without acquiring the lock.
func SaveProjectLocked(cwd string, pc ProjectConfig) error {
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
}
