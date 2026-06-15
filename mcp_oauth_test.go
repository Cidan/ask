package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

func TestMCPOAuthTokenPathAndRoundTrip(t *testing.T) {
	home := isolateHome(t)
	path, err := mcpOAuthTokenPath("https://mcp.example.com/api/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, home) || !strings.Contains(path, "mcp.example.com-") {
		t.Errorf("token path %q must live under home and carry the host", path)
	}
	// Different URLs on the same host must not collide.
	other, _ := mcpOAuthTokenPath("https://mcp.example.com/other")
	if other == path {
		t.Error("distinct URLs must map to distinct token files")
	}

	tok := &oauth2.Token{AccessToken: "at", RefreshToken: "rt",
		Expiry: time.Now().Add(time.Hour)}
	if err := saveMCPOAuthToken(path, tok); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v want 0600", info.Mode().Perm())
	}
	got, err := loadMCPOAuthToken(path)
	if err != nil || got.AccessToken != "at" || got.RefreshToken != "rt" {
		t.Fatalf("token round-trip: %+v %v", got, err)
	}
}

type scriptedTokenSource struct {
	tokens []*oauth2.Token
	idx    int
}

func (s *scriptedTokenSource) Token() (*oauth2.Token, error) {
	if s.idx >= len(s.tokens) {
		return nil, errors.New("out of tokens")
	}
	t := s.tokens[s.idx]
	s.idx++
	return t, nil
}

func TestPersistingTokenSource_SavesOnChange(t *testing.T) {
	isolateHome(t)
	path, _ := mcpOAuthTokenPath("https://persist.test/mcp")
	src := &persistingTokenSource{
		inner: &scriptedTokenSource{tokens: []*oauth2.Token{
			{AccessToken: "one", Expiry: time.Now().Add(time.Hour)},
			{AccessToken: "one", Expiry: time.Now().Add(time.Hour)},
			{AccessToken: "two", Expiry: time.Now().Add(2 * time.Hour)},
		}},
		path: path,
	}
	for range 3 {
		if _, err := src.Token(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := loadMCPOAuthToken(path)
	if err != nil || got.AccessToken != "two" {
		t.Fatalf("last token must be persisted: %+v %v", got, err)
	}
}

func TestMCPOAuthCallback_CapturesCode(t *testing.T) {
	cb, err := newMCPOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.close()
	if !strings.HasPrefix(cb.url, "http://127.0.0.1:") || !strings.HasSuffix(cb.url, "/callback") {
		t.Fatalf("callback url %q", cb.url)
	}

	opened := ""
	prev := mcpOAuthOpenBrowser
	mcpOAuthOpenBrowser = func(u string) error {
		opened = u
		// Simulate the user finishing the flow: the AS redirects the
		// browser to our callback.
		go func() {
			resp, err := http.Get(cb.url + "?code=abc123&state=xyz")
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { mcpOAuthOpenBrowser = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cb.fetch(ctx, &auth.AuthorizationArgs{URL: "https://as.example/authorize"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != "abc123" || res.State != "xyz" {
		t.Errorf("captured %+v", res)
	}
	if opened != "https://as.example/authorize" {
		t.Errorf("browser must open the authorization URL, got %q", opened)
	}
}

func TestMCPOAuthCallback_MissingCodeRejected(t *testing.T) {
	cb, err := newMCPOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.close()
	resp, err := http.Get(cb.url + "?state=only")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing code must 400, got %d", resp.StatusCode)
	}
}

func TestAskMCPOAuthHandler_ServesStoredToken(t *testing.T) {
	isolateHome(t)
	serverURL := "https://stored.test/mcp"
	path, _ := mcpOAuthTokenPath(serverURL)
	if err := saveMCPOAuthToken(path, &oauth2.Token{
		AccessToken: "stored-token", Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	h, err := newMCPOAuthHandler(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	src, err := h.TokenSource(context.Background())
	if err != nil || src == nil {
		t.Fatalf("stored token must produce a source: %v", err)
	}
	tok, err := src.Token()
	if err != nil || tok.AccessToken != "stored-token" {
		t.Fatalf("stored token must be served without a browser flow: %+v %v", tok, err)
	}
}

// TestMCPOAuthTokenPath_BadURLFallsBackToServer: an unparseable
// server URL → host defaults to "server" (rather than panicking
// or producing an empty host). Same-shape URL hash so the file
// is still namespaced under .config/ask/mcp-oauth/.
func TestMCPOAuthTokenPath_BadURLFallsBackToServer(t *testing.T) {
	isolateHome(t)
	path, err := mcpOAuthTokenPath("://not a url")
	if err != nil {
		t.Fatalf("mcpOAuthTokenPath: %v", err)
	}
	if !strings.Contains(path, "server-") {
		t.Errorf("path should fall back to 'server-' prefix; got %q", path)
	}
	if !strings.HasSuffix(path, ".json") {
		t.Errorf("path should end in .json; got %q", path)
	}
}

// TestMCPOAuthTokenPath_SameURLSameHash: deterministic — two
// calls with the same URL produce the same path. Important
// because loadMCPOAuthToken is keyed off this path.
func TestMCPOAuthTokenPath_SameURLSameHash(t *testing.T) {
	isolateHome(t)
	a, _ := mcpOAuthTokenPath("https://api.example.com/mcp")
	b, _ := mcpOAuthTokenPath("https://api.example.com/mcp")
	if a != b {
		t.Errorf("same URL should produce same path; got %q vs %q", a, b)
	}
	c, _ := mcpOAuthTokenPath("https://other.example.com/mcp")
	if a == c {
		t.Errorf("different URLs should produce different paths; got %q == %q", a, c)
	}
}

// TestSaveMCPOAuthToken_NilIsNoOp: a nil token must be silently
// dropped — no file created, no error returned. Defends against
// a torn-down token round-trip where the inner Token() races to
// nil before save runs.
func TestSaveMCPOAuthToken_NilIsNoOp(t *testing.T) {
	isolateHome(t)
	dir := filepath.Join(t.TempDir(), "sub", "deep")
	path := filepath.Join(dir, "tok.json")
	if err := saveMCPOAuthToken(path, nil); err != nil {
		t.Errorf("save nil: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("no file should be created for nil token; stat err=%v", err)
	}
}

func TestAskMCPOAuthHandler_NoTokenMeansNilSource(t *testing.T) {
	isolateHome(t)
	h, err := newMCPOAuthHandler("https://fresh.test/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	src, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if src != nil {
		tok, _ := src.Token()
		t.Errorf("fresh handler must report no source (got token %+v) so the transport 401s into Authorize", tok)
	}
}
