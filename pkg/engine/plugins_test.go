package engine

import (
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/genai"
)

func TestDefaultPlugins_Configuration(t *testing.T) {
	plugins := DefaultPlugins()
	if len(plugins) == 0 {
		t.Fatal("expected DefaultPlugins to return at least 1 plugin, got 0")
	}

	hasRetry := false
	hasModifier := false
	for _, p := range plugins {
		if p == nil {
			t.Fatal("received nil plugin in DefaultPlugins()")
		}
		switch p.Name() {
		case "RetryAndReflectPlugin":
			hasRetry = true
		case "FunctionCallModifierPlugin":
			hasModifier = true
		}
	}

	if !hasRetry {
		t.Error("expected RetryAndReflectPlugin to be present in DefaultPlugins")
	}
	if !hasModifier {
		t.Error("expected FunctionCallModifierPlugin to be present in DefaultPlugins")
	}
}

func TestNewRetryAndReflectPlugin(t *testing.T) {
	t.Run("default max retries", func(t *testing.T) {
		p, err := NewRetryAndReflectPlugin(0)
		if err != nil {
			t.Fatalf("unexpected error creating retry and reflect plugin: %v", err)
		}
		if p == nil {
			t.Fatal("expected non-nil plugin")
		}
		if p.Name() != "RetryAndReflectPlugin" {
			t.Errorf("expected plugin name 'RetryAndReflectPlugin', got %q", p.Name())
		}
	})

	t.Run("custom max retries", func(t *testing.T) {
		p, err := NewRetryAndReflectPlugin(3)
		if err != nil {
			t.Fatalf("unexpected error creating retry and reflect plugin: %v", err)
		}
		if p == nil {
			t.Fatal("expected non-nil plugin")
		}
	})
}


func TestFunctionCallModifier_ActiveParameterInjection(t *testing.T) {
	plugins := DefaultPlugins()
	var modPlugin *plugin.Plugin
	for _, p := range plugins {
		if p != nil && p.Name() == "FunctionCallModifierPlugin" {
			modPlugin = p
			break
		}
	}
	if modPlugin == nil {
		t.Fatal("expected FunctionCallModifierPlugin in DefaultPlugins")
	}

	// Test isCoreCodingTool predicate
	coreTools := []string{"read", "write", "edit", "glob", "grep", "ls", "bash", "job_output", "job_kill", "fetch", "todos", "ask_user_question", "end_turn", "web_search"}
	for _, name := range coreTools {
		if !isCoreCodingTool(name) {
			t.Errorf("expected %q to be recognized as core coding tool", name)
		}
	}

	nonCore := []string{"custom_tool", "random_func", "unknown"}
	for _, name := range nonCore {
		if isCoreCodingTool(name) {
			t.Errorf("expected %q not to be recognized as core coding tool", name)
		}
	}
}

func TestDefaultPlugins_DoesNotCorruptParametersJsonSchema(t *testing.T) {
	plugins := DefaultPlugins()
	var modPlugin *plugin.Plugin
	for _, p := range plugins {
		if p != nil && p.Name() == "FunctionCallModifierPlugin" {
			modPlugin = p
			break
		}
	}
	if modPlugin == nil {
		t.Fatal("expected FunctionCallModifierPlugin in DefaultPlugins")
	}

	// Create a tool with ParametersJsonSchema
	decl := &genai.FunctionDeclaration{
		Name:                 "read",
		Description:          "Read file content",
		ParametersJsonSchema: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}},
	}

	req := &model.LLMRequest{
		Tools: map[string]any{
			"read": struct{}{},
		},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{
				{
					FunctionDeclarations: []*genai.FunctionDeclaration{decl},
				},
			},
		},
	}

	// Run BeforeModelCallback
	actx := NewStandaloneAgentContext(nil)
	cb := modPlugin.BeforeModelCallback()
	if cb != nil {
		_, err := cb(actx, req)
		if err != nil {
			t.Fatalf("unexpected error from BeforeModelCallback: %v", err)
		}
	}

	// Parameters must remain nil to prevent Vertex AI proto validation failure
	if decl.Parameters != nil {
		t.Errorf("decl.Parameters was mutated to non-nil: %+v; this triggers proto validation error with ParametersJsonSchema", decl.Parameters)
	}
	if decl.ParametersJsonSchema == nil {
		t.Errorf("decl.ParametersJsonSchema was unexpectedly cleared")
	}
}
