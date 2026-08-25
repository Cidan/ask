package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestSavedMCPToken_RoundTripWithClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, _ := MCPOAuthTokenPath("https://client.test/mcp")
	in := &savedMCPToken{
		AccessToken:  "at",
		RefreshToken: "rt",
		Expiry:       time.Now().Add(time.Hour),
		ClientID:     "cid",
		ClientSecret: "secret",
		AuthURL:      "https://as/authorize",
		TokenURL:     "https://as/token",
		AuthStyle:    int(oauth2.AuthStyleInParams),
	}
	if err := saveSavedMCPToken(path, in); err != nil {
		t.Fatal(err)
	}
	got, err := loadSavedMCPToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "cid" || got.ClientSecret != "secret" || got.TokenURL != "https://as/token" {
		t.Fatalf("client registration must persist: %+v", got)
	}
	if oc := got.oauthConfig(); oc == nil || oc.ClientID != "cid" || oc.Endpoint.TokenURL != "https://as/token" {
		t.Fatalf("oauthConfig: %+v", oc)
	}
}

func TestMCPServerAuthorizedAndForget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	url := "https://auth.test/mcp"
	path, _ := MCPOAuthTokenPath(url)

	if MCPServerAuthorized(url) {
		t.Fatal("no token yet -> not authorized")
	}
	_ = saveSavedMCPToken(path, &savedMCPToken{AccessToken: "at", Expiry: time.Now().Add(time.Hour)})
	if !MCPServerAuthorized(url) {
		t.Fatal("valid token -> authorized")
	}
	_ = saveSavedMCPToken(path, &savedMCPToken{AccessToken: "at", Expiry: time.Now().Add(-time.Hour)})
	if MCPServerAuthorized(url) {
		t.Fatal("expired token -> not authorized")
	}
	_ = saveSavedMCPToken(path, &savedMCPToken{AccessToken: "at", Expiry: time.Now().Add(time.Hour)})
	if err := ForgetMCPServerAuth(url); err != nil {
		t.Fatal(err)
	}
	if MCPServerAuthorized(url) {
		t.Fatal("sign out must remove the token")
	}
}

func TestNewMCPOAuthHandler_NonInteractiveReturnsSentinel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spy := false
	prev := MCPOAuthOpenBrowser
	MCPOAuthOpenBrowser = func(string) error { spy = true; return nil }
	t.Cleanup(func() { MCPOAuthOpenBrowser = prev })

	h, err := NewMCPOAuthHandler("https://needs-auth.test/mcp", false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	req, _ := http.NewRequest(http.MethodGet, "https://needs-auth.test/mcp", nil)
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Www-Authenticate": {"Bearer"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	err = h.Authorize(context.Background(), req, resp)
	if !errors.Is(err, ErrMCPInteractiveAuthRequired) {
		t.Fatalf("non-interactive Authorize must return the sentinel, got %v", err)
	}
	if spy {
		t.Fatal("non-interactive Authorize must not open a browser")
	}
}
