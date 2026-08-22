package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	OpenRouterProviderID     = "openrouter"
	OpenRouterDefaultModel   = "anthropic/claude-3.7-sonnet"
	OpenRouterDefaultBaseURL = "https://openrouter.ai/api/v1"
	OpenRouterEnvAPIKey      = "OPENROUTER_API_KEY"
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

// openRouterHeaders identify ask on OpenRouter's dashboards / rankings.
func openRouterHeaders() map[string]string {
	return map[string]string{
		"HTTP-Referer": "https://github.com/Cidan/ask",
		"X-Title":      "ask",
	}
}

// openRouterModelMeta is the slice of OpenRouter's /models metadata ask cares
// about. It is the authoritative source of a model's capabilities — the static
// catalog only seeds a handful of well-known ids for the picker.
type openRouterModelMeta struct {
	ID   string
	Name string
	// Description is OpenRouter's server-truncated text (most end in
	// "..."); ModelMetaFor uses it only when models.dev has none.
	Description         string
	Created             int64
	KnowledgeCutoff     string
	SupportsReasoning   bool
	SupportedEfforts    []string
	SupportsImages      bool
	InputModalities     []string
	ContextLength       int64
	MaxCompletionTokens int64
	Pricing             *ModelPricing
}

func (m openRouterModelMeta) modelMeta() ModelMeta {
	return ModelMeta{
		ID:              m.ID,
		Name:            m.Name,
		Description:     m.Description,
		ContextWindow:   m.ContextLength,
		MaxOutputTokens: m.MaxCompletionTokens,
		Pricing:         m.Pricing,
		InputModalities: m.InputModalities,
		Reasoning:       m.SupportsReasoning,
		ReasoningLevels: m.SupportedEfforts,
		KnowledgeCutoff: m.KnowledgeCutoff,
	}
}

var openRouterMeta = struct {
	mu      sync.RWMutex
	byID    map[string]openRouterModelMeta
	fetched bool
}{byID: map[string]openRouterModelMeta{}}

