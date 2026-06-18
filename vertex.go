package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
)

const (
	vertexProviderID              = "vertex"
	vertexDefaultModel            = "gemini-3.1-pro-preview"
	vertexDefaultLocation         = "global"
	vertexContextWindow           = 1_048_576
	vertexFallbackMaxOutputTokens = 32_000

	// vertexEnvApplicationCredentials is the env var the genai
	// library consults via credentials.DetectDefault to find a
	// service-account JSON. Set in vertexApplyEnv when the user
	// has configured cfg.Vertex.ServiceAccountKey (or wants the
	// existing env value promoted); otherwise the same detection
	// path falls through to gcloud / GCE metadata.
	vertexEnvApplicationCredentials = "GOOGLE_APPLICATION_CREDENTIALS"

	// vertexEnvCloudProject is the env-var fallback for the project
	// id, mirroring `gcloud config get-value project` behavior.
	vertexEnvCloudProject = "GOOGLE_CLOUD_PROJECT"
)

// vertexEffortOptions is the picker surface for reasoning effort.
// Same shape as googleai: catalogClampEffort de-grades a pick the
// chosen model doesn't support (Vertex's Gemini 3.1 Pro rejects
// "minimal" — it drops to "low").
var vertexEffortOptions = []string{"minimal", "low", "medium", "high"}

// vertexModelOptions filters the catalog's Vertex model list: the
// Vertex catwalk bundle ships five Claude models alongside the
// Gemini line, but fantasy's google provider transparently routes
// those through anthropic.WithVertex (different wire, different
// reasoning enums). They are out of scope for v1 — users wanting
// Claude-on-Vertex use the regular Anthropic provider.
var vertexModelOptions = filterVertexModelOptions(catalogModelIDs(catwalk.InferenceProviderVertexAI))

