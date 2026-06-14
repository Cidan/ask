package main

import (
	"context"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
)

const (
	minimaxProviderID              = "minimax"
	minimaxDefaultModel            = "MiniMax-M3"
	minimaxFallbackContextWindow   = 200_000
	minimaxFallbackMaxOutputTokens = 32_000
)

// minimaxModelOptions exposes the flagship M3 model by default. AllowCustom
// covers the rest of the MiniMax catalog (M2.x, etc.).
var minimaxModelOptions = []string{"MiniMax-M3"}

// minimaxEffortOptions controls M3's reasoning/thinking mode.
var minimaxEffortOptions = []string{"off", "high"}

// minimaxLanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests so StartSession can run against a fake model.
var minimaxLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := resolveMiniMaxAPIKey(cfg)
	if key == "" {
		return nil, missingAPIKeyError(minimaxEnvAPIKey)
	}
	provider, err := openaicompat.New(
		openaicompat.WithName(minimaxProviderID),
		openaicompat.WithBaseURL(resolveMiniMaxBaseURL(cfg)),
		openaicompat.WithAPIKey(key),
	)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// minimaxProviderOptions maps ask's effort picker onto MiniMax's OpenAI-
// compatible thinking controls. "off" disables reasoning and pins temperature
// to 0.0; "high" enables adaptive thinking and asks the API to split
// reasoning content into reasoning_details.
func minimaxProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	opts := &openaicompat.ProviderOptions{}
	var temperature *float64
	switch effort {
	case "off":
		opts.ExtraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
		t := 0.0
		temperature = &t
	default: // "high" and unset
		opts.ExtraBody = map[string]any{
			"thinking":        map[string]any{"type": "adaptive"},
			"reasoning_split": true,
		}
	}
	return fantasy.ProviderOptions{minimaxProviderID: opts}, temperature
}

var minimaxSpec = agentProviderSpec{
	id:            minimaxProviderID,
	displayName:   "MiniMax",
	defaultModel:  minimaxDefaultModel,
	modelOptions:  minimaxModelOptions,
	effortOptions: minimaxEffortOptions,
	buildModel: func(cfg askConfig, modelID string) (fantasy.LanguageModel, error) {
		return minimaxLanguageModel(cfg.MiniMax, modelID)
	},
	callOptions: func(_, effort string) (fantasy.ProviderOptions, *float64) {
		return minimaxProviderOptions(effort)
	},
	// MiniMax-M3 supports image and video input on the OpenAI-compatible API.
	supportsImages: func(modelID string) bool { return modelID == minimaxDefaultModel },
	contextWindow: func(modelID string) int64 {
		return catalogContextWindow(catwalk.InferenceProviderMiniMax, modelID, minimaxFallbackContextWindow)
	},
	maxOutputTokens: func(modelID string) int64 {
		return catalogDefaultMaxTokens(catwalk.InferenceProviderMiniMax, modelID, minimaxFallbackMaxOutputTokens)
	},
	loadSettings: func(cfg askConfig) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.MiniMax.Model,
			Effort:        cfg.MiniMax.Effort,
			SlashCommands: cfg.MiniMax.SlashCommands,
		}
	},
	saveSettings: func(cfg *askConfig, s ProviderSettings) {
		cfg.MiniMax.Model = s.Model
		cfg.MiniMax.Effort = s.Effort
		cfg.MiniMax.SlashCommands = s.SlashCommands
	},
}

func minimaxAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &minimaxSpec} }

func minimaxStore() *agentSessionStore {
	return &agentSessionStore{provider: minimaxProviderID}
}
