package providers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	OpenRouterProviderID      = "openrouter"
	OpenRouterDefaultModel    = "anthropic/claude-3.7-sonnet"
	OpenRouterDefaultBaseURL  = "https://openrouter.ai/api/v1"
	OpenRouterEnvAPIKey       = "OPENROUTER_API_KEY"
)

var OpenRouterEffortOptions = GlobalEffortOptions

var OpenRouterModelOptions = CatalogModelIDs(OpenRouterProviderID)

func ResolveOpenRouterAPIKey(c config.APIProviderConfig) string {
	return ResolveAPIProviderKey(c, OpenRouterEnvAPIKey)
}

func ResolveOpenRouterBaseURL(c config.APIProviderConfig) string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return OpenRouterDefaultBaseURL
}

var ListOpenRouterModels = func(ctx context.Context, ac config.APIProviderConfig) ([]string, error) {
	baseURL := ResolveOpenRouterBaseURL(ac)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("HTTP-Referer", "https://github.com/Cidan/ask")
	req.Header.Set("X-Title", "ask")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var models []string
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func OpenRouterProviderOptions(modelID, effort string) (*genai.GenerateContentConfig, *float64) {
	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(CatalogDefaultMaxTokens("openrouter", modelID, 65536)),
	}
	// Effort is passed through in thinking config, handled by the OpenRouter adapter.
	return cfg, nil
}

var OpenRouterModelBuilder = func(ctx context.Context, ac config.APIProviderConfig, modelID string) (model.LLM, error) {
	apiKey := ResolveOpenRouterAPIKey(ac)
	if apiKey == "" {
		return nil, MissingAPIKeyError(OpenRouterEnvAPIKey)
	}
	return NewOpenRouterModel(modelID, apiKey, ResolveOpenRouterBaseURL(ac)), nil
}

var OpenRouterSpec = AgentProviderSpec{
	ID:            OpenRouterProviderID,
	DisplayName:   "OpenRouter",
	DefaultModel:  OpenRouterDefaultModel,
	ModelOptions:  OpenRouterModelOptions,
	EffortOptions: OpenRouterEffortOptions,
	CanonicalModelID: CanonicalOpenRouterModelID,
	BuildModel: func(ctx context.Context, cfg config.Config, modelID string) (model.LLM, error) {
		return OpenRouterModelBuilder(ctx, cfg.OpenRouter, modelID)
	},
	BuildClient: func(cfg config.Config) (*genai.Client, error) {
		// Not natively used as OpenRouter isn't supported by GenAI SDK for custom LLM yet?
		// ADK only uses BuildModel, BuildClient is mostly for Vertex fallback/utilities?
		return nil, nil
	},
	CallOptions: OpenRouterProviderOptions,
	SupportsImages: func(modelID string) bool {
		return CatalogSupportsImages("openrouter", modelID, true)
	},
	ContextWindow: func(modelID string) int64 {
		return CatalogContextWindow("openrouter", modelID, 200_000)
	},
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens("openrouter", modelID, 64_000)
	},
	LoadSettings: func(cfg config.Config) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.OpenRouter.Model,
			Effort:        cfg.Effort,
			SlashCommands: cfg.OpenRouter.SlashCommands,
		}
	},
	SaveSettings: func(cfg *config.Config, s ProviderSettings) {
		cfg.OpenRouter.Model = s.Model
		cfg.Effort = s.Effort
		cfg.OpenRouter.SlashCommands = s.SlashCommands
	},
}
