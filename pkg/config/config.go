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
	Provider string `json:"provider,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// Providers holds one block per registered provider id. Providers
	// declare their own fields (pkg/providers SettingField); config only
	// stores them.
	Providers  map[string]ProviderConfig  `json:"providers,omitempty"`
	UI         UIConfig                   `json:"ui,omitempty"`
	WebSearch  WebSearchConfig            `json:"webSearch,omitempty"`
	MCPServers map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	// MCPDisabled overrides the effective enabled/disabled state of an MCP
	// server by name, regardless of which source defined it (project
	// .mcp.json, user/project config, or a plugin). true = disabled,
	// false = force-enabled. Absent = fall back to the server's own state.
	MCPDisabled  map[string]bool          `json:"mcpDisabled,omitempty"`
	Keybindings  map[string]string        `json:"keybindings,omitempty"`
	RecentModels []RecentModelRef         `json:"recentModels,omitempty"`
	Projects     map[string]ProjectConfig `json:"projects,omitempty"`
	Memory       MemoryConfig             `json:"memory,omitempty"`
}

// MemoryConfig picks the model that runs the post-turn concept
// extraction. Empty Provider means the session's provider; empty Model
// means that provider's cheapest listed model.
type MemoryConfig struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

type ProviderSlashEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ProviderConfig is one provider's block under Config.Providers: the
// model the user picked, the slash commands the provider discovered, and
// the settings the provider declares (an API key, a project id, …) keyed
// by their setting key. On disk the block is flat —
// {"model": "…", "apiKey": "…"} — so ask.json stays hand-editable and
// byte-compatible with the per-provider blocks it replaced.
type ProviderConfig struct {
	Model         string
	SlashCommands []ProviderSlashEntry
	Fields        map[string]string
}

const (
	providerConfigKeyModel         = "model"
	providerConfigKeySlashCommands = "slashCommands"
)

// ProviderConfigReservedKeys are the typed keys of a provider block; a
// provider must not declare a setting under one of them.
var ProviderConfigReservedKeys = []string{providerConfigKeyModel, providerConfigKeySlashCommands}

// Field returns the stored value for key, "" when unset.
func (pc ProviderConfig) Field(key string) string { return pc.Fields[key] }

// WithField returns a copy of pc with key set to value; an empty value
// removes the key. The Fields map is copied, never shared.
func (pc ProviderConfig) WithField(key, value string) ProviderConfig {
	fields := make(map[string]string, len(pc.Fields)+1)
	for k, v := range pc.Fields {
		fields[k] = v
	}
	if value == "" {
		delete(fields, key)
	} else {
		fields[key] = value
	}
	if len(fields) == 0 {
		fields = nil
	}
	pc.Fields = fields
	return pc
}

// IsEmpty reports whether the block carries nothing worth persisting.
func (pc ProviderConfig) IsEmpty() bool {
	return pc.Model == "" && len(pc.SlashCommands) == 0 && len(pc.Fields) == 0
}

func (pc ProviderConfig) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(pc.Fields)+2)
	for k, v := range pc.Fields {
		if v != "" {
			out[k] = v
		}
	}
	if pc.Model != "" {
		out[providerConfigKeyModel] = pc.Model
	}
	if len(pc.SlashCommands) > 0 {
		out[providerConfigKeySlashCommands] = pc.SlashCommands
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the flat block: the typed keys land on their
// fields, every other string-valued key lands in Fields. Non-string
// extras are dropped — provider settings are strings by contract.
func (pc *ProviderConfig) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*pc = ProviderConfig{}
	for k, v := range raw {
		switch k {
		case providerConfigKeyModel:
			if err := json.Unmarshal(v, &pc.Model); err != nil {
				return err
			}
		case providerConfigKeySlashCommands:
			if err := json.Unmarshal(v, &pc.SlashCommands); err != nil {
				return err
			}
		default:
			var s string
			if json.Unmarshal(v, &s) != nil || s == "" {
				continue
			}
			if pc.Fields == nil {
				pc.Fields = make(map[string]string)
			}
			pc.Fields[k] = s
		}
	}
	return nil
}

// ProviderConfig returns provider id's block, zero when absent.
func (c Config) ProviderConfig(id string) ProviderConfig { return c.Providers[id] }

// SetProviderConfig stores pc under id, dropping the entry (and the map,
// when it empties) for an empty block so ask.json never grows "{}" rows.
func (c *Config) SetProviderConfig(id string, pc ProviderConfig) {
	if pc.IsEmpty() {
		delete(c.Providers, id)
		if len(c.Providers) == 0 {
			c.Providers = nil
		}
		return
	}
	if c.Providers == nil {
		c.Providers = make(map[string]ProviderConfig)
	}
	c.Providers[id] = pc
}

type UIConfig struct {
	QuietMode          *bool          `json:"quietMode,omitempty"`
	CursorBlink        *bool          `json:"cursorBlink,omitempty"`
	RenderDiffs        *bool          `json:"renderDiffs,omitempty"`
	ToolOutput         string         `json:"toolOutput,omitempty"`
	SkipAllPermissions *bool          `json:"skipAllPermissions,omitempty"`
	Worktree           *bool          `json:"worktree,omitempty"`
	Theme              string         `json:"theme,omitempty"`
	Retry              *RetryUIConfig `json:"retry,omitempty"`
}

type RetryUIConfig struct {
	MaxRetries     *int     `json:"maxRetries,omitempty"`
	InitialDelayMs *int     `json:"initialDelayMs,omitempty"`
	BackoffFactor  *float64 `json:"backoffFactor,omitempty"`
}

type RetryConfig = RetryUIConfig

const (
	AgentDefaultMaxRetries     = 4
	AgentDefaultInitialDelayMs = 2000
	AgentDefaultBackoffFactor  = 2.0
)

func AgentRetryOptions(cfg Config) (maxRetries int, initialDelay time.Duration, backoffFactor float64) {
	maxRetries = AgentDefaultMaxRetries
	initialDelay = time.Duration(AgentDefaultInitialDelayMs) * time.Millisecond
	backoffFactor = AgentDefaultBackoffFactor
	if cfg.UI.Retry != nil {
		if v := cfg.UI.Retry.MaxRetries; v != nil {
			maxRetries = *v
		}
		if v := cfg.UI.Retry.InitialDelayMs; v != nil {
			initialDelay = time.Duration(*v) * time.Millisecond
		}
		if v := cfg.UI.Retry.BackoffFactor; v != nil {
			backoffFactor = *v
		}
	}
	return
}

type WebSearchConfig struct {
	BraveAPIKey string `json:"braveApiKey,omitempty"`
}

type MCPServerConfig struct {
	Type          string            `json:"type,omitempty"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	URL           string            `json:"url,omitempty"`
	OAuth         bool              `json:"oauth,omitempty"`
	Timeout       int               `json:"timeout,omitempty"`
	EnabledTools  []string          `json:"enabledTools,omitempty"`
	DisabledTools []string          `json:"disabledTools,omitempty"`
	Disabled      bool              `json:"disabled,omitempty"`
}

type RecentModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type ProjectConfig struct {
	Issues      IssuesConfig               `json:"issues,omitempty"`
	MCP         ProjectMCPConfig           `json:"mcp,omitempty"`
	Workflows   WorkflowsConfig            `json:"workflows,omitempty"`
	MCPServers  map[string]MCPServerConfig `json:"mcpServers,omitempty"`
	MCPDisabled map[string]bool            `json:"mcpDisabled,omitempty"`
	Worktree    *bool                      `json:"worktree,omitempty"`
}

// WorktreeEnabled resolves the effective worktree flag for cwd: the
// project override when set, else the global UI.Worktree, else false.
func WorktreeEnabled(cfg Config, cwd string) bool {
	if v := LoadProjectConfig(cfg, cwd).Worktree; v != nil {
		return *v
	}
	return cfg.UI.Worktree != nil && *cfg.UI.Worktree
}

type IssuesConfig struct {
	Provider string `json:"provider,omitempty"`
	Tracker  string `json:"tracker,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Project  string `json:"project,omitempty"`
	Team     string `json:"team,omitempty"`
	Cycle    string `json:"cycle,omitempty"`
}

type ProjectMCPConfig struct {
	GitHub GitHubMCPConfig `json:"github,omitempty"`
	Linear LinearMCPConfig `json:"linear,omitempty"`
}

type GitHubMCPConfig struct {
	Endpoint string `json:"endpoint,omitempty"`
	Token    string `json:"token,omitempty"`
}

const GitHubMCPDefaultEndpoint = "https://api.githubcopilot.com/mcp"

func GitHubMCPEndpointOrDefault(c GitHubMCPConfig) string {
	if c.Endpoint == "" {
		return GitHubMCPDefaultEndpoint
	}
	return c.Endpoint
}

type LinearMCPConfig struct {
	Endpoint string `json:"endpoint,omitempty"`
	Token    string `json:"token,omitempty"`
	TeamKey  string `json:"teamKey,omitempty"`
}

const LinearGraphQLDefaultEndpoint = "https://api.linear.app/graphql"

func LinearGraphQLEndpointOrDefault(c LinearMCPConfig) string {
	if c.Endpoint == "" {
		return LinearGraphQLDefaultEndpoint
	}
	return c.Endpoint
}

type WorkflowsConfig struct {
	ActiveWorkflow string                     `json:"activeWorkflow,omitempty"`
	Items          []WorkflowDef              `json:"items,omitempty"`
	Sessions       map[string]WorkflowSession `json:"sessions,omitempty"`
}

type WorkflowDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Steps       []WorkflowStep `json:"steps,omitempty"`
	Scope       string         `json:"-"`
	// Plugin is the "name@marketplace" that ships this workflow when
	// Scope == WorkflowScopePlugin (read-only).
	Plugin string `json:"-"`
}

const (
	WorkflowScopeUser   = "user"
	WorkflowScopeRepo   = "repo"
	WorkflowScopeGlobal = "global"
	WorkflowScopePlugin = "plugin"

	WorkflowStepKindAgent = ""
	WorkflowStepKindLoop  = "loop"

	WorkflowLoopBreak    = "break"
	WorkflowLoopContinue = "continue"

	WorkflowLoopDefaultMaxIterations = 10
)

type WorkflowStep struct {
	Name          string         `json:"name"`
	Kind          string         `json:"kind,omitempty"`
	Provider      string         `json:"provider,omitempty"`
	Model         string         `json:"model,omitempty"`
	Prompt        string         `json:"prompt,omitempty"`
	Steps         []WorkflowStep `json:"steps,omitempty"`
	MaxIterations int            `json:"maxIterations,omitempty"`
	ExitCondition string         `json:"exitCondition,omitempty"`
}

func (s WorkflowStep) IsLoop() bool { return s.Kind == WorkflowStepKindLoop }

func (s WorkflowStep) EffectiveMaxIterations() int {
	if s.MaxIterations > 0 {
		return s.MaxIterations
	}
	return WorkflowLoopDefaultMaxIterations
}

type WorkflowSession struct {
	Workflow  string    `json:"workflow"`
	StepIndex int       `json:"stepIndex"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *WorkflowSession) UnmarshalJSON(data []byte) error {
	type alias struct {
		Workflow   string    `json:"workflow"`
		StepIndex  int       `json:"stepIndex"`
		StepIndex2 int       `json:"step_index"`
		Status     string    `json:"status"`
		StartedAt  time.Time `json:"startedAt"`
		StartedAt2 time.Time `json:"started_at"`
		UpdatedAt  time.Time `json:"updatedAt"`
		UpdatedAt2 time.Time `json:"updated_at"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	s.Workflow = a.Workflow
	s.Status = a.Status
	s.StepIndex = a.StepIndex
	if s.StepIndex == 0 && a.StepIndex2 != 0 {
		s.StepIndex = a.StepIndex2
	}
	s.StartedAt = a.StartedAt
	if s.StartedAt.IsZero() && !a.StartedAt2.IsZero() {
		s.StartedAt = a.StartedAt2
	}
	s.UpdatedAt = a.UpdatedAt
	if s.UpdatedAt.IsZero() && !a.UpdatedAt2.IsZero() {
		s.UpdatedAt = a.UpdatedAt2
	}
	return nil
}

// ConfigPath returns the standard configuration file path.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "ask.json"), nil
}

// Load reads and unmarshals the configuration file, applying legacy migrations.
func Load() (Config, error) {
	var cfg Config
	p, err := ConfigPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	_ = json.Unmarshal(data, &cfg)
	MigrateLegacyProviderEffort(&cfg, data)
	MigrateLegacyProviderBlocks(&cfg, data)
	MigrateLegacyToolOutput(&cfg, data)
	MigrateLegacyIssuesGitHub(&cfg, data)
	return cfg, nil
}

// Save marshals and writes the configuration file atomically with 0600 permissions.
func Save(cfg Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".ask.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

// MigrateLegacyProviderEffort folds old per-provider effort fields into the
// new global Config.Effort field. It maps the first non-empty value found.
func MigrateLegacyProviderEffort(cfg *Config, data []byte) {
	if cfg.Effort != "" {
		return
	}
	var legacy struct {
		Anthropic struct {
			Effort string `json:"effort,omitempty"`
		} `json:"anthropic,omitempty"`
		OpenAI struct {
			Effort string `json:"effort,omitempty"`
		} `json:"openai,omitempty"`
		OpenRouter struct {
			Effort string `json:"effort,omitempty"`
		} `json:"openrouter,omitempty"`
		DeepSeek struct {
			Effort string `json:"effort,omitempty"`
		} `json:"deepseek,omitempty"`
		GoogleAI struct {
			Effort string `json:"effort,omitempty"`
		} `json:"googleai,omitempty"`
		Vertex struct {
			Effort string `json:"effort,omitempty"`
		} `json:"vertex,omitempty"`
		Kimi struct {
			Effort string `json:"effort,omitempty"`
		} `json:"kimi,omitempty"`
		MiniMax struct {
			Effort string `json:"effort,omitempty"`
		} `json:"minimax,omitempty"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return
	}

	var found string
	if legacy.Anthropic.Effort != "" {
		found = legacy.Anthropic.Effort
	} else if legacy.OpenRouter.Effort != "" {
		found = legacy.OpenRouter.Effort
	} else if legacy.OpenAI.Effort != "" {
		found = legacy.OpenAI.Effort
	} else if legacy.DeepSeek.Effort != "" {
		found = legacy.DeepSeek.Effort
	} else if legacy.GoogleAI.Effort != "" {
		found = legacy.GoogleAI.Effort
	} else if legacy.Vertex.Effort != "" {
		found = legacy.Vertex.Effort
	} else if legacy.Kimi.Effort != "" {
		found = legacy.Kimi.Effort
	} else if legacy.MiniMax.Effort != "" {
		found = legacy.MiniMax.Effort
	}

	switch found {
	case "off", "minimal", "low":
		cfg.Effort = "low"
	case "medium":
		cfg.Effort = "medium"
	case "high", "xhigh", "max":
		cfg.Effort = "high"
	}
}

