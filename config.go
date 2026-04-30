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