// filterVertexModelOptions strips Claude / Anthropic ids from the
// Vertex catwalk list. The substring match covers the catwalk
// naming patterns we see today (claude-sonnet-4-6, claude-opus-4-5-
// 20251101, …); an "anthropic" guard catches any future ids the
// provider would also auto-route to anthropic.WithVertex.
func filterVertexModelOptions(all []string) []string {
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

// vertexResolveProject: config value wins, then GOOGLE_CLOUD_PROJECT.
// Empty result is the fail-fast signal at session start.
func vertexResolveProject(vc vertexConfig) string {
	if vc.Project != "" {
		return vc.Project
	}
	return os.Getenv(vertexEnvCloudProject)
}

// vertexResolveLocation: config value wins, then the documented
// Vertex "global" default (broadest availability for current Gemini
// 3.x models).
func vertexResolveLocation(vc vertexConfig) string {
	if vc.Location != "" {
		return vc.Location
	}
	return vertexDefaultLocation
}

// vertexApplyEnv is the testable seam for the SA-key env-mutation
// strategy. The real implementation calls os.Setenv so the genai
// library's credentials.DetectDefault finds the SA key bytes on
// the next call. Tests swap this var to assert the path is being
// propagated without actually mutating the process env.
//
// Note: the env var is process-wide. Two Vertex sessions with
// different SA key paths in the same process would race; the last
// Setenv wins. v1 limitation — "one Vertex project per process" —
// documented in the spec comment.
var vertexApplyEnv = func(path string) {
	_ = os.Setenv(vertexEnvApplicationCredentials, path)
}

// vertexPrepareCredentials resolves the SA key path, validates it
// is readable, and applies the env var so the genai library's
// credentials.DetectDefault finds the bytes on the next call.
// Returns the path in use, or "" when no SA key is configured
// (ADC discovery via gcloud / GCE metadata is then the fallback).
// Extracted from vertexLanguageModel so the SA-key resolution
// chain (config > env > ADC) is unit-testable without spinning up
// a real google.New.
//
// Tests swap vertexApplyEnv to assert the env mutation without
// polluting the process env.
var vertexPrepareCredentials = func(vc vertexConfig) (string, error) {
	saKeyPath := vc.ServiceAccountKey
	if saKeyPath == "" {
		saKeyPath = os.Getenv(vertexEnvApplicationCredentials)
	}
	if saKeyPath == "" {
		return "", nil
	}
	if _, err := os.Stat(saKeyPath); err != nil {
		return "", fmt.Errorf("vertex: read service account key %s: %w", saKeyPath, err)
	}
	vertexApplyEnv(saKeyPath)
	return saKeyPath, nil
}

// vertexLanguageModel builds the fantasy LanguageModel for one
// session. Swappable in tests so StartSession can run against a
// fake model with zero network.
var vertexLanguageModel = func(vc vertexConfig, modelID string) (fantasy.LanguageModel, error) {
	project := vertexResolveProject(vc)
	if project == "" {
		return nil, errors.New("vertex: project is required — set it in /config → Vertex AI, or via " + vertexEnvCloudProject)
	}
	location := vertexResolveLocation(vc)
	opts := []google.Option{google.WithVertex(project, location)}

	// Service-account key resolution: explicit config wins, then the
	// env var, then ADC discovery (gcloud / GCE metadata). When we
	// have a path, set the env so credentials.DetectDefault — which
	// fantasy's UseDefaultCredentials calls into — finds it.
	if _, err := vertexPrepareCredentials(vc); err != nil {
		return nil, err
	}
	provider, err := google.New(opts...)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// vertexProviderOptions translates ask's effort picker onto Gemini's
// thinking controls. Same shape as googleaiProviderOptions — both
// providers share the ThinkingConfig + ThinkingLevel wire surface.
func vertexProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	if effort == "" || effort == "off" {
		return nil, nil
	}
	clamped := catalogClampEffort(catwalk.InferenceProviderVertexAI, modelID, effort)
	if clamped == "" || clamped == "off" {
		return nil, nil
	}
	level := google.ThinkingLevel(strings.ToUpper(clamped))
	opts := &google.ProviderOptions{
		ThinkingConfig: &google.ThinkingConfig{ThinkingLevel: &level},
	}
	return fantasy.ProviderOptions{google.Name: opts}, nil
}

var vertexSpec = agentProviderSpec{
	id:            vertexProviderID,
	displayName:   "Vertex AI",
	defaultModel:  vertexDefaultModel,
	modelOptions:  vertexModelOptions,
	effortOptions: vertexEffortOptions,
	buildModel: func(cfg askConfig, modelID string) (fantasy.LanguageModel, error) {
		return vertexLanguageModel(cfg.Vertex, modelID)
	},
	callOptions: vertexProviderOptions,
	// Every Gemini model in the Vertex catalog supports image
	// attachments; unknown ids default to true (the same reasoning
	// as googleai — silent drops are worse than a clear error).
	supportsImages: func(modelID string) bool {
		return catalogSupportsImages(catwalk.InferenceProviderVertexAI, modelID, true)
	},
	contextWindow: func(modelID string) int64 {
		return catalogContextWindow(catwalk.InferenceProviderVertexAI, modelID, vertexContextWindow)
	},
	maxOutputTokens: func(modelID string) int64 {
		return catalogDefaultMaxTokens(catwalk.InferenceProviderVertexAI, modelID, vertexFallbackMaxOutputTokens)
	},
	loadSettings: func(cfg askConfig) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.Vertex.Model,
			Effort:        cfg.Vertex.Effort,
			SlashCommands: cfg.Vertex.SlashCommands,
		}
	},
	saveSettings: func(cfg *askConfig, s ProviderSettings) {
		cfg.Vertex.Model = s.Model
		cfg.Vertex.Effort = s.Effort
		cfg.Vertex.SlashCommands = s.SlashCommands
	},
}

func vertexAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &vertexSpec} }

func vertexStore() *agentSessionStore {
	return &agentSessionStore{provider: vertexProviderID}
}
