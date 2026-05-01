package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// neo4jDefaultHost is what the picker fills in when the user has not
// configured a host explicitly. memmy talks Bolt, and Bolt's
// well-known port is 7687.
const (
	neo4jDefaultHost     = "localhost"
	neo4jDefaultPort     = 7687
	neo4jDefaultDatabase = "neo4j"
)

type askConfig struct {
	// Provider is the agent CLI backend ID ("claude", "codex", …). Empty
	// means "use the first registered provider" — currently Claude.
	Provider string       `json:"provider,omitempty"`
	Claude   claudeConfig `json:"claude"`
	Codex    codexConfig  `json:"codex,omitempty"`
	UI       uiConfig     `json:"ui,omitempty"`
	Memory   memoryConfig `json:"memory,omitempty"`

	// Projects holds per-project settings keyed by the canonical
	// absolute path of the project root. Issue-tracking is the first
	// per-project surface; future per-project knobs (custom slash
	// commands, tab-default cwd, project labels, etc.) plug in here
	// without growing the top-level struct.
	//
	// Lives in the user-global config file rather than a per-checkout
	// file because the most security-sensitive field — a GitHub PAT —
	// must NOT be committed. Putting it in ~/.config/ask/ask.json
	// (mode 0600) keeps tokens out of git, out of dotfile-sync repos,
	// and in one place per user.
	Projects map[string]projectConfig `json:"projects,omitempty"`
}

// projectConfig is the per-cwd settings bag. Empty struct = defaults
// (no issue provider configured, etc.). Map zero values are fine —
// loadProjectConfig returns the zero value when the project key is
// absent, so callers don't have to nil-check.
type projectConfig struct {
	Issues issuesConfig `json:"issues,omitempty"`
}

// issuesConfig is the per-project issue-tracking configuration.
// Provider names which IssueProvider implementation to dispatch to;
// the rest of the fields are provider-specific and only consulted
// when the matching Provider is selected. This shape lets us add a
// ClickUp / Linear / GitLab block as siblings to GitHub without
// migrating the existing on-disk config.
type issuesConfig struct {
	// Provider is the IssueProvider id ("github", "clickup", …) or
	// "" for "issues not configured for this project". The default
	// is the empty string, which surfaces the "Issues not
	// configured for this project" toast when the user opens the
	// issues screen.
	Provider string `json:"provider,omitempty"`

	GitHub githubIssuesConfig `json:"github,omitempty"`
}

// githubIssuesConfig wires the GitHub MCP server. Endpoint defaults
// to the official Copilot MCP host when blank; Token is the user's
// PAT. Token is held in 0600 config — the user explicitly rejected
// env-var indirection ("just config file for that specific project
// setting"), so it lives here.
type githubIssuesConfig struct {
	Endpoint string `json:"endpoint,omitempty"`
	Token    string `json:"token,omitempty"`
}

// githubIssuesDefaultEndpoint is the official GitHub Copilot-hosted
// MCP server. Used when githubIssuesConfig.Endpoint is empty.
const githubIssuesDefaultEndpoint = "https://api.githubcopilot.com/mcp"

// githubEndpointOrDefault applies the documented fallback so callers
// don't have to remember the constant.
func githubEndpointOrDefault(c githubIssuesConfig) string {
	if c.Endpoint == "" {
		return githubIssuesDefaultEndpoint
	}
	return c.Endpoint
}

// projectKey is the canonical key for projectConfig lookups —
// filepath.Clean of the absolute cwd path. Centralises the
// normalisation so two callers with the same logical project never
// collide on `/home/me/proj` vs `/home/me/proj/` vs `~/proj`.
func projectKey(cwd string) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return filepath.Clean(cwd)
	}
	return filepath.Clean(abs)
}

// loadProjectConfig returns the saved projectConfig for cwd, or the
// zero value when no entry exists. Pure read — does not mutate the
// underlying map. Empty cwd returns the zero value too.
func loadProjectConfig(cfg askConfig, cwd string) projectConfig {
	key := projectKey(cwd)
	if key == "" {
		return projectConfig{}
	}
	return cfg.Projects[key]
}

