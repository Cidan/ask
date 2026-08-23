package main

import (
	"testing"

	"github.com/Cidan/ask/pkg/providers"
)

func TestModelContextLimit(t *testing.T) {
	cases := []struct {
		provider string
		model    string
		want     int
	}{
		// Unknown provider: the name heuristic.
		{"", "custom-gemini", 1_048_576},
		{"", "custom-1m", 1_048_576},
		{"", "CUSTOM-1M", 1_048_576},
		{"", "unknown-model", 200_000},
		{"", "default", 200_000},
		{"", "", 200_000},
		// A registered provider answers for its own models.
		{vertexProviderID, "gemini-2.5-pro", 1_048_576},
		{vertexProviderID, "unknown-model", 1_048_576},
		{providers.OpenRouterProviderID, "anthropic/claude-3.7-sonnet", 200_000},
	}
	for _, tc := range cases {
		if got := modelContextLimit(tc.provider, tc.model); got != tc.want {
			t.Errorf("modelContextLimit(%q, %q) = %d, want %d", tc.provider, tc.model, got, tc.want)
		}
	}
}

func TestContextPercent(t *testing.T) {
	cases := []struct {
		name  string
		used  int
		limit int
		want  int
	}{
		{"15% of 1M", 150_000, 1_000_000, 15},
		{"20% exact", 200_000, 1_000_000, 20},
		{"0 used", 0, 1_000_000, 0},
		{"over limit clamps to 100", 1_500_000, 1_000_000, 100},
		{"zero limit yields 0", 500, 0, 0},
		{"negative limit yields 0", 500, -1, 0},
		{"small fraction floors to 0", 999, 1_000_000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextPercent(tc.used, tc.limit); got != tc.want {
				t.Errorf("contextPercent(%d, %d) = %d, want %d", tc.used, tc.limit, got, tc.want)
			}
		})
	}
}