// fetchOpenRouterModels pulls the full model catalog (public, no auth) and
// projects it onto openRouterModelMeta.
func fetchOpenRouterModels(ctx context.Context, baseURL string) ([]openRouterModelMeta, error) {
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
			ID              string `json:"id"`
			Name            string `json:"name"`
			Description     string `json:"description"`
			Created         int64  `json:"created"`
			KnowledgeCutoff string `json:"knowledge_cutoff"`
			ContextLength   int64  `json:"context_length"`
			Architecture    struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
			Pricing struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
			TopProvider struct {
				MaxCompletionTokens int64 `json:"max_completion_tokens"`
			} `json:"top_provider"`
			SupportedParameters []string `json:"supported_parameters"`
			Reasoning           *struct {
				SupportedEfforts []string `json:"supported_efforts"`
			} `json:"reasoning"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	metas := make([]openRouterModelMeta, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID == "" {
			continue
		}
		meta := openRouterModelMeta{
			ID:                  m.ID,
			Name:                m.Name,
			Description:         m.Description,
			Created:             m.Created,
			KnowledgeCutoff:     m.KnowledgeCutoff,
			ContextLength:       m.ContextLength,
			MaxCompletionTokens: m.TopProvider.MaxCompletionTokens,
			InputModalities:     m.Architecture.InputModalities,
		}
		if in, okIn := perTokenToPer1M(m.Pricing.Prompt); okIn {
			out, _ := perTokenToPer1M(m.Pricing.Completion)
			cached, _ := perTokenToPer1M(m.Pricing.InputCacheRead)
			write, _ := perTokenToPer1M(m.Pricing.InputCacheWrite)
			meta.Pricing = &ModelPricing{
				InputPer1M:       in,
				OutputPer1M:      out,
				CachedInputPer1M: cached,
				CacheWritePer1M:  write,
			}
		}
		if m.Reasoning != nil {
			meta.SupportsReasoning = true
			meta.SupportedEfforts = m.Reasoning.SupportedEfforts
		}
		for _, p := range m.SupportedParameters {
			if p == "reasoning" || p == "reasoning_effort" {
				meta.SupportsReasoning = true
			}
		}
		for _, mod := range m.Architecture.InputModalities {
			if mod == "image" {
				meta.SupportsImages = true
			}
		}
		metas = append(metas, meta)
	}
	return metas, nil
}

func cacheOpenRouterMeta(metas []openRouterModelMeta) {
	openRouterMeta.mu.Lock()
	defer openRouterMeta.mu.Unlock()
	for _, m := range metas {
		openRouterMeta.byID[m.ID] = m
	}
	openRouterMeta.fetched = true
}

// cachedOpenRouterMeta is the in-memory-only lookup for callers that must not
// block on the network (the model picker); ok=false until a listing has landed.
func cachedOpenRouterMeta(modelID string) (openRouterModelMeta, bool) {
	openRouterMeta.mu.RLock()
	defer openRouterMeta.mu.RUnlock()
	m, ok := openRouterMeta.byID[modelID]
	return m, ok
}

// lookupOpenRouterMeta returns cached metadata for a model, fetching the full
// catalog once if it has never been loaded. A miss (unknown model, or a failed
// fetch) returns ok=false so callers fall back to provider-neutral behavior.
func lookupOpenRouterMeta(baseURL, modelID string) (openRouterModelMeta, bool) {
	openRouterMeta.mu.RLock()
	m, ok := openRouterMeta.byID[modelID]
	fetched := openRouterMeta.fetched
	openRouterMeta.mu.RUnlock()
	if ok {
		return m, true
	}
	if fetched {
		return openRouterModelMeta{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	metas, err := fetchOpenRouterModels(ctx, baseURL)
	if err != nil {
		// Mark fetched so a persistent failure doesn't re-fetch on every call;
		// capabilities stay unknown and callers degrade gracefully.
		openRouterMeta.mu.Lock()
		openRouterMeta.fetched = true
		openRouterMeta.mu.Unlock()
		return openRouterModelMeta{}, false
	}
	cacheOpenRouterMeta(metas)

	openRouterMeta.mu.RLock()
	m, ok = openRouterMeta.byID[modelID]
	openRouterMeta.mu.RUnlock()
	return m, ok
}

// openRouterReasoningEncoder maps ask's effort onto OpenRouter's unified
// `reasoning: {effort}` object, gated by the model's real capabilities: a model
// with no reasoning support gets nothing; an effort the model doesn't offer is
// clamped to the nearest one it does. Unknown models (catalog miss) pass the
// effort through — OpenRouter ignores it if unsupported.
func openRouterReasoningEncoder(baseURL string) func(*openai.ChatCompletionNewParams, string, string) {
	return func(params *openai.ChatCompletionNewParams, modelID, effort string) {
		effort = strings.ToLower(effort)
		if effort == "" {
			return
		}
		if meta, ok := lookupOpenRouterMeta(baseURL, modelID); ok {
			if !meta.SupportsReasoning {
				return
			}
			effort = clampEffortToSet(effort, meta.SupportedEfforts)
		}
		params.SetExtraFields(map[string]any{
			"reasoning": map[string]any{"effort": effort},
		})
	}
}

var ListOpenRouterModels = func(ctx context.Context, ac config.APIProviderConfig) ([]string, error) {
	metas, err := fetchOpenRouterModels(ctx, ResolveOpenRouterBaseURL(ac))
	if err != nil {
		return nil, err
	}
	cacheOpenRouterMeta(metas)
	ids := make([]string, 0, len(metas))
	for _, m := range metas {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// OpenRouterProviderOptions carries ask's effort intent through genai's
// ThinkingConfig and sets the output-token budget. The effort is passed
// verbatim; the per-model capability gate and clamp live in the reasoning
// encoder, which has OpenRouter's live model metadata.
func OpenRouterProviderOptions(modelID, effort string) (*genai.GenerateContentConfig, *float64) {
	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(CatalogDefaultMaxTokens(OpenRouterProviderID, modelID, 64_000)),
	}
	if effort != "" && effort != "off" {
		cfg.ThinkingConfig = &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingLevel:   genai.ThinkingLevel(strings.ToUpper(effort)),
		}
	}
	return cfg, nil
}

var OpenRouterModelBuilder = func(ctx context.Context, ac config.APIProviderConfig, modelID string) (model.LLM, error) {
	apiKey := ResolveOpenRouterAPIKey(ac)
	if apiKey == "" {
		return nil, MissingAPIKeyError(OpenRouterEnvAPIKey)
	}
	return NewOpenAICompatModel(OpenAICompatConfig{
		ModelID:         modelID,
		APIKey:          apiKey,
		BaseURL:         ResolveOpenRouterBaseURL(ac),
		Headers:         openRouterHeaders(),
		EncodeReasoning: openRouterReasoningEncoder(ResolveOpenRouterBaseURL(ac)),
	}), nil
}

// openRouterSupportsImages prefers OpenRouter's live model metadata, falling
// back to the static catalog (then true) for ids not present in a fetched list.
func openRouterSupportsImages(modelID string) bool {
	if meta, ok := lookupOpenRouterMeta(OpenRouterDefaultBaseURL, modelID); ok {
		return meta.SupportsImages
	}
	return CatalogSupportsImages(OpenRouterProviderID, modelID, true)
}

// openRouterContextWindow prefers OpenRouter's live model metadata, falling back
// to the static catalog (then 200k) for ids not present in a fetched list.
func openRouterContextWindow(modelID string) int64 {
	if meta, ok := lookupOpenRouterMeta(OpenRouterDefaultBaseURL, modelID); ok && meta.ContextLength > 0 {
		return meta.ContextLength
	}
	return CatalogContextWindow(OpenRouterProviderID, modelID, 200_000)
}

var OpenRouterSpec = AgentProviderSpec{
	ID:               OpenRouterProviderID,
	DisplayName:      "OpenRouter",
	DefaultModel:     OpenRouterDefaultModel,
	ModelOptions:     OpenRouterModelOptions,
	EffortOptions:    OpenRouterEffortOptions,
	CanonicalModelID: CanonicalOpenRouterModelID,
	BuildModel: func(ctx context.Context, cfg config.Config, modelID string) (model.LLM, error) {
		return OpenRouterModelBuilder(ctx, cfg.OpenRouter, modelID)
	},
	BuildClient: func(cfg config.Config) (*genai.Client, error) {
		// OpenRouter is reached through the OpenAI-compatible model.LLM
		// (BuildModel), not a genai.Client. The in-process runtime only
		// needs the client for genai-native providers (Vertex).
		return nil, nil
	},
	CallOptions:    OpenRouterProviderOptions,
	SupportsImages: openRouterSupportsImages,
	ContextWindow:  openRouterContextWindow,
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens(OpenRouterProviderID, modelID, 64_000)
	},
	ListModels: func(ctx context.Context, cfg config.Config) ([]string, error) {
		return ListOpenRouterModels(ctx, cfg.OpenRouter)
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
