package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatTTL(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"zero time", time.Time{}, "0s"},
		{"past", now.Add(-time.Second), "0s"},
		{"exact now", now, "0s"},
		{"45 seconds", now.Add(45 * time.Second), "45s"},
		{"47m12s", now.Add(47*time.Minute + 12*time.Second), "47m12s"},
		{"exactly 1m", now.Add(time.Minute), "1m0s"},
		{"3h29m", now.Add(3*time.Hour + 29*time.Minute + 15*time.Second), "3h29m"},
		{"exactly 1h", now.Add(time.Hour), "1h0m"},
		{"exactly 24h", now.Add(24 * time.Hour), "1d0h"},
		{"5d23h", now.Add(5*24*time.Hour + 23*time.Hour + 17*time.Minute), "5d23h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTTL(tc.in, now)
			if got != tc.want {
				t.Errorf("formatTTL(%v) = %q, want %q", tc.in.Sub(now), got, tc.want)
			}
		})
	}
}

func TestReadUsageCache_Missing(t *testing.T) {
	isolateHome(t)
	uc, err := readUsageCache()
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if uc != nil {
		t.Errorf("missing file should return nil struct, got %+v", uc)
	}
}

func TestReadUsageCache_Malformed(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".claude", ".usage-cache.json")
	writeFile(t, path, "{not: valid, json")
	uc, err := readUsageCache()
	if err == nil {
		t.Fatal("malformed JSON should return an error")
	}
	if uc != nil {
		t.Errorf("malformed JSON should return nil struct, got %+v", uc)
	}
}

func TestReadUsageCache_Valid(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".claude", ".usage-cache.json")
	payload := `{
		"five_hour": {"utilization": 7.5, "resets_at": "2026-04-23T22:40:00Z"},
		"seven_day": {"utilization": 1.2, "resets_at": "2026-04-29T19:00:00Z"},
		"extra_usage": {"utilization": 72.83}
	}`
	writeFile(t, path, payload)
	uc, err := readUsageCache()
	if err != nil {
		t.Fatalf("valid JSON should parse, got %v", err)
	}
	if uc == nil {
		t.Fatal("valid JSON should return populated struct")
	}
	if uc.FiveHour.Utilization != 7.5 {
		t.Errorf("five_hour utilization = %v, want 7.5", uc.FiveHour.Utilization)
	}
	wantFH, _ := time.Parse(time.RFC3339, "2026-04-23T22:40:00Z")
	if !uc.FiveHour.ResetsAt.Equal(wantFH) {
		t.Errorf("five_hour resets_at = %v, want %v", uc.FiveHour.ResetsAt, wantFH)
	}
	if uc.SevenDay.Utilization != 1.2 {
		t.Errorf("seven_day utilization = %v, want 1.2", uc.SevenDay.Utilization)
	}
}

func TestReadUsageCache_HonorsConfigDir(t *testing.T) {
	isolateHome(t)
	alt := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", alt)
	path := filepath.Join(alt, ".usage-cache.json")
	payload := `{"five_hour": {"utilization": 42}}`
	writeFile(t, path, payload)
	uc, err := readUsageCache()
	if err != nil {
		t.Fatalf("read from CLAUDE_CONFIG_DIR: %v", err)
	}
	if uc == nil || uc.FiveHour.Utilization != 42 {
		t.Errorf("$CLAUDE_CONFIG_DIR not honored: uc=%+v", uc)
	}
}

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

func TestAssistantUsage_FromStreamEvent(t *testing.T) {
	raw := `{
		"type": "assistant",
		"message": {
			"role": "assistant",
			"usage": {
				"input_tokens": 100,
				"cache_read_input_tokens": 50000,
				"cache_creation_input_tokens": 200,
				"output_tokens": 1234
			},
			"content": []
		}
	}`
	var ev map[string]any
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal test event: %v", err)
	}
	tokens, ok := assistantUsage(ev)
	if !ok {
		t.Fatal("expected assistantUsage to return ok=true for event with usage")
	}
	if want := 100 + 50000 + 200; tokens != want {
		t.Errorf("tokens = %d, want %d (output_tokens must be excluded)", tokens, want)
	}
}

func TestAssistantUsage_NoUsageField(t *testing.T) {
	raw := `{"type":"assistant","message":{"role":"assistant","content":[]}}`
	var ev map[string]any
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := assistantUsage(ev); ok {
		t.Error("expected ok=false when message.usage is absent")
	}
}

func TestAssistantUsage_ZeroUsageStillReturnsOk(t *testing.T) {
	raw := `{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":0}}}`
	var ev map[string]any
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tokens, ok := assistantUsage(ev)
	if !ok {
		t.Error("expected ok=true when usage field is present (even if all zero)")
	}
	if tokens != 0 {
		t.Errorf("tokens = %d, want 0", tokens)
	}
}

