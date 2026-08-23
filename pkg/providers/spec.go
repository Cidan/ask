package providers

import (
	"context"
	"fmt"
	"os"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// ProviderSettings is the per-provider slice of configuration.
type ProviderSettings struct {
	Model         string                      `json:"model"`
	Effort        string                      `json:"effort"`
	SlashCommands []config.ProviderSlashEntry `json:"slashCommands,omitempty"`
}

// AgentProviderSpec describes one in-process API provider.
type AgentProviderSpec struct {
	ID               string
	DisplayName      string
	DefaultModel     string
	ModelOptions     []string
	EffortOptions    []string
	CanonicalModelID func(modelID string, fallback ...string) string
	BuildModel       func(ctx context.Context, cfg config.Config, modelID string) (model.LLM, error)
	BuildClient      func(cfg config.Config) (*genai.Client, error)
	CallOptions      func(modelID, effort string) (*genai.GenerateContentConfig, *float64)
	SupportsImages   func(modelID string) bool
	ContextWindow    func(modelID string) int64
	MaxOutputTokens  func(modelID string) int64
	// ListModels queries the provider for the ids it currently serves. nil
	// means the provider has no listing and the static catalog is all there is.
	ListModels   func(ctx context.Context, cfg config.Config) ([]string, error)
	LoadSettings func(config.Config) ProviderSettings
	SaveSettings func(*config.Config, ProviderSettings)
	// Configured reports whether the provider has the credentials it
	// needs (an API key, a project). nil means always configured. It
	// must not touch the network.
	Configured func(cfg config.Config) bool
}

// ProviderConfigured reports whether provider id can run with cfg —
// false for an unknown id or one missing its credentials.
func ProviderConfigured(cfg config.Config, id string) bool {
	spec, ok := GetAgentProviderSpec(id)
	if !ok || spec == nil {
		return false
	}
	if spec.Configured == nil {
		return true
	}
	return spec.Configured(cfg)
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