// legacyProviderBlocks are the pre-registry top-level provider blocks and
// the keys each carried that are not provider settings any more: effort
// was folded into Config.Effort, serviceAccount was an alias of
// serviceAccountKey, and vertex's baseURL was never read.
var legacyProviderBlocks = map[string][]string{
	"vertex":     {"effort", "serviceAccount", "baseURL"},
	"openrouter": {"effort"},
}

// MigrateLegacyProviderBlocks lifts the pre-registry top-level provider
// blocks ("vertex": {…}, "openrouter": {…}) into Config.Providers. Each
// block is already flat, so it decodes as a ProviderConfig directly; the
// keys listed in legacyProviderBlocks are dropped after the serviceAccount
// alias is honoured. Runs only for ids absent from the new map — an
// explicit new block always wins.
func MigrateLegacyProviderBlocks(cfg *Config, data []byte) {
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(data, &legacy); err != nil {
		return
	}
	for id, drop := range legacyProviderBlocks {
		raw, ok := legacy[id]
		if !ok {
			continue
		}
		if _, present := cfg.Providers[id]; present {
			continue
		}
		var pc ProviderConfig
		if err := json.Unmarshal(raw, &pc); err != nil {
			continue
		}
		if pc.Field("serviceAccountKey") == "" && pc.Field("serviceAccount") != "" {
			pc = pc.WithField("serviceAccountKey", pc.Field("serviceAccount"))
		}
		for _, key := range drop {
			pc = pc.WithField(key, "")
		}
		cfg.SetProviderConfig(id, pc)
	}
}

