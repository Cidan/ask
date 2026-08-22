package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cidan/ask/pkg/config"
)

func TestResolveOpenRouterAPIKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "env-key")

	// Test env var
	cfg := config.APIProviderConfig{}
	key := ResolveOpenRouterAPIKey(cfg)
	if key != "env-key" {
		t.Errorf("Expected env-key, got %s", key)
	}

	// Test config takes precedence
	cfg.APIKey = "cfg-key"
	key = ResolveOpenRouterAPIKey(cfg)
	if key != "cfg-key" {
		t.Errorf("Expected cfg-key, got %s", key)
	}
}

func TestResolveOpenRouterBaseURL(t *testing.T) {
	cfg := config.APIProviderConfig{}
	url := ResolveOpenRouterBaseURL(cfg)
	if url != OpenRouterDefaultBaseURL {
		t.Errorf("Expected default url %s, got %s", OpenRouterDefaultBaseURL, url)
	}

	cfg.BaseURL = "http://custom-url"
	url = ResolveOpenRouterBaseURL(cfg)
	if url != "http://custom-url" {
		t.Errorf("Expected custom url, got %s", url)
	}
}

func TestListOpenRouterModels(t *testing.T) {
	resetOpenRouterMeta(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("Expected /models, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data": [
			{"id": "model1", "name": "Vendor: Model One", "description": "First model", "created": 1750172488,
			 "knowledge_cutoff": "2025-06-01", "context_length": 1048576,
			 "architecture": {"input_modalities": ["text", "image"]},
			 "pricing": {"prompt": "0.0000003", "completion": "0.0000025", "input_cache_read": "0.00000003", "input_cache_write": "0.0000000833"},
			 "top_provider": {"context_length": 1048576, "max_completion_tokens": 65535},
			 "supported_parameters": ["reasoning", "tools"],
			 "reasoning": {"supported_efforts": ["low", "medium", "high"]}},
			{"id": "model2", "pricing": {"prompt": "-1", "completion": "-1"}, "top_provider": {"max_completion_tokens": null}}
		]}`))
	}))
	defer server.Close()

	cfg := config.APIProviderConfig{BaseURL: server.URL}
	models, err := ListOpenRouterModels(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(models) != 2 || models[0] != "model1" || models[1] != "model2" {
		t.Errorf("Unexpected models: %v", models)
	}

	m, ok := cachedOpenRouterMeta("model1")
	if !ok {
		t.Fatal("listing must populate the in-memory meta cache")
	}
	if m.Name != "Vendor: Model One" || m.Description != "First model" || m.Created != 1750172488 || m.KnowledgeCutoff != "2025-06-01" {
		t.Errorf("identity fields wrong: %+v", m)
	}
	if m.ContextLength != 1_048_576 || m.MaxCompletionTokens != 65_535 {
		t.Errorf("limits wrong: %+v", m)
	}
	if m.Pricing == nil {
		t.Fatal("pricing must be projected to per-1M")
	}
	near := func(got, want float64) bool { return got > want-1e-9 && got < want+1e-9 }
	if !near(m.Pricing.InputPer1M, 0.3) || !near(m.Pricing.OutputPer1M, 2.5) || !near(m.Pricing.CachedInputPer1M, 0.03) || !near(m.Pricing.CacheWritePer1M, 0.0833) {
		t.Errorf("per-1M pricing wrong: %+v", m.Pricing)
	}
	if !m.SupportsReasoning || len(m.SupportedEfforts) != 3 || !m.SupportsImages || len(m.InputModalities) != 2 {
		t.Errorf("capabilities wrong: %+v", m)
	}

	m2, ok := cachedOpenRouterMeta("model2")
	if !ok {
		t.Fatal("sparse entries must still be cached")
	}
	if m2.Pricing != nil || m2.MaxCompletionTokens != 0 || m2.Name != "" {
		t.Errorf("dynamic pricing (-1) and null fields must stay unknown: %+v", m2)
	}
	if _, ok := cachedOpenRouterMeta("model3"); ok {
		t.Error("unlisted model must miss without a fetch")
	}
}

func TestOpenRouterModelBuilder(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	cfg := config.APIProviderConfig{}
	_, err := OpenRouterModelBuilder(context.Background(), cfg, "model")
	if err == nil {
		t.Errorf("Expected error for missing API key")
	}

	cfg.APIKey = "key"
	m, err := OpenRouterModelBuilder(context.Background(), cfg, "model")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if m == nil {
		t.Errorf("Expected non-nil model")
	}
}

func TestOpenRouterSpec(t *testing.T) {
	if OpenRouterSpec.ID != OpenRouterProviderID {
		t.Errorf("Expected ID %s, got %s", OpenRouterProviderID, OpenRouterSpec.ID)
	}
	cfg := config.Config{}
	cfg.OpenRouter.Model = "cfg-model"
	cfg.Effort = "low"
	cfg.OpenRouter.SlashCommands = []config.ProviderSlashEntry{{Name: "test", Description: "test cmd"}}

	s := OpenRouterSpec.LoadSettings(cfg)
	if s.Model != "cfg-model" || s.Effort != "low" || len(s.SlashCommands) != 1 {
		t.Errorf("Unexpected settings: %+v", s)
	}

	s.Model = "new-model"
	OpenRouterSpec.SaveSettings(&cfg, s)
	if cfg.OpenRouter.Model != "new-model" {
		t.Errorf("Expected new-model, got %s", cfg.OpenRouter.Model)
	}
}
