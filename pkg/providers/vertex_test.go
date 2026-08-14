package providers

import (
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
