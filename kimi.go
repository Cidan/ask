package main

import (
	"context"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

const (
	kimiProviderID             = "kimi"
	kimiDefaultModel           = "kimi-k2.7-code"
	kimiContextWindow          = 128_000
	kimiFallbackMaxOutputTokens = 32_000
)

var kimiModelOptions = []string{"kimi-k2.7-code", "kimi-k2.5", "kimi-k2-thinking"}

var kimiEffortOptions = []string{"off", "high"}

// kimiLanguageModel builds the fantasy LanguageModel for one session.
// Swappable in tests so StartSession can run against a fake model with
// zero network.
var kimiLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := resolveKimiAPIKey(cfg)
	if key == "" {
		return nil, missingAPIKeyError(moonshotEnvAPIKey)
	}
	provider, err := openaicompat.New(
		openaicompat.WithName(kimiProviderID),
		openaicompat.WithBaseURL(resolveKimiBaseURL(cfg)),
		openaicompat.WithAPIKey(key),
	)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// kimiProviderOptions translates ask's effort picker onto the wire
// controls. Kimi supports thinking on/off via the same openaicompat
// pattern as DeepSeek: "off" disables thinking, "high" engages it.
func kimiProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	opts := &openaicompat.ProviderOptions{}
	var temperature *float64
	switch effort {
	case "off":
		opts.ExtraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
		t := 0.0
		temperature = &t
	default: // "high" and unset both ride the default thinking mode
		e := openai.ReasoningEffortHigh
		opts.ReasoningEffort = &e
	}
	return fantasy.ProviderOptions{kimiProviderID: opts}, temperature
}

var kimiSpec = agentProviderSpec{
	id:            kimiProviderID,
	displayName:   "Kimi",
	defaultModel:  kimiDefaultModel,
	modelOptions:  kimiModelOptions,
	effortOptions: kimiEffortOptions,
	buildModel: func(cfg askConfig, modelID string) (fantasy.LanguageModel, error) {
		return kimiLanguageModel(cfg.Moonshot, modelID)
	},
	callOptions: func(_, effort string) (fantasy.ProviderOptions, *float64) {
		return kimiProviderOptions(effort)
	},
	// kimi-k2.5 supports vision; kimi-k2-thinking does not.
	supportsImages: func(modelID string) bool { return modelID != "kimi-k2-thinking" },
	contextWindow:  func(string) int64 { return kimiContextWindow },
	maxOutputTokens: func(string) int64 {
		return kimiFallbackMaxOutputTokens
	},
	loadSettings: func(cfg askConfig) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.Moonshot.Model,
			Effort:        cfg.Moonshot.Effort,
			SlashCommands: cfg.Moonshot.SlashCommands,
		}
	},
	saveSettings: func(cfg *askConfig, s ProviderSettings) {
		cfg.Moonshot.Model = s.Model
		cfg.Moonshot.Effort = s.Effort
		cfg.Moonshot.SlashCommands = s.SlashCommands
	},
}

func kimiAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &kimiSpec} }

func kimiStore() *agentSessionStore {
	return &agentSessionStore{provider: kimiProviderID}
}
