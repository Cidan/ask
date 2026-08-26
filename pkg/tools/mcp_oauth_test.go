package tools

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
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := MCPOAuthTokenPath("https://mcp.example.com/api/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, home) || !strings.Contains(path, "mcp.example.com-") {
		t.Errorf("token path %q must live under home and carry the host", path)
	}
	other, _ := MCPOAuthTokenPath("https://mcp.example.com/other")
	if other == path {
		t.Error("distinct URLs must map to distinct token files")
	}

	tok := &oauth2.Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}
	if err := SaveMCPOAuthToken(path, tok); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v want 0600", info.Mode().Perm())
	}
	got, err := LoadMCPOAuthToken(path)
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := MCPOAuthTokenPath("https://persist.test/mcp")
	src := &PersistingTokenSource{
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
	got, err := LoadMCPOAuthToken(path)
	if err != nil || got.AccessToken != "two" {
		t.Fatalf("last token must be persisted: %+v %v", got, err)
	}
}

func TestMCPOAuthCallback_CapturesCode(t *testing.T) {
	cb, err := NewMCPOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	if !strings.HasPrefix(cb.URL, "http://127.0.0.1:") || !strings.HasSuffix(cb.URL, "/callback") {
		t.Fatalf("callback url %q", cb.URL)
	}

	opened := ""
	prev := MCPOAuthOpenBrowser
	MCPOAuthOpenBrowser = func(u string) error {
		opened = u
		go func() {
			resp, err := http.Get(cb.URL + "?code=abc123&state=xyz")
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	t.Cleanup(func() { MCPOAuthOpenBrowser = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cb.Fetch(ctx, &auth.AuthorizationArgs{URL: "https://as.example/authorize"})
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
	cb, err := NewMCPOAuthCallback()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	resp, err := http.Get(cb.URL + "?state=only")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing code must 400, got %d", resp.StatusCode)
	}
}

func TestMCPOAuthHandler_ServesStoredToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	serverURL := "https://stored.test/mcp"
	path, _ := MCPOAuthTokenPath(serverURL)
	if err := SaveMCPOAuthToken(path, &oauth2.Token{
		AccessToken: "stored-token", Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	h, err := NewMCPOAuthHandler(serverURL, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	src, err := h.TokenSource(context.Background())
	if err != nil || src == nil {
		t.Fatalf("stored token must produce a source: %v", err)
	}
	tok, err := src.Token()
	if err != nil || tok.AccessToken != "stored-token" {
		t.Fatalf("stored token must be served: %+v %v", tok, err)
	}
}

func TestMCPOAuthTokenPath_BadURLFallsBackToServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := MCPOAuthTokenPath("://not a url")
	if err != nil {
		t.Fatalf("MCPOAuthTokenPath: %v", err)
	}
	if !strings.Contains(path, "server-") {
		t.Errorf("path should fall back to 'server-' prefix; got %q", path)
	}
	if !strings.HasSuffix(path, ".json") {
		t.Errorf("path should end in .json; got %q", path)
	}
}

func TestSaveMCPOAuthToken_NilIsNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(t.TempDir(), "sub", "deep")
	path := filepath.Join(dir, "tok.json")
	if err := SaveMCPOAuthToken(path, nil); err != nil {
		t.Errorf("save nil: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("no file should be created for nil token; stat err=%v", err)
	}
}
