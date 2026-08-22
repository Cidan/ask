package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Cidan/ask/pkg/config"
)

func TestResolveOpenRouterAPIKey(t *testing.T) {
	// Setup
	os.Clearenv()
	os.Setenv("OPENROUTER_API_KEY", "env-key")
	
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("Expected /models, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data": [{"id": "model1"}, {"id": "model2"}]}`))
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
}

func TestOpenRouterModelBuilder(t *testing.T) {
	os.Clearenv()
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
