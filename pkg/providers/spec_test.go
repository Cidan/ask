package providers

import (
	"testing"

	"github.com/Cidan/ask/pkg/config"
)

func TestResolveAPIKeysAndBaseURLs(t *testing.T) {
	cfg := config.APIProviderConfig{APIKey: "custom-key", BaseURL: "https://custom.api/v1"}
	if got := ResolveAnthropicAPIKey(cfg); got != "custom-key" {
		t.Errorf("expected custom-key, got %s", got)
	}
	if got := ResolveOpenAIAPIKey(cfg); got != "custom-key" {
		t.Errorf("expected custom-key, got %s", got)
	}
	if got := ResolveDeepSeekAPIKey(cfg); got != "custom-key" {
		t.Errorf("expected custom-key, got %s", got)
	}
	if got := ResolveGoogleAIAPIKey(cfg); got != "custom-key" {
		t.Errorf("expected custom-key, got %s", got)
	}
	if got := ResolveMiniMaxAPIKey(cfg); got != "custom-key" {
		t.Errorf("expected custom-key, got %s", got)
	}
	if got := ResolveKimiAPIKey(cfg); got != "custom-key" {
		t.Errorf("expected custom-key, got %s", got)
	}

	if got := ResolveDeepSeekBaseURL(cfg); got != "https://custom.api/v1" {
		t.Errorf("expected custom base url, got %s", got)
	}
	if got := ResolveKimiBaseURL(cfg); got != "https://custom.api/v1" {
		t.Errorf("expected custom base url, got %s", got)
	}
	if got := ResolveMiniMaxBaseURL(cfg); got != "https://custom.api/v1" {
		t.Errorf("expected custom base url, got %s", got)
	}

	emptyCfg := config.APIProviderConfig{}
	if got := ResolveDeepSeekBaseURL(emptyCfg); got != DeepSeekDefaultBaseURL {
		t.Errorf("expected default base url, got %s", got)
	}
	if got := ResolveKimiBaseURL(emptyCfg); got != MoonshotDefaultBaseURL {
		t.Errorf("expected default base url, got %s", got)
	}
	if got := ResolveMiniMaxBaseURL(emptyCfg); got != MiniMaxDefaultBaseURL {
		t.Errorf("expected default base url, got %s", got)
	}
}

func TestGetAgentProviderSpec(t *testing.T) {
	for _, id := range []string{"anthropic", "openai", "deepseek", "googleai", "vertex", "kimi", "minimax"} {
		spec, ok := GetAgentProviderSpec(id)
		if !ok || spec == nil {
			t.Fatalf("expected spec for %q", id)
		}
		if spec.ID != id {
			t.Errorf("spec id mismatch: got %q want %q", spec.ID, id)
		}
		if spec.DisplayName == "" {
			t.Errorf("spec %q missing display name", id)
		}
	}

	all := AllAgentProviderSpecs()
	if len(all) != 7 {
		t.Errorf("expected 7 provider specs, got %d", len(all))
	}
}
