package engine

import (
	"testing"

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

func TestNewFunctionCallModifierPlugin(t *testing.T) {
	t.Run("with nil predicate and args", func(t *testing.T) {
		p, err := NewFunctionCallModifierPlugin(FunctionCallModifierOptions{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p == nil || p.Name() != "FunctionCallModifierPlugin" {
			t.Fatalf("expected valid FunctionCallModifierPlugin, got %v", p)
		}
	})

	t.Run("with custom schema args and description override", func(t *testing.T) {
		p, err := NewFunctionCallModifierPlugin(FunctionCallModifierOptions{
			Predicate: func(toolName string) bool {
				return toolName == "read"
			},
			Args: map[string]*genai.Schema{
				"description": {
					Type:        "STRING",
					Description: "short phrase",
				},
			},
			OverrideDescription: func(orig string) string {
				return orig + " (modified)"
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p == nil || p.Name() != "FunctionCallModifierPlugin" {
			t.Fatalf("expected valid FunctionCallModifierPlugin, got %v", p)
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
