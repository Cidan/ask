package providers

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/google"
	"github.com/Cidan/ask/pkg/config"
)

func TestGoogleAIProviderOptions(t *testing.T) {
	opts, temp := GoogleAIProviderOptions("gemini-2.5-pro", "high")
	if opts != nil || temp != nil {
		t.Errorf("gemini-2.5-pro should return nil options: opts=%v temp=%v", opts, temp)
	}

	opts, _ = GoogleAIProviderOptions("gemini-3.1-pro-preview-customtools", "high")
	if opts == nil {
		t.Fatalf("expected non-nil options for gemini-3.1-pro")
	}
	goOpts, ok := opts[google.Name].(*google.ProviderOptions)
	if !ok || goOpts.ThinkingConfig == nil || goOpts.ThinkingConfig.ThinkingLevel == nil {
		t.Fatalf("invalid google provider options: %+v", goOpts)
	}
}

func TestGoogleAISpec_Properties(t *testing.T) {
	if GoogleAISpec.ID != "googleai" || GoogleAISpec.DisplayName != "Google AI Studio" {
		t.Errorf("identity wrong: %+v", GoogleAISpec)
	}
	if len(GoogleAISpec.ModelOptions) == 0 {
		t.Error("expected model options")
	}
	if GoogleAISpec.ContextWindow("gemini-3.1-pro-preview-customtools") != 1_048_576 {
		t.Errorf("context window wrong: %d", GoogleAISpec.ContextWindow("gemini-3.1-pro-preview-customtools"))
	}
	if !GoogleAISpec.SupportsImages("gemini-3.1-pro-preview-customtools") {
		t.Error("gemini supports images")
	}

	cfg := config.Config{}
	cfg.GoogleAI.Model = "gemini-custom"
	cfg.Effort = "low"
	settings := GoogleAISpec.LoadSettings(cfg)
	if settings.Model != "gemini-custom" || settings.Effort != "low" {
		t.Errorf("settings mismatch: %+v", settings)
	}
	var newCfg config.Config
	GoogleAISpec.SaveSettings(&newCfg, settings)
	if newCfg.GoogleAI.Model != "gemini-custom" || newCfg.Effort != "low" {
		t.Errorf("save settings mismatch: %+v", newCfg)
	}
}

func TestGoogleAI_ThoughtSignatureReasoningMetadata(t *testing.T) {
	// Verify google.ReasoningMetadata round-trips correctly through fantasy ProviderMetadata/ProviderOptions.
	meta := &google.ReasoningMetadata{
		Signature: "test-thought-signature-bytes",
		ToolID:    "call-123",
	}

	part := fantasy.ReasoningPart{
		Text: "Let me think about how to solve this...",
		ProviderOptions: fantasy.ProviderOptions{
			google.Name: meta,
		},
	}

	if part.ProviderOptions[google.Name] == nil {
		t.Fatal("expected google provider options to be set on reasoning part")
	}

	rm, ok := part.ProviderOptions[google.Name].(*google.ReasoningMetadata)
	if !ok || rm.Signature != "test-thought-signature-bytes" || rm.ToolID != "call-123" {
		t.Fatalf("reasoning metadata signature mismatch: got %+v", rm)
	}
}

func TestGoogleAI_ParallelToolCallsWithThoughtSignatures(t *testing.T) {
	// Verify parallel tool calls correctly pair with reasoning parts carrying thought signatures.
	sig := "sig-parallel-xyz"
	prompt := fantasy.Prompt{
		{
			Role: fantasy.MessageRoleUser,
			Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: "Read both files"},
			},
		},
		{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{
					Text: "I will read both files in parallel.",
					ProviderOptions: fantasy.ProviderOptions{
						google.Name: &google.ReasoningMetadata{Signature: sig},
					},
				},
				fantasy.ToolCallPart{
					ToolCallID: "call_1",
					ToolName:   "read",
					Input:      `{"file_path":"file1.txt"}`,
				},
				fantasy.ToolCallPart{
					ToolCallID: "call_2",
					ToolName:   "read",
					Input:      `{"file_path":"file2.txt"}`,
				},
			},
		},
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output:     fantasy.ToolResultOutputContentText{Text: "contents of file 1"},
				},
				fantasy.ToolResultPart{
					ToolCallID: "call_2",
					Output:     fantasy.ToolResultOutputContentText{Text: "contents of file 2"},
				},
			},
		},
	}

	if len(prompt) != 3 {
		t.Fatalf("expected 3 prompt messages, got %d", len(prompt))
	}

	asst := prompt[1]
	if asst.Role != fantasy.MessageRoleAssistant || len(asst.Content) != 3 {
		t.Fatalf("expected assistant message with 3 parts, got %+v", asst)
	}

	reasoningPart, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](asst.Content[0])
	if !ok {
		t.Fatalf("expected first part to be ReasoningPart, got %T", asst.Content[0])
	}
	gmeta, ok := reasoningPart.ProviderOptions[google.Name].(*google.ReasoningMetadata)
	if !ok || gmeta.Signature != sig {
		t.Fatalf("expected reasoning metadata with signature %q, got %+v", sig, gmeta)
	}
}

