package providers

import (
	"testing"

	"github.com/Cidan/ask/pkg/config"
)

func TestResolveAPIProviderKey(t *testing.T) {
	cfg := config.APIProviderConfig{APIKey: "custom-key"}
	if got := ResolveAPIProviderKey(cfg, "SOME_ENV"); got != "custom-key" {
		t.Errorf("expected custom-key, got %s", got)
	}
	emptyCfg := config.APIProviderConfig{}
	t.Setenv("SOME_ENV", "env-key")
	if got := ResolveAPIProviderKey(emptyCfg, "SOME_ENV"); got != "env-key" {
		t.Errorf("expected env-key, got %s", got)
	}
}

func TestGetAgentProviderSpec(t *testing.T) {
	for _, id := range []string{"vertex", "openrouter"} {
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
	if len(all) != 2 {
		t.Errorf("expected 2 provider specs, got %d", len(all))
	}
}