// MigrateLegacyToolOutput folds the deprecated "renderToolOutput" bool
// into the new tri-state "toolOutput" string so users who upgrade don't
// see their tool rendering reset on first launch. Runs only when the
// new key is absent — an explicit new setting always wins.
func MigrateLegacyToolOutput(cfg *Config, data []byte) {
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
		cfg.UI.ToolOutput = "short"
	} else {
		cfg.UI.ToolOutput = "off"
	}
}

// MigrateLegacyIssuesGitHub lifts the old per-project
// `issues.github.{endpoint,token}` block into the new
// `mcp.github.{endpoint,token}` slot.
func MigrateLegacyIssuesGitHub(cfg *Config, data []byte) {
	if len(cfg.Projects) == 0 {
		return
	}
	var legacy struct {
		Projects map[string]struct {
			Issues struct {
				GitHub struct {
					Endpoint string `json:"endpoint,omitempty"`
					Token    string `json:"token,omitempty"`
				} `json:"github,omitempty"`
			} `json:"issues,omitempty"`
		} `json:"projects,omitempty"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return
	}
	for key, lp := range legacy.Projects {
		pc, ok := cfg.Projects[key]
		if !ok {
			continue
		}
		if pc.MCP.GitHub.Endpoint == "" && lp.Issues.GitHub.Endpoint != "" {
			pc.MCP.GitHub.Endpoint = lp.Issues.GitHub.Endpoint
		}
		if pc.MCP.GitHub.Token == "" && lp.Issues.GitHub.Token != "" {
			pc.MCP.GitHub.Token = lp.Issues.GitHub.Token
		}
		cfg.Projects[key] = pc
	}
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

// ProjectKey returns the canonical key for project lookups.
func ProjectKey(cwd string) string {
	if cwd == "" {
		return ""
	}
	return ProjectRoot(cwd)
}

// LoadProjectConfig returns the saved ProjectConfig for cwd, or zero value if absent.
func LoadProjectConfig(cfg Config, cwd string) ProjectConfig {
	key := ProjectKey(cwd)
	if key == "" {
		return ProjectConfig{}
	}
	return cfg.Projects[key]
}

// LoadProject returns the project configuration for the canonical cwd.
func LoadProject(cwd string) (ProjectConfig, error) {
	cfg, err := Load()
	if err != nil {
		return ProjectConfig{}, err
	}
	return LoadProjectConfig(cfg, cwd), nil
}

// IsProjectConfigEmpty reports whether pc carries no user-meaningful data.
func IsProjectConfigEmpty(pc ProjectConfig) bool {
	if pc.Issues != (IssuesConfig{}) {
		return false
	}
	if pc.MCP != (ProjectMCPConfig{}) {
		return false
	}
	if len(pc.Workflows.Items) > 0 || len(pc.Workflows.Sessions) > 0 {
		return false
	}
	if len(pc.MCPServers) > 0 {
		return false
	}
	if len(pc.MCPDisabled) > 0 {
		return false
	}
	if pc.Worktree != nil {
		return false
	}
	return true
}

// UpsertProjectConfig writes pc into cfg.Projects under cwd's key.
func UpsertProjectConfig(cfg Config, cwd string, pc ProjectConfig) Config {
	key := ProjectKey(cwd)
	if key == "" {
		return cfg
	}
	if IsProjectConfigEmpty(pc) {
		delete(cfg.Projects, key)
		if len(cfg.Projects) == 0 {
			cfg.Projects = nil
		}
		return cfg
	}
	if cfg.Projects == nil {
		cfg.Projects = make(map[string]ProjectConfig)
	}
	cfg.Projects[key] = pc
	return cfg
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
	cfg = UpsertProjectConfig(cfg, cwd, pc)
	return Save(cfg)
}