// TestUsageCache_RoundTrip exercises the serialization shape against a
// realistic payload (mirrors the in-tree sample at ~/.claude/.usage-cache.json),
// so schema drift shows up here rather than in a late-binding runtime
// path.
func TestUsageCache_RoundTrip(t *testing.T) {
	realistic := `{"five_hour":{"utilization":0,"resets_at":"2026-04-23T22:40:00.555575+00:00"},"seven_day":{"utilization":0,"resets_at":"2026-04-29T19:00:00.555624+00:00"},"seven_day_oauth_apps":null,"seven_day_opus":null,"seven_day_sonnet":{"utilization":0,"resets_at":null},"extra_usage":{"is_enabled":true,"monthly_limit":20000,"used_credits":14566,"utilization":72.83,"currency":"USD"}}`
	var uc usageCache
	if err := json.Unmarshal([]byte(realistic), &uc); err != nil {
		t.Fatalf("real-shape cache: %v", err)
	}
	if !strings.HasPrefix(uc.FiveHour.ResetsAt.Format(time.RFC3339), "2026-04-23T22:40:00") {
		t.Errorf("five_hour resets_at parsed wrong: %v", uc.FiveHour.ResetsAt)
	}
	if !strings.HasPrefix(uc.SevenDay.ResetsAt.Format(time.RFC3339), "2026-04-29T19:00:00") {
		t.Errorf("seven_day resets_at parsed wrong: %v", uc.SevenDay.ResetsAt)
	}
}

func TestUsageCachePath_DefaultHome(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	got := usageCachePath()
	want := filepath.Join(home, ".claude", ".usage-cache.json")
	if got != want {
		t.Errorf("usageCachePath() = %q, want %q", got, want)
	}
}

func TestUsageCachePath_ConfigDirOverride(t *testing.T) {
	isolateHome(t)
	alt := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", alt)
	got := usageCachePath()
	want := filepath.Join(alt, ".usage-cache.json")
	if got != want {
		t.Errorf("usageCachePath() with CLAUDE_CONFIG_DIR = %q, want %q", got, want)
	}
}

func TestReadPluginUsageCache_Valid(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".cache.json")
	writeFile(t, cachePath, `{
		"timestamp": 1776973869143,
		"fiveHourPercent": 15,
		"weeklyPercent": 2,
		"fiveHourResetsAt": "2026-04-23T22:40:00Z",
		"weeklyResetsAt": "2026-04-29T19:00:00Z"
	}`)
	uc := readPluginUsageCache(cachePath)
	if uc == nil {
		t.Fatal("expected populated usageCache")
	}
	if uc.FiveHour.Utilization != 15 {
		t.Errorf("five_hour utilization = %v, want 15", uc.FiveHour.Utilization)
	}
	if uc.SevenDay.Utilization != 2 {
		t.Errorf("seven_day utilization = %v, want 2", uc.SevenDay.Utilization)
	}
	wantFH, _ := time.Parse(time.RFC3339, "2026-04-23T22:40:00Z")
	if !uc.FiveHour.ResetsAt.Equal(wantFH) {
		t.Errorf("five_hour resets_at = %v, want %v", uc.FiveHour.ResetsAt, wantFH)
	}
}

func TestReadPluginUsageCache_Missing(t *testing.T) {
	if uc := readPluginUsageCache(""); uc != nil {
		t.Errorf("empty path should return nil, got %+v", uc)
	}
	if uc := readPluginUsageCache(filepath.Join(t.TempDir(), "nope.json")); uc != nil {
		t.Errorf("missing file should return nil, got %+v", uc)
	}
}

func TestReadPluginUsageCache_Malformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".cache.json")
	writeFile(t, path, "{not: valid}")
	if uc := readPluginUsageCache(path); uc != nil {
		t.Errorf("malformed JSON should return nil, got %+v", uc)
	}
}

// TestReadUsageCache_PrefersPlugin verifies that when both the plugin
// cache and the legacy cache file are present, the plugin's snapshot
// wins — it's the source the SessionStart hook kept fresh.
func TestReadUsageCache_PrefersPlugin(t *testing.T) {
	home := isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	// Legacy cache: stale-looking 0%.
	legacyPath := filepath.Join(home, ".claude", ".usage-cache.json")
	writeFile(t, legacyPath, `{"five_hour":{"utilization":0,"resets_at":"2026-04-23T22:40:00Z"},"seven_day":{"utilization":0,"resets_at":"2026-04-29T19:00:00Z"}}`)

	// Plugin cache: fresh 15%.
	pluginPath := filepath.Join(xdg, "ask", "plugins", "ask-usage", ".cache.json")
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, pluginPath, `{"timestamp":1,"fiveHourPercent":15,"weeklyPercent":2,"fiveHourResetsAt":"2026-04-23T22:40:00Z","weeklyResetsAt":"2026-04-29T19:00:00Z"}`)

	uc, err := readUsageCache()
	if err != nil {
		t.Fatalf("readUsageCache: %v", err)
	}
	if uc == nil || uc.FiveHour.Utilization != 15 {
		t.Errorf("plugin cache should win: got %+v", uc)
	}
}

// TestReadUsageCache_FallsBackToLegacy verifies that when the plugin
// cache is absent, the legacy ~/.claude/.usage-cache.json is used.
func TestReadUsageCache_FallsBackToLegacy(t *testing.T) {
	home := isolateHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)

	legacyPath := filepath.Join(home, ".claude", ".usage-cache.json")
	writeFile(t, legacyPath, `{"five_hour":{"utilization":42,"resets_at":"2026-04-23T22:40:00Z"},"seven_day":{"utilization":7,"resets_at":"2026-04-29T19:00:00Z"}}`)

	uc, err := readUsageCache()
	if err != nil {
		t.Fatalf("readUsageCache: %v", err)
	}
	if uc == nil || uc.FiveHour.Utilization != 42 {
		t.Errorf("legacy cache should be used when plugin absent: got %+v", uc)
	}
}

