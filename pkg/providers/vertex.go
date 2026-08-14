package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
	"github.com/Cidan/ask/pkg/config"
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

// FilterVertexModelOptions strips Claude / Anthropic ids from the Vertex catwalk list.
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

// VertexLanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests.
var VertexLanguageModel = func(vc config.VertexConfig, modelID string) (fantasy.LanguageModel, error) {
	project := VertexResolveProject(vc)
	if project == "" {
		return nil, errors.New("vertex: project is required — set it in /config → Vertex AI, or via " + VertexEnvCloudProject)
	}
	location := VertexResolveLocation(vc)
	opts := []google.Option{google.WithVertex(project, location)}

	if _, err := VertexPrepareCredentials(vc); err != nil {
		return nil, err
	}
	provider, err := google.New(opts...)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// VertexProviderOptions translates ask's effort picker onto Gemini's thinking controls.
func VertexProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	if effort == "" || effort == "off" {
		return nil, nil
	}
	resolved := CatalogResolveEffort(catwalk.InferenceProviderVertexAI, modelID, effort)
	clamped := CatalogClampEffort(catwalk.InferenceProviderVertexAI, modelID, resolved)
	if clamped == "" || clamped == "off" {
		return nil, nil
	}
	level := google.ThinkingLevel(strings.ToUpper(clamped))
	opts := &google.ProviderOptions{
		ThinkingConfig: &google.ThinkingConfig{ThinkingLevel: &level},
	}
	return fantasy.ProviderOptions{google.Name: opts}, nil
}

var VertexSpec = AgentProviderSpec{
	ID:            VertexProviderID,
	DisplayName:   "Vertex AI",
	DefaultModel:  VertexDefaultModel,
	ModelOptions:  VertexModelOptions,
	EffortOptions: VertexEffortOptions,
	BuildModel: func(cfg config.Config, modelID string) (fantasy.LanguageModel, error) {
		return VertexLanguageModel(cfg.Vertex, modelID)
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
