package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cidan/ask/pkg/config"
)

func TestFilterVertexModelOptions(t *testing.T) {
	in := []string{"gemini-2.5-pro", "claude-sonnet-4-6", "claude-opus-4-5", "gemini-3.1-pro-preview"}
	out := FilterVertexModelOptions(in)
	if len(out) != 2 || out[0] != "gemini-2.5-pro" || out[1] != "gemini-3.1-pro-preview" {
		t.Errorf("filter failed: got %v", out)
	}
}

func TestVertexResolveProjectAndLocation(t *testing.T) {
	vc := config.VertexConfig{Project: "my-proj", Location: "europe-west1"}
	if got := VertexResolveProject(vc); got != "my-proj" {
		t.Errorf("project wrong: %s", got)
	}
	if got := VertexResolveLocation(vc); got != "europe-west1" {
		t.Errorf("location wrong: %s", got)
	}

	vcEmpty := config.VertexConfig{}
	t.Setenv(VertexEnvCloudProject, "env-proj")
	if got := VertexResolveProject(vcEmpty); got != "env-proj" {
		t.Errorf("project env fallback wrong: %s", got)
	}
	if got := VertexResolveLocation(vcEmpty); got != VertexDefaultLocation {
		t.Errorf("location default wrong: %s", got)
	}
}

func TestVertexPrepareCredentials(t *testing.T) {
	tmp := t.TempDir()
	keyFile := filepath.Join(tmp, "key.json")
	if err := os.WriteFile(keyFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	var applied string
	prev := VertexApplyEnv
	VertexApplyEnv = func(path string) { applied = path }
	defer func() { VertexApplyEnv = prev }()

	vc := config.VertexConfig{ServiceAccountKey: keyFile}
	path, err := VertexPrepareCredentials(vc)
	if err != nil || path != keyFile || applied != keyFile {
		t.Fatalf("credentials prep failed: path=%s applied=%s err=%v", path, applied, err)
	}
}

func TestVertexSpec_Properties(t *testing.T) {
	if VertexSpec.ID != "vertex" || VertexSpec.DisplayName != "Vertex AI" {
		t.Errorf("identity wrong: %+v", VertexSpec)
	}
	if len(VertexSpec.ModelOptions) == 0 {
		t.Error("expected model options")
	}
	for _, m := range VertexSpec.ModelOptions {
		if m == "claude-sonnet-4-6" {
			t.Error("Vertex models must not include claude")
		}
	}
	if VertexSpec.ContextWindow("gemini-3.1-pro-preview") != 1_048_576 {
		t.Errorf("context window wrong: %d", VertexSpec.ContextWindow("gemini-3.1-pro-preview"))
	}
	if got := VertexSpec.MaxOutputTokens("gemini-3.7-flash"); got != MaxOutputTokensGemini {
		t.Errorf("gemini-3.7-flash max tokens wrong: %d want %d", got, MaxOutputTokensGemini)
	}

	cfg := config.Config{}
	cfg.Vertex.Model = "gemini-vertex"
	cfg.Effort = "medium"
	settings := VertexSpec.LoadSettings(cfg)
	if settings.Model != "gemini-vertex" || settings.Effort != "medium" {
		t.Errorf("settings mismatch: %+v", settings)
	}
	var newCfg config.Config
	VertexSpec.SaveSettings(&newCfg, settings)
	if newCfg.Vertex.Model != "gemini-vertex" || newCfg.Effort != "medium" {
		t.Errorf("save settings mismatch: %+v", newCfg)
	}
}

func TestVertexProviderOptions(t *testing.T) {
	cfg, temp := VertexProviderOptions("gemini-3.7-flash", "high")
	if cfg == nil || cfg.ThinkingConfig == nil {
		t.Fatal("expected thinking config for high effort")
	}
	if !cfg.ThinkingConfig.IncludeThoughts {
		t.Error("expected IncludeThoughts to be true")
	}
	if cfg.MaxOutputTokens != int32(MaxOutputTokensGemini) {
		t.Errorf("expected MaxOutputTokens=%d, got %d", MaxOutputTokensGemini, cfg.MaxOutputTokens)
	}
	if temp != nil {
		t.Errorf("expected nil temp, got %v", temp)
	}

	cfgOff, _ := VertexProviderOptions("gemini-3.7-flash", "off")
	if cfgOff == nil {
		t.Fatal("expected non-nil config for off effort")
	}
	if cfgOff.ThinkingConfig != nil {
		t.Errorf("expected nil thinking config for off effort, got %+v", cfgOff.ThinkingConfig)
	}
	if cfgOff.MaxOutputTokens != int32(MaxOutputTokensGemini) {
		t.Errorf("expected MaxOutputTokens=%d for off effort, got %d", MaxOutputTokensGemini, cfgOff.MaxOutputTokens)
	}
}

func TestListVertexModels_Swapped(t *testing.T) {
	prev := ListVertexModels
	defer func() { ListVertexModels = prev }()

	ListVertexModels = func(ctx context.Context, vc config.VertexConfig) ([]string, error) {
		return []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-3.1-pro-preview"}, nil
	}

	models, err := ListVertexModels(context.Background(), config.VertexConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 3 || models[0] != "gemini-2.5-pro" {
		t.Errorf("unexpected models returned: %v", models)
	}
}
