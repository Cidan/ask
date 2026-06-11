package main

import (
	"context"
	"errors"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

const (
	deepseekProviderID    = "deepseek"
	deepseekDefaultModel  = "deepseek-v4-pro"
	deepseekContextWindow = 1_000_000
)

// deepseekModelOptions are the API model ids as of the V4 line. The
// deprecated deepseek-chat/deepseek-reasoner aliases (retired
// 2026-07-24) are deliberately absent; AllowCustom covers stragglers.
var deepseekModelOptions = []string{"deepseek-v4-pro", "deepseek-v4-flash"}

// deepseekEffortOptions map onto the API's thinking controls: "off"
// disables thinking entirely; "high"/"max" select reasoning_effort
// (xhigh is DeepSeek's wire name for max).
var deepseekEffortOptions = []string{"off", "high", "max"}

// deepseekLanguageModel builds the fantasy LanguageModel for one
// session. Swappable in tests so StartSession can run against a fake
// model with zero network.
var deepseekLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := resolveDeepSeekAPIKey(cfg)
	if key == "" {
		return nil, errors.New("no API key configured — set one in /config → DeepSeek..., or export " + deepseekEnvAPIKey)
	}
	provider, err := openaicompat.New(
		openaicompat.WithName(deepseekProviderID),
		openaicompat.WithBaseURL(resolveDeepSeekBaseURL(cfg)),
		openaicompat.WithAPIKey(key),
	)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// deepseekProviderOptions translates ask's effort picker onto the wire
// controls, returning the per-call provider options and the sampling
// temperature (DeepSeek recommends 0.0 for coding, but thinking mode
// does not accept sampling params at all, so it only applies to
// thinking=off).
func deepseekProviderOptions(effort string) (fantasy.ProviderOptions, *float64) {
	opts := &openaicompat.ProviderOptions{}
	var temperature *float64
	switch effort {
	case "off":
		opts.ExtraBody = map[string]any{"thinking": map[string]any{"type": "disabled"}}
		t := 0.0
		temperature = &t
	case "max":
		e := openai.ReasoningEffortXHigh
		opts.ReasoningEffort = &e
	default: // "high" and unset both ride the default thinking mode
		e := openai.ReasoningEffortHigh
		opts.ReasoningEffort = &e
	}
	return fantasy.ProviderOptions{deepseekProviderID: opts}, temperature
}

var deepseekSpec = agentProviderSpec{
	id:            deepseekProviderID,
	displayName:   "DeepSeek",
	defaultModel:  deepseekDefaultModel,
	modelOptions:  deepseekModelOptions,
	effortOptions: deepseekEffortOptions,
	buildModel: func(cfg askConfig, modelID string) (fantasy.LanguageModel, error) {
		return deepseekLanguageModel(cfg.DeepSeek, modelID)
	},
	callOptions: func(_, effort string) (fantasy.ProviderOptions, *float64) {
		return deepseekProviderOptions(effort)
	},
	// The V4 models do not accept image input.
	supportsImages: func(string) bool { return false },
	contextWindow:  func(string) int64 { return deepseekContextWindow },
	loadSettings: func(cfg askConfig) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.DeepSeek.Model,
			Effort:        cfg.DeepSeek.Effort,
			SlashCommands: cfg.DeepSeek.SlashCommands,
		}
	},
	saveSettings: func(cfg *askConfig, s ProviderSettings) {
		cfg.DeepSeek.Model = s.Model
		cfg.DeepSeek.Effort = s.Effort
		cfg.DeepSeek.SlashCommands = s.SlashCommands
	},
}

func deepseekAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &deepseekSpec} }

func deepseekStore() *agentSessionStore {
	return &agentSessionStore{provider: deepseekProviderID}
}
