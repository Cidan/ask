package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Claude Code subscription usage limits are not exposed by the headless
// `claude -p` stream ask drives: the CLI's statusLine assembles them from
// cached `anthropic-ratelimit-unified-*` headers, but that only runs in the
// TUI, and the SDK's rate_limit_event omits per-bucket utilization under
// normal load. The community-discovered `GET /api/oauth/usage` endpoint is
// the one path that returns live per-bucket percentages, so ask polls it the
// same way ccusage / the ccstatusline tools do: an OAuth bearer read from the
// same credentials the `claude` CLI maintains, plus the `claude-code/<ver>`
// User-Agent the endpoint requires to avoid an aggressively rate-limited
// bucket. It is undocumented and best-effort — every failure degrades to the
// last good snapshot or to showing nothing.

const (
	claudeUsageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	// claudeUsageBeta is the anthropic-beta header the endpoint expects for
	// OAuth-authenticated calls.
	claudeUsageBeta = "oauth-2025-04-20"
	// claudeUsageTTL is how long a successful snapshot is served before the
	// next network refresh. Matches the community tools' default to stay well
	// clear of the endpoint's rate limiter.
	claudeUsageTTL = 5 * time.Minute
	// claudeUsageErrTTL is the shorter backoff after a failed fetch so a
	// transient error (missing token mid-login, a 429) recovers quickly
	// without hammering.
	claudeUsageErrTTL = 30 * time.Second
)

// claudeUsageUserAgent is sent verbatim as the User-Agent. The endpoint keys
// its generous rate-limit bucket off the `claude-code/` prefix; without it the
// caller lands in an aggressively throttled bucket and gets persistent 429s.
var claudeUsageUserAgent = "claude-code/2.1.0"

// errNoClaudeToken means no OAuth credential could be resolved (not logged in
// to a Claude subscription, or running on an API key). Usage limits simply do
// not apply, so callers treat it as "nothing to show".
var errNoClaudeToken = errors.New("claude-code: no oauth credential found")

// ClaudeUsageWindow is one rate-limit bucket: how much of it is used and when
// it resets. Utilization is a percentage in [0, 100]. On dollar-budgeted
// accounts (enterprise / pay-as-you-go) the same window also carries a dollar
// view — used/limit/remaining — which is nil on plain subscription accounts.
type ClaudeUsageWindow struct {
	Utilization float64
	ResetsAt    time.Time // zero when the endpoint omitted resets_at

	// Dollar view of the window; HasDollars is set only when the endpoint
	// populated limit_dollars (i.e. the account is billed by dollars).
	HasDollars       bool
	UsedDollars      float64
	LimitDollars     float64
	RemainingDollars float64
}

// ClaudeExtraUsage is the pay-as-you-go dollar budget attached to a paid plan
// ("extra usage"). When enabled with a monthly limit it is the account's
// dollar ceiling — the answer to "budget controlled by dollars".
type ClaudeExtraUsage struct {
	IsEnabled    bool
	MonthlyLimit float64
	UsedCredits  float64
	HasLimit     bool // MonthlyLimit was present (non-null) in the response
}

// ClaudeUsage is the parsed /api/oauth/usage response. Each window pointer is
// nil when the endpoint returned null for that bucket (e.g. seven_day_opus for
// a plan without Opus weekly tracking).
type ClaudeUsage struct {
	FiveHour       *ClaudeUsageWindow
	SevenDay       *ClaudeUsageWindow
	SevenDayOpus   *ClaudeUsageWindow
	SevenDaySonnet *ClaudeUsageWindow
	Extra          ClaudeExtraUsage
	FetchedAt      time.Time
}

// claudeUsageCacheT holds the single account-global snapshot. Usage is not
// per-tab, so one cache serves every claude-code tab and one network call
// covers them all within the TTL.
type claudeUsageCacheT struct {
	mu        sync.Mutex
	snap      *ClaudeUsage
	ok        bool
	lastErr   error
	fetchedAt time.Time
}

var claudeUsageCache claudeUsageCacheT

// Seams so tests exercise the parse/cache logic without a token, a subprocess,
// or the network.
var (
	claudeUsageTokenFn = defaultClaudeUsageToken
	claudeUsageFetchFn = defaultClaudeUsageFetch
)

var claudeUsageHTTPClient = &http.Client{Timeout: 10 * time.Second}

// ClaudeCodeUsage returns the current usage snapshot, refreshing from the
// network when the cached one is stale. Concurrent callers serialize on the
// cache mutex, so a burst of tabs refreshing at once produces one network
// call. On any failure the last good snapshot is returned alongside the error
// (the caller keeps showing it); with no snapshot yet, a zero value + error.
func ClaudeCodeUsage(ctx context.Context) (ClaudeUsage, error) {
	claudeUsageCache.mu.Lock()
	defer claudeUsageCache.mu.Unlock()

	if !claudeUsageCache.fetchedAt.IsZero() {
		ttl := claudeUsageTTL
		if claudeUsageCache.lastErr != nil {
			ttl = claudeUsageErrTTL
		}
		if time.Since(claudeUsageCache.fetchedAt) < ttl {
			return claudeUsageCache.snapshotLocked()
		}
	}

	token, err := claudeUsageTokenFn()
	if err == nil {
		var body []byte
		body, err = claudeUsageFetchFn(ctx, token)
		if err == nil {
			var u ClaudeUsage
			u, err = parseClaudeUsage(body)
			if err == nil {
				u.FetchedAt = time.Now()
				claudeUsageCache.snap = &u
				claudeUsageCache.ok = true
				claudeUsageCache.lastErr = nil
				claudeUsageCache.fetchedAt = u.FetchedAt
				return u, nil
			}
		}
	}

	claudeUsageCache.lastErr = err
	claudeUsageCache.fetchedAt = time.Now()
	return claudeUsageCache.snapshotLocked()
}

