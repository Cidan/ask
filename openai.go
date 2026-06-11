package main

import (
	"context"
	"errors"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
)

const (
	openaiProviderID   = "openai"
	openaiDefaultModel = "gpt-5.5"
	// openaiFallbackContextWindow is the conservative window assumed
	// for model ids the catalog doesn't know.
	openaiFallbackContextWindow = 200_000
)

// openaiEffortOptions mirror the Responses API reasoning_effort
// levels. catalogClampEffort degrades a pick the chosen model doesn't
// offer (e.g. xhigh on a high-capped model → high).
var openaiEffortOptions = []string{"minimal", "low", "medium", "high", "xhigh"}

// openaiUseResponsesAPI routes the reasoning lineups through the
// Responses API by prefix instead of fantasy's exact-id list, so a
// freshly released gpt-5.x doesn't silently fall back to chat
// completions (codex models are Responses-only).
func openaiUseResponsesAPI(modelID string) bool {
	for _, prefix := range []string{"gpt-5", "o1", "o3", "o4", "codex", "gpt-oss"} {
		if strings.HasPrefix(modelID, prefix) {
			return true
		}
	}
	return false
}

// openaiLanguageModel builds the fantasy LanguageModel for one
// session. Swappable in tests.
var openaiLanguageModel = func(cfg apiProviderConfig, modelID string) (fantasy.LanguageModel, error) {
	key := resolveOpenAIAPIKey(cfg)
	if key == "" {
		return nil, errors.New("no API key configured — set one in /config → OpenAI..., or export " + openaiEnvAPIKey)
	}
	opts := []openai.Option{
		openai.WithAPIKey(key),
		openai.WithUseResponsesAPI(),
		openai.WithResponsesAPIFunc(openaiUseResponsesAPI),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, openai.WithBaseURL(cfg.BaseURL))
	}
	provider, err := openai.New(opts...)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(context.Background(), modelID)
}

// openaiProviderOptions maps ask's effort picker onto the Responses
// API. Encrypted reasoning content is always requested and reasoning
// summaries enabled: responses are stored stateless (Store defaults
// false), so replaying a persisted transcript across turns and
// /resume needs the encrypted blobs round-tripped in the messages.
func openaiProviderOptions(modelID, effort string) (fantasy.ProviderOptions, *float64) {
	summary := "auto"
	opts := &openai.ResponsesProviderOptions{
		Include:          []openai.IncludeType{openai.IncludeReasoningEncryptedContent},
		ReasoningSummary: &summary,
	}
	if effort != "" {
		e := openai.ReasoningEffort(catalogClampEffort(catwalk.InferenceProviderOpenAI, modelID, effort))
		opts.ReasoningEffort = &e
	}
	return fantasy.ProviderOptions{openai.Name: opts}, nil
}

var openaiSpec = agentProviderSpec{
	id:            openaiProviderID,
	displayName:   "OpenAI",
	defaultModel:  openaiDefaultModel,
	modelOptions:  catalogModelIDs(catwalk.InferenceProviderOpenAI),
	effortOptions: openaiEffortOptions,
	buildModel: func(cfg askConfig, modelID string) (fantasy.LanguageModel, error) {
		return openaiLanguageModel(cfg.OpenAI, modelID)
	},
	callOptions: openaiProviderOptions,
	supportsImages: func(modelID string) bool {
		// The gpt-5/codex lineups all accept images; default unknown
		// (custom) ids to true and let the catalog override.
		return catalogSupportsImages(catwalk.InferenceProviderOpenAI, modelID, true)
	},
	contextWindow: func(modelID string) int64 {
		return catalogContextWindow(catwalk.InferenceProviderOpenAI, modelID, openaiFallbackContextWindow)
	},
	loadSettings: func(cfg askConfig) ProviderSettings {
		return ProviderSettings{
			Model:         cfg.OpenAI.Model,
			Effort:        cfg.OpenAI.Effort,
			SlashCommands: cfg.OpenAI.SlashCommands,
		}
	},
	saveSettings: func(cfg *askConfig, s ProviderSettings) {
		cfg.OpenAI.Model = s.Model
		cfg.OpenAI.Effort = s.Effort
		cfg.OpenAI.SlashCommands = s.SlashCommands
	},
}

func openaiAgentProvider() agentAPIProvider { return agentAPIProvider{spec: &openaiSpec} }
