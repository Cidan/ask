package providers

import (
	"fmt"
	"os"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/config"
)

// Conventional environment fallbacks consulted when the config field is empty.
const (
	DeepSeekEnvAPIKey  = "DEEPSEEK_API_KEY"
	MoonshotEnvAPIKey  = "MOONSHOT_API_KEY"
	AnthropicEnvAPIKey = "ANTHROPIC_API_KEY"
	OpenAIEnvAPIKey    = "OPENAI_API_KEY"
	BraveEnvAPIKey     = "BRAVE_API_KEY"
	MiniMaxEnvAPIKey   = "MINIMAX_API_KEY"
	GoogleAIEnvAPIKey  = "GOOGLE_API_KEY"
)

// Default API endpoints for OpenAI-compatible providers.
const (
	DeepSeekDefaultBaseURL = "https://api.deepseek.com/v1"
	MoonshotDefaultBaseURL = "https://api.moonshot.ai/v1"
	MiniMaxDefaultBaseURL  = "https://api.minimax.io/v1"
)

// ProviderSettings is the per-provider slice of configuration.
type ProviderSettings struct {
	Model         string                      `json:"model"`
	Effort        string                      `json:"effort"`
	SlashCommands []config.ProviderSlashEntry `json:"slashCommands,omitempty"`
}

// AgentProviderSpec describes one fantasy-backed in-process API provider.
type AgentProviderSpec struct {
	ID              string
	DisplayName     string
	DefaultModel    string
	ModelOptions    []string
	EffortOptions   []string
	BuildModel      func(cfg config.Config, modelID string) (fantasy.LanguageModel, error)
	CallOptions     func(modelID, effort string) (fantasy.ProviderOptions, *float64)
	PrepareStep     fantasy.PrepareStepFunction
	DecorateTools   func(tools []fantasy.AgentTool)
	SupportsImages  func(modelID string) bool
	NativeWebSearch func(modelID string) fantasy.ProviderTool
	ContextWindow   func(modelID string) int64
	MaxOutputTokens func(modelID string) int64
	LoadSettings    func(config.Config) ProviderSettings
	SaveSettings    func(*config.Config, ProviderSettings)
}

// MissingAPIKeyError returns a descriptive error when an API key is missing.
func MissingAPIKeyError(envKey string, hint ...string) error {
	picker := "the model picker"
	if len(hint) > 0 && hint[0] != "" {
		picker = hint[0]
	}
	return fmt.Errorf("no API key configured — add one via %s, or export %s", picker, envKey)
}

// ResolveAPIProviderKey returns the API key to use from config or environment.
func ResolveAPIProviderKey(c config.APIProviderConfig, envKey string) string {
	if c.APIKey != "" {
		return c.APIKey
	}
	return os.Getenv(envKey)
}

func ResolveDeepSeekAPIKey(c config.APIProviderConfig) string {
	return ResolveAPIProviderKey(c, DeepSeekEnvAPIKey)
}

func ResolveKimiAPIKey(c config.APIProviderConfig) string {
	return ResolveAPIProviderKey(c, MoonshotEnvAPIKey)
}

func ResolveAnthropicAPIKey(c config.APIProviderConfig) string {
	return ResolveAPIProviderKey(c, AnthropicEnvAPIKey)
}

func ResolveOpenAIAPIKey(c config.APIProviderConfig) string {
	return ResolveAPIProviderKey(c, OpenAIEnvAPIKey)
}

func ResolveMiniMaxAPIKey(c config.APIProviderConfig) string {
	return ResolveAPIProviderKey(c, MiniMaxEnvAPIKey)
}

func ResolveGoogleAIAPIKey(c config.APIProviderConfig) string {
	return ResolveAPIProviderKey(c, GoogleAIEnvAPIKey)
}

func ResolveDeepSeekBaseURL(c config.APIProviderConfig) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DeepSeekDefaultBaseURL
}

func ResolveKimiBaseURL(c config.APIProviderConfig) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return MoonshotDefaultBaseURL
}

func ResolveMiniMaxBaseURL(c config.APIProviderConfig) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return MiniMaxDefaultBaseURL
}
