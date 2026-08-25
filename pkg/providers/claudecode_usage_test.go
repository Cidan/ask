package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resetClaudeUsageCache clears the package-global snapshot so each test starts
// from a cold cache. Lives in the test file so no reset hook ships in
// production.
func resetClaudeUsageCache() {
	claudeUsageCache.mu.Lock()
	defer claudeUsageCache.mu.Unlock()
	claudeUsageCache.snap = nil
	claudeUsageCache.ok = false
	claudeUsageCache.lastErr = nil
	claudeUsageCache.fetchedAt = time.Time{}
}

const sampleUsageJSON = `{
  "five_hour":        { "utilization": 33.0, "resets_at": "2026-04-11T07:00:00.528743+00:00" },
  "seven_day":        { "utilization": 13.0, "resets_at": "2026-04-17T00:59:59.951713+00:00" },
  "seven_day_opus":   null,
  "seven_day_sonnet": { "utilization": 1.0,  "resets_at": "2026-04-16T03:00:00Z" },
  "extra_usage":      { "is_enabled": true, "monthly_limit": 100.0, "used_credits": 12.5, "utilization": null }
}`

func TestClaudeCodeUsage_ParsesWindowsAndExtra(t *testing.T) {
	resetClaudeUsageCache()
	t.Cleanup(resetClaudeUsageCache)

	prevTok, prevFetch := claudeUsageTokenFn, claudeUsageFetchFn
	t.Cleanup(func() { claudeUsageTokenFn, claudeUsageFetchFn = prevTok, prevFetch })

	claudeUsageTokenFn = func() (string, error) { return "tok", nil }
	claudeUsageFetchFn = func(context.Context, string) ([]byte, error) {
		return []byte(sampleUsageJSON), nil
	}

	u, err := ClaudeCodeUsage(context.Background())
	if err != nil {
		t.Fatalf("ClaudeCodeUsage: %v", err)
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 33.0 {
		t.Fatalf("five_hour = %+v, want 33", u.FiveHour)
	}
	wantReset := time.Date(2026, 4, 11, 7, 0, 0, 528743000, time.UTC)
	if u.FiveHour.ResetsAt.UTC() != wantReset {
		t.Errorf("five_hour resets_at = %v, want %v", u.FiveHour.ResetsAt.UTC(), wantReset)
	}
	if u.SevenDay == nil || u.SevenDay.Utilization != 13.0 {
		t.Errorf("seven_day = %+v, want 13", u.SevenDay)
	}
	if u.SevenDayOpus != nil {
		t.Errorf("seven_day_opus = %+v, want nil (null in payload)", u.SevenDayOpus)
	}
	if u.SevenDaySonnet == nil || u.SevenDaySonnet.Utilization != 1.0 {
		t.Errorf("seven_day_sonnet = %+v, want 1", u.SevenDaySonnet)
	}
	if !u.Extra.IsEnabled || !u.Extra.HasLimit || u.Extra.MonthlyLimit != 100.0 || u.Extra.UsedCredits != 12.5 {
		t.Errorf("extra = %+v, want enabled 12.5/100", u.Extra)
	}
	if u.FetchedAt.IsZero() {
		t.Error("FetchedAt not set")
	}
}

func TestClaudeCodeUsage_ParsesWindowDollars(t *testing.T) {
	resetClaudeUsageCache()
	t.Cleanup(resetClaudeUsageCache)

	prevTok, prevFetch := claudeUsageTokenFn, claudeUsageFetchFn
	t.Cleanup(func() { claudeUsageTokenFn, claudeUsageFetchFn = prevTok, prevFetch })

	const dollarJSON = `{
	  "five_hour": { "utilization": 42.0, "resets_at": "2026-08-25T03:59:59Z",
	                 "used_dollars": 18.5, "limit_dollars": 100.0, "remaining_dollars": 81.5 },
	  "seven_day": { "utilization": 20.0, "resets_at": "2026-08-30T00:00:00Z",
	                 "used_dollars": null, "limit_dollars": null, "remaining_dollars": null }
	}`
	claudeUsageTokenFn = func() (string, error) { return "tok", nil }
	claudeUsageFetchFn = func(context.Context, string) ([]byte, error) { return []byte(dollarJSON), nil }

	u, err := ClaudeCodeUsage(context.Background())
	if err != nil {
		t.Fatalf("ClaudeCodeUsage: %v", err)
	}
	if u.FiveHour == nil || !u.FiveHour.HasDollars {
		t.Fatalf("five_hour dollars = %+v, want HasDollars", u.FiveHour)
	}
	if u.FiveHour.LimitDollars != 100.0 || u.FiveHour.UsedDollars != 18.5 || u.FiveHour.RemainingDollars != 81.5 {
		t.Errorf("five_hour dollar view = %+v, want 18.5/100/81.5", u.FiveHour)
	}
	if u.SevenDay == nil || u.SevenDay.HasDollars {
		t.Errorf("seven_day should have no dollar view (nulls), got %+v", u.SevenDay)
	}
}

func TestClaudeCodeUsage_CachesWithinTTL(t *testing.T) {
	resetClaudeUsageCache()
	t.Cleanup(resetClaudeUsageCache)

	prevTok, prevFetch := claudeUsageTokenFn, claudeUsageFetchFn
	t.Cleanup(func() { claudeUsageTokenFn, claudeUsageFetchFn = prevTok, prevFetch })

	calls := 0
	claudeUsageTokenFn = func() (string, error) { return "tok", nil }
	claudeUsageFetchFn = func(context.Context, string) ([]byte, error) {
		calls++
		return []byte(sampleUsageJSON), nil
	}

	for i := 0; i < 3; i++ {
		if _, err := ClaudeCodeUsage(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("fetch called %d times, want 1 (cached within TTL)", calls)
	}
}

func TestClaudeCodeUsage_ErrorKeepsLastGoodSnapshot(t *testing.T) {
	resetClaudeUsageCache()
	t.Cleanup(resetClaudeUsageCache)

	prevTok, prevFetch := claudeUsageTokenFn, claudeUsageFetchFn
	t.Cleanup(func() { claudeUsageTokenFn, claudeUsageFetchFn = prevTok, prevFetch })

	claudeUsageTokenFn = func() (string, error) { return "tok", nil }
	claudeUsageFetchFn = func(context.Context, string) ([]byte, error) {
		return []byte(sampleUsageJSON), nil
	}
	if _, err := ClaudeCodeUsage(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Force the TTL to elapse, then fail the next fetch.
	claudeUsageCache.mu.Lock()
	claudeUsageCache.fetchedAt = time.Now().Add(-2 * claudeUsageTTL)
	claudeUsageCache.mu.Unlock()
	claudeUsageFetchFn = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("429")
	}

	u, err := ClaudeCodeUsage(context.Background())
	if err == nil {
		t.Fatal("expected error from failing fetch")
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 33.0 {
		t.Errorf("expected last good snapshot on error, got %+v", u.FiveHour)
	}
	if _, ok := CachedClaudeUsage(); !ok {
		t.Error("CachedClaudeUsage should still report the last good snapshot")
	}
}

func TestClaudeCodeUsage_NoTokenReturnsError(t *testing.T) {
	resetClaudeUsageCache()
	t.Cleanup(resetClaudeUsageCache)

	prevTok, prevFetch := claudeUsageTokenFn, claudeUsageFetchFn
	t.Cleanup(func() { claudeUsageTokenFn, claudeUsageFetchFn = prevTok, prevFetch })

	fetched := false
	claudeUsageTokenFn = func() (string, error) { return "", errNoClaudeToken }
	claudeUsageFetchFn = func(context.Context, string) ([]byte, error) {
		fetched = true
		return nil, nil
	}

	if _, err := ClaudeCodeUsage(context.Background()); err == nil {
		t.Fatal("expected error when no token resolves")
	}
	if fetched {
		t.Error("fetch must not run without a token")
	}
	if _, ok := CachedClaudeUsage(); ok {
		t.Error("CachedClaudeUsage should be empty when nothing ever succeeded")
	}
}

func TestDefaultClaudeUsageToken_EnvWins(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "env-token")
	got, err := defaultClaudeUsageToken()
	if err != nil || got != "env-token" {
		t.Fatalf("defaultClaudeUsageToken = %q, %v; want env-token", got, err)
	}
}

func TestDefaultClaudeUsageToken_ReadsCredentialsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	creds := `{"claudeAiOauth":{"accessToken":"file-token","expiresAt":1893456000000}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := defaultClaudeUsageToken()
	if err != nil || got != "file-token" {
		t.Fatalf("defaultClaudeUsageToken = %q, %v; want file-token", got, err)
	}
}
