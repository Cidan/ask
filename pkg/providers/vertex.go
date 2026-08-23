package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"
)

const (
	VertexProviderID              = "vertex"
	VertexDefaultModel            = "gemini-3.7-flash"
	VertexDefaultLocation         = "global"
	VertexContextWindow           = 1_048_576
	VertexFallbackMaxOutputTokens = 65_536

	VertexEnvApplicationCredentials = "GOOGLE_APPLICATION_CREDENTIALS"
	VertexEnvCloudProject           = "GOOGLE_CLOUD_PROJECT"

	VertexFieldProject           = "project"
	VertexFieldLocation          = "location"
	VertexFieldServiceAccountKey = "serviceAccountKey"
)

var VertexEffortOptions = GlobalEffortOptions

// Vertex is the Vertex AI provider: Gemini through ADK's gemini model,
// authenticated with Google Cloud ADC or an explicit service-account key.
type Vertex struct{}

var vertexSettings = []SettingField{
	{
		Key:      VertexFieldProject,
		Title:    "Project",
		Hint:     "Google Cloud project id; enter to save",
		EnvKey:   VertexEnvCloudProject,
		Validate: ValidateVertexProject,
	},
	{
		Key:      VertexFieldLocation,
		Title:    "Location",
		Hint:     "Vertex region (e.g. us-central1) or 'global'; enter to save",
		Default:  VertexDefaultLocation,
		Validate: ValidateVertexLocation,
	},
	{
		Key:      VertexFieldServiceAccountKey,
		Title:    "Service Account Key",
		Hint:     "path to a service-account JSON; blank to use ADC; enter to save",
		EnvKey:   VertexEnvApplicationCredentials,
		Validate: ValidateVertexServiceAccountKey,
	},
}

func (Vertex) ID() string              { return VertexProviderID }
func (Vertex) DisplayName() string     { return "Vertex AI" }
func (Vertex) DefaultModel() string    { return VertexDefaultModel }
func (Vertex) ModelOptions() []string  { return VertexModelOptions }
func (Vertex) EffortOptions() []string { return VertexEffortOptions }
func (Vertex) Settings() []SettingField {
	return append([]SettingField(nil), vertexSettings...)
}

func (Vertex) Configured(pc config.ProviderConfig) bool {
	return VertexResolveProject(pc) != ""
}

func (Vertex) BuildModel(ctx context.Context, pc config.ProviderConfig, modelID string) (model.LLM, error) {
	return VertexModel(ctx, pc, modelID)
}

func (Vertex) CanonicalModelID(modelID, fallback string) string {
	return CanonicalVertexModelID(modelID, fallback)
}

func (Vertex) CallOptions(modelID, effort string) (*genai.GenerateContentConfig, *float64) {
	return VertexProviderOptions(modelID, effort)
}

func (Vertex) SupportsImages(modelID string) bool {
	return CatalogSupportsImages(VertexProviderID, modelID, true)
}

func (Vertex) ContextWindow(modelID string) int64 {
	return CatalogContextWindow(VertexProviderID, modelID, VertexContextWindow)
}

func (Vertex) MaxOutputTokens(modelID string) int64 {
	return CatalogDefaultMaxTokens(VertexProviderID, modelID, VertexFallbackMaxOutputTokens)
}

func (Vertex) ListModels(ctx context.Context, pc config.ProviderConfig) ([]string, error) {
	return ListVertexModels(ctx, pc)
}

var (
	_ Provider    = Vertex{}
	_ ModelLister = Vertex{}
)

// FilterVertexModelOptions strips Claude / Anthropic ids from the Vertex model list.
func FilterVertexModelOptions(all []string) []string {
	out := make([]string, 0, len(all))
	for _, id := range all {
		low := strings.ToLower(id)
		if strings.Contains(low, "claude") || strings.Contains(low, "anthropic") {
			continue
		}
		out = append(out, id)
	}
	return out
}

var VertexModelOptions = FilterVertexModelOptions(CatalogModelIDs(VertexProviderID))

// VertexResolveProject: config value wins, then GOOGLE_CLOUD_PROJECT.
func VertexResolveProject(pc config.ProviderConfig) string {
	return SettingValue(pc, vertexSettings[0])
}

// VertexResolveLocation: config value wins, then the default location.
func VertexResolveLocation(pc config.ProviderConfig) string {
	return SettingValue(pc, vertexSettings[1])
}

// VertexResolveServiceAccountKey: config value wins, then
// GOOGLE_APPLICATION_CREDENTIALS; "" means ADC. The path is
// tilde-expanded so a saved "~/keys/vertex.json" works.
func VertexResolveServiceAccountKey(pc config.ProviderConfig) string {
	return expandTilde(SettingValue(pc, vertexSettings[2]))
}

