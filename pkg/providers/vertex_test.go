package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/auth"
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
	vc := config.ProviderConfig{}.WithField(VertexFieldProject, "my-proj").WithField(VertexFieldLocation, "europe-west1")
	if got := VertexResolveProject(vc); got != "my-proj" {
		t.Errorf("project wrong: %s", got)
	}
	if got := VertexResolveLocation(vc); got != "europe-west1" {
		t.Errorf("location wrong: %s", got)
	}

	vcEmpty := config.ProviderConfig{}
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

	fakeCreds := &auth.Credentials{}
	var loadedPath string
	prevLoader := VertexCredentialsLoader
	VertexCredentialsLoader = func(path string) (*auth.Credentials, error) {
		loadedPath = path
		return fakeCreds, nil
	}
	defer func() { VertexCredentialsLoader = prevLoader }()

	// Ensure GOOGLE_APPLICATION_CREDENTIALS is not set initially
	t.Setenv(VertexEnvApplicationCredentials, "")

	vc := config.ProviderConfig{}.WithField(VertexFieldServiceAccountKey, keyFile)
	creds, err := VertexPrepareCredentials(vc)
	if err != nil {
		t.Fatalf("credentials prep failed: %v", err)
	}
	if creds != fakeCreds {
		t.Errorf("expected returned creds to match loader output")
	}
	if loadedPath != keyFile {
		t.Errorf("expected loadedPath=%s, got %s", keyFile, loadedPath)
	}

	// Verify env was NOT mutated
	if envVal := os.Getenv(VertexEnvApplicationCredentials); envVal != "" {
		t.Errorf("VertexPrepareCredentials must NOT mutate %s; got %q", VertexEnvApplicationCredentials, envVal)
	}

	// Test missing file returns error
	vcMissing := config.ProviderConfig{}.WithField(VertexFieldServiceAccountKey, filepath.Join(tmp, "nonexistent.json"))
	if _, err := VertexPrepareCredentials(vcMissing); err == nil {
		t.Error("expected error for non-existent service account key")
	}

	// Test empty SA returns nil credentials without error
	vcEmpty := config.ProviderConfig{}
	emptyCreds, err := VertexPrepareCredentials(vcEmpty)
	if err != nil {
		t.Fatalf("unexpected error for empty config: %v", err)
	}
	if emptyCreds != nil {
		t.Errorf("expected nil creds for empty config, got %v", emptyCreds)
	}

	// A tilde path is expanded before the file is read.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "sa.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	loadedPath = ""
	if _, err := VertexPrepareCredentials(config.ProviderConfig{}.WithField(VertexFieldServiceAccountKey, "~/sa.json")); err != nil {
		t.Fatalf("tilde path: %v", err)
	}
	if loadedPath != filepath.Join(home, "sa.json") {
		t.Errorf("tilde path must be expanded before loading, got %q", loadedPath)
	}

	// The env fallback is honoured when config has no key.
	t.Setenv(VertexEnvApplicationCredentials, filepath.Join(home, "sa.json"))
	loadedPath = ""
	if _, err := VertexPrepareCredentials(config.ProviderConfig{}); err != nil {
		t.Fatalf("env path: %v", err)
	}
	if loadedPath != filepath.Join(home, "sa.json") {
		t.Errorf("env fallback must load the env path, got %q", loadedPath)
	}
}

func TestVertex_Provider(t *testing.T) {
	var p Provider = Vertex{}
	if p.ID() != "vertex" || p.DisplayName() != "Vertex AI" || p.DefaultModel() != VertexDefaultModel {
		t.Errorf("identity wrong: %s %s %s", p.ID(), p.DisplayName(), p.DefaultModel())
	}
	if len(p.ModelOptions()) == 0 {
		t.Error("expected model options")
	}
	for _, m := range p.ModelOptions() {
		if m == "claude-sonnet-4-6" {
			t.Error("Vertex models must not include claude")
		}
	}
	if p.ContextWindow("gemini-3.1-pro-preview") != 1_048_576 {
		t.Errorf("context window wrong: %d", p.ContextWindow("gemini-3.1-pro-preview"))
	}
	if got := p.MaxOutputTokens("gemini-3.7-flash"); got != MaxOutputTokensGemini {
		t.Errorf("gemini-3.7-flash max tokens wrong: %d want %d", got, MaxOutputTokensGemini)
	}
	if !p.SupportsImages("gemini-3.7-flash") {
		t.Error("gemini takes images")
	}
	if got := p.CanonicalModelID("claude-3-7-sonnet", ""); got != VertexDefaultModel {
		t.Errorf("foreign id must fall back to the default: %q", got)
	}
	if got := p.CanonicalModelID("", "gemini-2.5-pro"); got != "gemini-2.5-pro" {
		t.Errorf("explicit fallback: %q", got)
	}
	keys := make([]string, 0, 3)
	for _, f := range p.Settings() {
		keys = append(keys, f.Key)
	}
	if len(keys) != 3 || keys[0] != VertexFieldProject || keys[1] != VertexFieldLocation || keys[2] != VertexFieldServiceAccountKey {
		t.Errorf("settings order: %v", keys)
	}
	t.Setenv(VertexEnvCloudProject, "")
	if p.Configured(config.ProviderConfig{}) {
		t.Error("no project → not configured")
	}
	if !p.Configured(config.ProviderConfig{}.WithField(VertexFieldProject, "proj")) {
		t.Error("project set → configured")
	}
	if _, err := p.BuildModel(context.Background(), config.ProviderConfig{}, VertexDefaultModel); err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Errorf("BuildModel without a project must fail fast, got %v", err)
	}
}

func TestVertexValidators(t *testing.T) {
	for _, good := range []string{"myproj", "my-proj", "abc123", "a12345"} {
		if err := ValidateVertexProject(good); err != nil {
			t.Errorf("%q must validate: %v", good, err)
		}
	}
	for _, bad := range []string{"", "abc", "ABCdef", "1abcde", "a-very-long-project-name-that-is-too-long"} {
		if err := ValidateVertexProject(bad); err == nil {
			t.Errorf("%q must fail validation", bad)
		}
	}
	for _, good := range []string{"global", "us-central1", "europe-west4"} {
		if err := ValidateVertexLocation(good); err != nil {
			t.Errorf("%q must validate: %v", good, err)
		}
	}
	for _, bad := range []string{"", "blah", "US-central1"} {
		if err := ValidateVertexLocation(bad); err == nil {
			t.Errorf("%q must fail validation", bad)
		}
	}

	if err := ValidateVertexServiceAccountKey(""); err != nil {
		t.Errorf("empty SA key must validate (ADC): %v", err)
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(real, []byte(`{"type":"service_account"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVertexServiceAccountKey(real); err != nil {
		t.Errorf("real path must validate: %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "keys", "sa.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVertexServiceAccountKey("~/keys/sa.json"); err != nil {
		t.Errorf("tilde path must validate: %v", err)
	}
	if err := ValidateVertexServiceAccountKey("/nonexistent/path/sa.json"); err == nil {
		t.Error("missing file must fail")
	}
	if err := ValidateVertexServiceAccountKey(dir); err == nil {
		t.Error("directory must fail")
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

	ListVertexModels = func(ctx context.Context, pc config.ProviderConfig) ([]string, error) {
		return []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-3.1-pro-preview"}, nil
	}

	var lister ModelLister = Vertex{}
	models, err := lister.ListModels(context.Background(), config.ProviderConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 3 || models[0] != "gemini-2.5-pro" {
		t.Errorf("unexpected models returned: %v", models)
	}
}
