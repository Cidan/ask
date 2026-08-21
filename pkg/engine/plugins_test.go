package engine

import (
	"strings"
	"testing"
)

func TestDefaultPlugins_Configuration(t *testing.T) {
	plugins := DefaultPlugins()
	if len(plugins) == 0 {
		t.Fatal("expected DefaultPlugins to return at least 1 plugin, got 0")
	}

	hasRetry := false
	for _, p := range plugins {
		if p == nil {
			t.Fatal("received nil plugin in DefaultPlugins()")
		}
		if p.Name() == "RetryAndReflectPlugin" {
			hasRetry = true
		}
	}

	if !hasRetry {
		t.Error("expected RetryAndReflectPlugin to be present in DefaultPlugins")
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

// The description phrase is a static field on every native tool's params
// struct, so nothing needs to inject it at request time. This pins that
// the plugin list stays free of functioncallmodifier, whose only job here
// was that injection and which shipped disabled.
func TestDefaultPlugins_NoFunctionCallModifier(t *testing.T) {
	for _, p := range DefaultPlugins() {
		if p != nil && strings.Contains(p.Name(), "FunctionCallModifier") {
			t.Error("functioncallmodifier is redundant: description is a real params field")
		}
	}
}
