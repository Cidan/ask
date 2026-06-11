package main

import "testing"

func TestModelContextLimit(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"opus[1m]", 1_000_000},
		{"sonnet[1m]", 1_000_000},
		{"claude-opus-4-7-1m", 1_000_000},
		{"claude-OPUS-4-7-1M", 1_000_000},
		{"sonnet", 200_000},
		{"opus", 200_000},
		{"default", 200_000},
		{"", 200_000},
	}
	for _, tc := range cases {
		if got := modelContextLimit(tc.model); got != tc.want {
			t.Errorf("modelContextLimit(%q) = %d, want %d", tc.model, got, tc.want)
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