// upsertProjectConfig writes pc into cfg.Projects under cwd's key,
// creating the map if necessary. Returns the modified askConfig (the
// caller saves it). When pc is the zero value, the entry is dropped
// instead of stored so the on-disk file stays minimal.
func upsertProjectConfig(cfg askConfig, cwd string, pc projectConfig) askConfig {
	key := projectKey(cwd)
	if key == "" {
		return cfg
	}
	if (pc == projectConfig{}) {
		delete(cfg.Projects, key)
		if len(cfg.Projects) == 0 {
			cfg.Projects = nil
		}
		return cfg
	}
	if cfg.Projects == nil {
		cfg.Projects = make(map[string]projectConfig)
	}
	cfg.Projects[key] = pc
	return cfg
}

// memoryConfig holds the persistent memory toggle and embedder
// credentials. Memory is intentionally per-machine (not per-project):
// the integration plan partitions data via the {project: cwd} tenant
// tuple instead, so one global toggle is the right granularity.
//
// GeminiKey is the API key for Gemini embeddings. When set, the live
// memory service uses a Gemini embedder; when empty, opening the
// service surfaces an error in the picker (memory cannot run on the
// fake embedder in production — its embeddings are deterministic but
// semantically meaningless). The fake embedder is reserved for tests.
//
// The on-disk file already lives at mode 0600 per saveConfig's
// contract, so the key is no more exposed than any other field. We
// will revisit env-var indirection / keychain when remote backends
// land.
type memoryConfig struct {
	Enabled   *bool       `json:"enabled,omitempty"`
	GeminiKey string      `json:"geminiKey,omitempty"`
	Neo4j     neo4jConfig `json:"neo4j,omitempty"`
}

// neo4jConfig holds the Neo4j backend connection parameters. memmy
// v0.2.0 swapped the local bbolt store for a Neo4j-backed one, so the
// embedded library now needs a URI / credentials / database to dial at
// every Open. All fields are optional — empty values fall through to
// the documented defaults at call time so a fresh ask install can talk
// to a default-config Neo4j without any picker interaction.
type neo4jConfig struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
}

// neo4jHostOrDefault, neo4jPortOrDefault, neo4jDatabaseOrDefault apply
// the documented fallbacks so callers (memory.go, picker, validators)
// never have to reason about empty fields. Splitting the defaulting
// out of the struct lets the on-disk JSON stay minimal — only fields
// that differ from the default are persisted.
func neo4jHostOrDefault(c neo4jConfig) string {
	if c.Host == "" {
		return neo4jDefaultHost
	}
	return c.Host
}

func neo4jPortOrDefault(c neo4jConfig) int {
	if c.Port == 0 {
		return neo4jDefaultPort
	}
	return c.Port
}

func neo4jDatabaseOrDefault(c neo4jConfig) string {
	if c.Database == "" {
		return neo4jDefaultDatabase
	}
	return c.Database
}

// neo4jBoltURI assembles the bolt:// URI memmy expects from the
// host/port pair. Always returns a syntactically valid URI; callers
// should run validateNeo4jHost / validateNeo4jPort on the inputs first.
func neo4jBoltURI(c neo4jConfig) string {
	return fmt.Sprintf("bolt://%s:%d", neo4jHostOrDefault(c), neo4jPortOrDefault(c))
}

// validateNeo4jHost screens the picker input. We accept bare hostnames
// and IPs but reject schemes / slashes (URI is built by the helper, not
// pasted whole) and reject empty / whitespace-only strings.
func validateNeo4jHost(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("host is required")
	}
	if strings.ContainsAny(t, " /\\") {
		return errors.New("host must not contain spaces or slashes")
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "bolt://") || strings.HasPrefix(low, "neo4j://") ||
		strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return errors.New("enter only the hostname, no scheme")
	}
	return nil
}

// validateNeo4jPort screens the picker input. Must be 1..65535. The
// validator runs on the *string* the user typed so we can return a
// useful error when they paste a non-number; the caller converts to
// int once the validator returns nil.
func validateNeo4jPort(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("port is required")
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return errors.New("port must be a number")
		}
	}
	var n int
	if _, err := fmt.Sscanf(t, "%d", &n); err != nil || n < 1 || n > 65535 {
		return errors.New("port must be 1..65535")
	}
	return nil
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
