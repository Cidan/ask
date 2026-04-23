package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// usageCache mirrors the fields we care about in ~/.claude/.usage-cache.json.
// claude writes 0-100 floats (not 0-1) here; ResetsAt is an RFC3339 UTC
// timestamp, left as zero time.Time when the wire field is null.
type usageCache struct {
	FiveHour usageWindow `json:"five_hour"`
	SevenDay usageWindow `json:"seven_day"`
}

type usageWindow struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

// readUsageCache reads the claude CLI's usage cache file.
//
//   - (nil, nil) when the file is absent — claude may not have written it
//     yet, or this machine has never run claude. Best-effort: callers
//     should treat this as "no data" and just omit the 5h/wk segments.
//   - (nil, err) when JSON is malformed or the file is otherwise unreadable
//     (permissions, IO). debugLog records the reason.
//   - (populated, nil) on success.
func readUsageCache() (*usageCache, error) {
	path := usageCachePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		debugLog("usage cache read: %v", err)
		return nil, err
	}
	var uc usageCache
	if err := json.Unmarshal(data, &uc); err != nil {
		debugLog("usage cache parse: %v", err)
		return nil, err
	}
	return &uc, nil
}

func usageCachePath() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".usage-cache.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".usage-cache.json"
	}
	return filepath.Join(home, ".claude", ".usage-cache.json")
}

// formatTTL renders time remaining as two units, compact.
// Past/zero → "0s"; sub-minute → "Ns"; sub-hour → "NmNs"; sub-day →
// "NhNm"; day+ → "NdNh".
func formatTTL(expires, now time.Time) string {
	if expires.IsZero() {
		return "0s"
	}
	d := expires.Sub(now)
	if d <= 0 {
		return "0s"
	}
	totalSec := int(d.Round(time.Second) / time.Second)
	days := totalSec / 86400
	hours := (totalSec % 86400) / 3600
	mins := (totalSec % 3600) / 60
	secs := totalSec % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm%ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// modelContextLimit maps a claude model name to its context window size.
// Any name containing "1m" (case-insensitive) gets the 1M tier; anything
// else defaults to 200k. Matches the model aliases registered in
// claudeProvider.ModelPicker() ("opus[1m]", "sonnet[1m]") as well as the
// fully-qualified names claude returns in its system/init event.
func modelContextLimit(model string) int {
	if strings.Contains(strings.ToLower(model), "1m") {
		return 1_000_000
	}
	return 200_000
}

// contextPercent returns an integer percent in [0, 100]. Returns 0 when
// limit is non-positive (guards divide-by-zero if the model limit is
// unknown early in a session).
func contextPercent(used, limit int) int {
	if limit <= 0 {
		return 0
	}
	p := used * 100 / limit
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// assistantUsage pulls the current turn's context size out of an
// assistant event's message.usage block. The returned int is
// input_tokens + cache_read_input_tokens + cache_creation_input_tokens —
// together these cover the entire transcript the model just processed.
// output_tokens is deliberately excluded: those are what the model just
// wrote, not what it read.
func assistantUsage(ev map[string]any) (int, bool) {
	msg, _ := ev["message"].(map[string]any)
	usage, ok := msg["usage"].(map[string]any)
	if !ok {
		return 0, false
	}
	input := jsonInt(usage["input_tokens"])
	cacheRead := jsonInt(usage["cache_read_input_tokens"])
	cacheCreate := jsonInt(usage["cache_creation_input_tokens"])
	return input + cacheRead + cacheCreate, true
}