// vertexProjectIDPattern matches a valid GCP project id: starts with a
// lowercase letter, followed by 5-29 lowercase letters, digits, or
// hyphens.
var vertexProjectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{5,29}$`)

// ValidateVertexProject screens a project id draft. Empty is invalid:
// the session would fail at start with "project is required", so the
// field visibly requires a value instead.
func ValidateVertexProject(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("project is required")
	}
	if len(t) < 6 || len(t) > 30 {
		return errors.New("project id must be 6-30 characters")
	}
	if !vertexProjectIDPattern.MatchString(t) {
		return errors.New("project id must start with a lowercase letter and contain only lowercase letters, digits, or hyphens")
	}
	return nil
}

var vertexLocationPattern = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]$`)

// ValidateVertexLocation accepts the literal "global" or a region id in
// the canonical GCP shape ("us-central1", "europe-west4").
func ValidateVertexLocation(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return errors.New("location is required")
	}
	if t == VertexDefaultLocation {
		return nil
	}
	if !vertexLocationPattern.MatchString(t) {
		return errors.New("location must be 'global' or match shape like 'us-central1'")
	}
	return nil
}

// ValidateVertexServiceAccountKey accepts an empty string (ADC) or the
// path of a readable file, tilde-expanded.
func ValidateVertexServiceAccountKey(s string) error {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil
	}
	expanded := expandTilde(t)
	info, err := os.Stat(expanded)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", expanded, err)
	}
	if info.IsDir() {
		return errors.New("path is a directory, expected a JSON file")
	}
	return nil
}

// VertexCredentialsLoader is the swappable loader for service account *auth.Credentials.
var VertexCredentialsLoader = func(path string) (*auth.Credentials, error) {
	return credentials.NewCredentialsFromFile(credentials.ServiceAccount, path, &credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
}

// VertexPrepareCredentials resolves the SA key path, validates it is
// readable, and loads *auth.Credentials; nil credentials mean ADC.
var VertexPrepareCredentials = func(pc config.ProviderConfig) (*auth.Credentials, error) {
	saKeyPath := VertexResolveServiceAccountKey(pc)
	if saKeyPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(saKeyPath); err != nil {
		return nil, fmt.Errorf("vertex: read service account key %s: %w", saKeyPath, err)
	}
	return VertexCredentialsLoader(saKeyPath)
}

func vertexClientConfig(pc config.ProviderConfig) (*genai.ClientConfig, error) {
	project := VertexResolveProject(pc)
	if project == "" {
		return nil, errors.New("vertex: project is required — set it in /config → Vertex AI, or via " + VertexEnvCloudProject)
	}
	creds, err := VertexPrepareCredentials(pc)
	if err != nil {
		return nil, err
	}
	return &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     project,
		Location:    VertexResolveLocation(pc),
		Credentials: creds,
	}, nil
}

// VertexModel constructs a model.LLM backed by Vertex AI via ADK's gemini package.
// Swappable in tests.
var VertexModel = func(ctx context.Context, pc config.ProviderConfig, modelID string) (model.LLM, error) {
	cfg, err := vertexClientConfig(pc)
	if err != nil {
		return nil, err
	}
	return gemini.NewModel(ctx, modelID, cfg)
}

// VertexNewClient constructs a genai.Client configured for Vertex AI —
// the model listing needs the raw client. Swappable in tests.
var VertexNewClient = func(ctx context.Context, pc config.ProviderConfig) (*genai.Client, error) {
	cfg, err := vertexClientConfig(pc)
	if err != nil {
		return nil, err
	}
	return genai.NewClient(ctx, cfg)
}

// ListVertexModels queries the Vertex AI / Gemini API dynamically for available models.
var ListVertexModels = func(ctx context.Context, pc config.ProviderConfig) ([]string, error) {
	client, err := VertexNewClient(ctx, pc)
	if err != nil {
		return nil, err
	}
	var models []string
	page, err := client.Models.List(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, m := range page.Items {
		name := m.Name
		name = strings.TrimPrefix(name, "publishers/google/models/")
		name = strings.TrimPrefix(name, "models/")
		if strings.Contains(strings.ToLower(name), "claude") || strings.Contains(strings.ToLower(name), "anthropic") {
			continue
		}
		if name != "" {
			models = append(models, name)
		}
	}
	return models, nil
}

// VertexProviderOptions translates ask's effort picker onto Gemini's thinking controls and configures max tokens.
func VertexProviderOptions(modelID, effort string) (*genai.GenerateContentConfig, *float64) {
	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(MaxOutputTokensGemini),
	}
	if effort == "" || effort == "off" {
		return cfg, nil
	}
	resolved := CatalogResolveEffort(VertexProviderID, modelID, effort)
	if resolved == "" {
		resolved = effort
	}
	clamped := CatalogClampEffort(VertexProviderID, modelID, resolved)
	if clamped == "" {
		clamped = resolved
	}
	if clamped != "" && clamped != "off" {
		level := genai.ThinkingLevel(strings.ToUpper(clamped))
		cfg.ThinkingConfig = &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingLevel:   level,
		}
	}
	return cfg, nil
}
