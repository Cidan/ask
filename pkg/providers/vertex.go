package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"
)

const (
	VertexProviderID              = "vertex"
	VertexDefaultModel            = "gemini-3.1-pro-preview"
	VertexDefaultLocation         = "global"
	VertexContextWindow           = 1_048_576
	VertexFallbackMaxOutputTokens = 32_000

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

var VertexModelOptions = FilterVertexModelOptions(CatalogModelIDs(catwalk.InferenceProviderVertexAI))

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

// VertexApplyEnv is the testable seam for the SA-key env-mutation strategy.
var VertexApplyEnv = func(path string) {
	_ = os.Setenv(VertexEnvApplicationCredentials, path)
}

// VertexPrepareCredentials resolves SA key path, validates it is readable, and applies env.
var VertexPrepareCredentials = func(vc config.VertexConfig) (string, error) {
	saKeyPath := vc.ServiceAccountKey
	if saKeyPath == "" {
		saKeyPath = vc.ServiceAccount
	}
	if saKeyPath == "" {
		saKeyPath = os.Getenv(VertexEnvApplicationCredentials)
	}
	if saKeyPath == "" {
		return "", nil
	}
	if _, err := os.Stat(saKeyPath); err != nil {
		return "", fmt.Errorf("vertex: read service account key %s: %w", saKeyPath, err)
	}
	VertexApplyEnv(saKeyPath)
	return saKeyPath, nil
}

// VertexModel constructs a model.LLM backed by Vertex AI via ADK's gemini package.
// Swappable in tests.
var VertexModel = func(ctx context.Context, vc config.VertexConfig, modelID string) (model.LLM, error) {
	project := VertexResolveProject(vc)
	if project == "" {
		return nil, errors.New("vertex: project is required — set it in /config → Vertex AI, or via " + VertexEnvCloudProject)
	}
	location := VertexResolveLocation(vc)

	if _, err := VertexPrepareCredentials(vc); err != nil {
		return nil, err
	}
	cfg := &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  project,
		Location: location,
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

	if _, err := VertexPrepareCredentials(vc); err != nil {
		return nil, err
	}
	return genai.NewClient(ctx, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  project,
		Location: location,
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

// VertexProviderOptions translates ask's effort picker onto Gemini's thinking controls.
func VertexProviderOptions(modelID, effort string) (*genai.GenerateContentConfig, *float64) {
	if effort == "" || effort == "off" {
		return nil, nil
	}
	resolved := CatalogResolveEffort(catwalk.InferenceProviderVertexAI, modelID, effort)
	if resolved == "" {
		resolved = effort
	}
	clamped := CatalogClampEffort(catwalk.InferenceProviderVertexAI, modelID, resolved)
	if clamped == "" {
		clamped = resolved
	}
	if clamped == "" || clamped == "off" {
		return nil, nil
	}
	level := genai.ThinkingLevel(strings.ToUpper(clamped))
	cfg := &genai.GenerateContentConfig{
		ThinkingConfig: &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingLevel:   level,
		},
	}
	return cfg, nil
}

var VertexSpec = AgentProviderSpec{
	ID:            VertexProviderID,
	DisplayName:   "Vertex AI",
	DefaultModel:  VertexDefaultModel,
	ModelOptions:  VertexModelOptions,
	EffortOptions: VertexEffortOptions,
	BuildModel: func(ctx context.Context, cfg config.Config, modelID string) (model.LLM, error) {
		return VertexModel(ctx, cfg.Vertex, modelID)
	},
	BuildClient: func(cfg config.Config) (*genai.Client, error) {
		return VertexNewClient(context.Background(), cfg.Vertex)
	},
	CallOptions: VertexProviderOptions,
	SupportsImages: func(modelID string) bool {
		return CatalogSupportsImages(catwalk.InferenceProviderVertexAI, modelID, true)
	},
	ContextWindow: func(modelID string) int64 {
		return CatalogContextWindow(catwalk.InferenceProviderVertexAI, modelID, VertexContextWindow)
	},
	MaxOutputTokens: func(modelID string) int64 {
		return CatalogDefaultMaxTokens(catwalk.InferenceProviderVertexAI, modelID, VertexFallbackMaxOutputTokens)
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