// snapshotLocked returns the cached snapshot (or a zero value) with the last
// error. Caller holds the mutex.
func (c *claudeUsageCacheT) snapshotLocked() (ClaudeUsage, error) {
	if c.ok && c.snap != nil {
		return *c.snap, c.lastErr
	}
	return ClaudeUsage{}, c.lastErr
}

// CachedClaudeUsage returns the last fetched snapshot without touching the
// network. ok is false until a fetch has succeeded at least once. This is the
// render-path accessor: the TUI reads it every frame while a background
// refresh keeps it current.
func CachedClaudeUsage() (ClaudeUsage, bool) {
	claudeUsageCache.mu.Lock()
	defer claudeUsageCache.mu.Unlock()
	if !claudeUsageCache.ok || claudeUsageCache.snap == nil {
		return ClaudeUsage{}, false
	}
	return *claudeUsageCache.snap, true
}

// claudeUsageWire mirrors the endpoint's JSON. Unknown buckets
// (seven_day_oauth_apps, seven_day_cowork, …) are ignored by the decoder.
type claudeUsageWire struct {
	FiveHour       *claudeWindowWire `json:"five_hour"`
	SevenDay       *claudeWindowWire `json:"seven_day"`
	SevenDayOpus   *claudeWindowWire `json:"seven_day_opus"`
	SevenDaySonnet *claudeWindowWire `json:"seven_day_sonnet"`
	ExtraUsage     *struct {
		IsEnabled    bool     `json:"is_enabled"`
		MonthlyLimit *float64 `json:"monthly_limit"`
		UsedCredits  *float64 `json:"used_credits"`
		Utilization  *float64 `json:"utilization"`
	} `json:"extra_usage"`
}

type claudeWindowWire struct {
	Utilization      float64  `json:"utilization"`
	ResetsAt         string   `json:"resets_at"`
	UsedDollars      *float64 `json:"used_dollars"`
	LimitDollars     *float64 `json:"limit_dollars"`
	RemainingDollars *float64 `json:"remaining_dollars"`
}

func parseClaudeUsage(body []byte) (ClaudeUsage, error) {
	var w claudeUsageWire
	if err := json.Unmarshal(body, &w); err != nil {
		return ClaudeUsage{}, fmt.Errorf("claude-code: decode usage: %w", err)
	}
	u := ClaudeUsage{
		FiveHour:       toUsageWindow(w.FiveHour),
		SevenDay:       toUsageWindow(w.SevenDay),
		SevenDayOpus:   toUsageWindow(w.SevenDayOpus),
		SevenDaySonnet: toUsageWindow(w.SevenDaySonnet),
	}
	if e := w.ExtraUsage; e != nil {
		u.Extra.IsEnabled = e.IsEnabled
		if e.MonthlyLimit != nil {
			u.Extra.MonthlyLimit = *e.MonthlyLimit
			u.Extra.HasLimit = true
		}
		if e.UsedCredits != nil {
			u.Extra.UsedCredits = *e.UsedCredits
		}
	}
	return u, nil
}

func toUsageWindow(w *claudeWindowWire) *ClaudeUsageWindow {
	if w == nil {
		return nil
	}
	out := &ClaudeUsageWindow{Utilization: w.Utilization}
	if s := strings.TrimSpace(w.ResetsAt); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			out.ResetsAt = t
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			out.ResetsAt = t
		}
	}
	if w.LimitDollars != nil {
		out.HasDollars = true
		out.LimitDollars = *w.LimitDollars
	}
	if w.UsedDollars != nil {
		out.UsedDollars = *w.UsedDollars
	}
	if w.RemainingDollars != nil {
		out.RemainingDollars = *w.RemainingDollars
	}
	return out
}

func defaultClaudeUsageFetch(ctx context.Context, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeUsageBeta)
	req.Header.Set("User-Agent", claudeUsageUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := claudeUsageHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude-code: usage endpoint status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// defaultClaudeUsageToken resolves the OAuth bearer: the CLAUDE_CODE_OAUTH_TOKEN
// env (a long-lived `claude setup-token` credential) wins, then the JSON
// credentials file the CLI maintains, then the macOS Keychain.
func defaultClaudeUsageToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")); t != "" {
		return t, nil
	}
	if t := claudeTokenFromFile(); t != "" {
		return t, nil
	}
	if runtime.GOOS == "darwin" {
		if t := claudeTokenFromKeychain(); t != "" {
			return t, nil
		}
	}
	return "", errNoClaudeToken
}

// claudeCredentials is the shape ask reads out of the CLI's credential store.
type claudeCredentials struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

func claudeTokenFromFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return ""
	}
	return tokenFromCredentials(data)
}

func claudeTokenFromKeychain() string {
	out, err := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return ""
	}
	return tokenFromCredentials(out)
}

func tokenFromCredentials(data []byte) string {
	var c claudeCredentials
	if err := json.Unmarshal(data, &c); err != nil {
		return ""
	}
	return strings.TrimSpace(c.ClaudeAiOauth.AccessToken)
}
