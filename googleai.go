package main

import (
	"context"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
)

const (
	googleaiProviderID              = "googleai"
	googleaiDefaultModel            = "gemini-3.1-pro-preview-customtools"
	googleaiContextWindow           = 1_048_576
	googleaiFallbackMaxOutputTokens = 32_000
)

// googleaiEffortOptions is the picker surface for reasoning effort.
// catalogClampEffort degrades a pick the chosen model doesn't offer
// (e.g. "minimal" on a gemini-3.1-pro — which only supports
// [low, medium, high] — drops to "low").
var googleaiEffortOptions = []string{"minimal", "low", "medium", "high"}

// googleaiLanguageModel builds the fantasy LanguageModel for one
// session. Swappable in tests so StartSession can run against a fake
// model with zero network.
var googleaiLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := resolveGoogleAIAPIKey(cfg)
	if key == "" {
		return nil, missingAPIKeyError(googleaiEnvAPIKey)
	}
	provider, err := google.New(google.WithGeminiAPIKey(key))
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// googleaiProviderOptions translates ask's effort picker onto the
// Gemini wire controls. effort="low"/"medium"/"high" map onto
// google.ThinkingLevel; "minimal" maps to ThinkingLevelMinimal;
// anything unmapped (including the empty string) is a no-op so
// the API default kicks in. catalogClampEffort de-grades a pick
// the chosen model doesn't support (Gemini 3.1 Pro rejects
// "minimal", Gemini 2.5 Pro rejects everything).
func googleaiProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	if effort == "" || effort == "off" {
		return nil, nil
	}
	clamped := catalogClampEffort(catwalk.InferenceProviderGemini, modelID, effort)
	if clamped == "" || clamped == "off" {
		return nil, nil
	}
	level := google.ThinkingLevel(strings.ToUpper(clamped))
	opts := &google.ProviderOptions{
		ThinkingConfig: &google.ThinkingConfig{ThinkingLevel: &level},
	}
	return fantasy.ProviderOptions{google.Name: opts}, nil
}

var googleaiSpec = agentProviderSpec{
	id:            googleaiProviderID,
	displayName:   "Google AI Studio",
	defaultModel:  googleaiDefaultModel,
	modelOptions:  catalogModelIDs(catwalk.InferenceProviderGemini),
	effortOptions: googleaiEffortOptions,
	buildModel: func(cfg askConfig, modelID string) (fantasy.LanguageModel, error) {
		return googleaiLanguageModel(cfg.GoogleAI, modelID)
	},
	callOptions: googleaiProviderOptions,
	// Every Gemini model in catwalk supports image attachments
	// (supports_attachments=true). Unknown ids default to true —
	// the alternative is silently dropping a paste which is worse
	// than saying so.
	supportsImages: func(modelID string) bool {
		return catalogSupportsImages(catwalk.InferenceProviderGemini, modelID, true)
	},
	contextWindow: func(modelID string) int64 {
		return catalogContextWindow(catwalk.InferenceProviderGemini, modelID, googleaiContextWindow)
	},
	maxOutputTokens: func(modelID string) int64 {
		return catalogDefaultMaxTokens(catwalk.InferenceProviderGemini, modelID, googleaiFallbackMaxOutputTokens)
	},
	loadSettings: func(cfg askConfig) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.GoogleAI.Model,
			Effort:        cfg.GoogleAI.Effort,
			SlashCommands: cfg.GoogleAI.SlashCommands,
		}
	},
	saveSettings: func(cfg *askConfig, s ProviderSettings) {
		cfg.GoogleAI.Model = s.Model
		cfg.GoogleAI.Effort = s.Effort
		cfg.GoogleAI.SlashCommands = s.SlashCommands
	},
}

func googleaiAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &googleaiSpec} }

func googleaiStore() *agentSessionStore {
	return &agentSessionStore{provider: googleaiProviderID}
}
