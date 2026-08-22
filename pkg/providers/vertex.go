package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
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
)

var VertexEffortOptions = GlobalEffortOptions

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

var VertexModelOptions = FilterVertexModelOptions(CatalogModelIDs("vertex"))

// VertexResolveProject: config value wins, then GOOGLE_CLOUD_PROJECT.
func VertexResolveProject(vc config.VertexConfig) string {
	if vc.Project != "" {
		return vc.Project
	}
	return os.Getenv(VertexEnvCloudProject)
}

// VertexResolveLocation: config value wins, then the default location.
func VertexResolveLocation(vc config.VertexConfig) string {
	if vc.Location != "" {
		return vc.Location
	}
	return VertexDefaultLocation
}

// VertexCredentialsLoader is the swappable loader for service account *auth.Credentials.
var VertexCredentialsLoader = func(path string) (*auth.Credentials, error) {
	return credentials.NewCredentialsFromFile(credentials.ServiceAccount, path, &credentials.DetectOptions{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
}

// VertexPrepareCredentials resolves SA key path, validates it is readable, and loads *auth.Credentials.
var VertexPrepareCredentials = func(vc config.VertexConfig) (*auth.Credentials, error) {
	saKeyPath := vc.ServiceAccountKey
	if saKeyPath == "" {
		saKeyPath = vc.ServiceAccount
	}
	if saKeyPath == "" {
		saKeyPath = os.Getenv(VertexEnvApplicationCredentials)
	}
	if saKeyPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(saKeyPath); err != nil {
		return nil, fmt.Errorf("vertex: read service account key %s: %w", saKeyPath, err)
	}
	return VertexCredentialsLoader(saKeyPath)
}

// VertexModel constructs a model.LLM backed by Vertex AI via ADK's gemini package.
// Swappable in tests.
var VertexModel = func(ctx context.Context, vc config.VertexConfig, modelID string) (model.LLM, error) {
	project := VertexResolveProject(vc)
	if project == "" {
		return nil, errors.New("vertex: project is required — set it in /config → Vertex AI, or via " + VertexEnvCloudProject)
	}
	location := VertexResolveLocation(vc)

	creds, err := VertexPrepareCredentials(vc)
	if err != nil {
		return nil, err
	}
	cfg := &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     project,
		Location:    location,
		Credentials: creds,
	}
	return gemini.NewModel(ctx, modelID, cfg)
}

// VertexNewClient constructs a genai.Client configured for Vertex AI.
// Swappable in tests.
var VertexNewClient = func(ctx context.Context, vc config.VertexConfig) (*genai.Client, error) {
	project := VertexResolveProject(vc)
	if project == "" {
		return nil, errors.New("vertex: project is required — set it in /config → Vertex AI, or via " + VertexEnvCloudProject)
	}
	location := VertexResolveLocation(vc)

	creds, err := VertexPrepareCredentials(vc)
	if err != nil {
		return nil, err
	}
	return genai.NewClient(ctx, &genai.ClientConfig{
		Backend:     genai.BackendVertexAI,
		Project:     project,
		Location:    location,
		Credentials: creds,
	})
}

// ListVertexModels queries the Vertex AI / Gemini API dynamically for available models.
var ListVertexModels = func(ctx context.Context, vc config.VertexConfig) ([]string, error) {
	client, err := VertexNewClient(ctx, vc)
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
	resolved := CatalogResolveEffort("vertex", modelID, effort)
	if resolved == "" {
		resolved = effort
	}
	clamped := CatalogClampEffort("vertex", modelID, resolved)
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

var VertexSpec = AgentProviderSpec{
	ID:               VertexProviderID,
	DisplayName:      "Vertex AI",
	DefaultModel:     VertexDefaultModel,
	ModelOptions:     VertexModelOptions,
	EffortOptions:    VertexEffortOptions,
	CanonicalModelID: CanonicalVertexModelID,
	BuildModel: func(ctx context.Context, cfg config.Config, modelID string) (model.LLM, error) {
		return VertexModel(ctx, cfg.Vertex, modelID)
	},
	BuildClient: func(cfg config.Config) (*genai.Client, error) {
		return VertexNewClient(context.Background(), cfg.Vertex)
	},
	CallOptions: VertexProviderOptions,
	SupportsImages: func(modelID string) bool {
		return CatalogSupportsImages("vertex", modelID, true)
	},
	ContextWindow: func(modelID string) int64 {
		return CatalogContextWindow("vertex", modelID, VertexContextWindow)
	},
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens("vertex", modelID, VertexFallbackMaxOutputTokens)
	},
	ListModels: func(ctx context.Context, cfg config.Config) ([]string, error) {
		return ListVertexModels(ctx, cfg.Vertex)
	},
	LoadSettings: func(cfg config.Config) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.Vertex.Model,
			Effort:        cfg.Effort,
			SlashCommands: cfg.Vertex.SlashCommands,
		}
	},
	SaveSettings: func(cfg *config.Config, s ProviderSettings) {
		cfg.Vertex.Model = s.Model
		cfg.Effort = s.Effort
		cfg.Vertex.SlashCommands = s.SlashCommands
	},
}
