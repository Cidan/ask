package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeAuthServer is an MCP server behind bearer tokens and the authorization server that issues
// them, on one loopback address: protected-resource metadata (RFC 9728), server metadata advertising
// RFC 9207's iss parameter, dynamic registration, an authorization endpoint that redirects straight
// back with a code, state, and iss, and a token endpoint that checks PKCE.
type fakeAuthServer struct {
	srv *httptest.Server

	mu        sync.Mutex
	challenge string // the PKCE challenge of the last authorization
	redirect  string
}

const fakeAccessToken = "fake-access-token"

func newFakeAuthServer(t *testing.T) *fakeAuthServer {
	t.Helper()
	f := &fakeAuthServer{}
	server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "v1"}, nil)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	mux := http.NewServeMux()
	issuer := func() string { return f.srv.URL }
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeAccessToken {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+issuer()+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": issuer() + "/mcp", "authorization_servers": []string{issuer()}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer(), "authorization_endpoint": issuer() + "/authorize", "token_endpoint": issuer() + "/token",
			"registration_endpoint": issuer() + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"},
			"authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var reg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reg)
		reg["client_id"] = "registered-client"
		reg["token_endpoint_auth_method"] = "none"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reg)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.challenge, f.redirect = q.Get("code_challenge"), q.Get("redirect_uri")
		f.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := back.Query()
		bq.Set("code", "the-code")
		bq.Set("state", q.Get("state"))
		bq.Set("iss", issuer())
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		f.mu.Lock()
		ok := r.PostForm.Get("code") == "the-code" && base64.RawURLEncoding.EncodeToString(sum[:]) == f.challenge &&
			r.PostForm.Get("redirect_uri") == f.redirect
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fakeAccessToken, "token_type": "Bearer", "expires_in": 3600, "refresh_token": "fake-refresh"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// An authorization server that returns iss with the code (RFC 9207) and advertises it authorizes
// ask end to end: the loopback callback carries the issuer to the SDK, which otherwise refuses the
// response.
func TestAuthorizeMCPServer_IssuerInResponse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newFakeAuthServer(t)
	prev := MCPOAuthOpenBrowser
	MCPOAuthOpenBrowser = func(authURL string) error {
		go func() {
			if !strings.HasPrefix(authURL, f.srv.URL+"/authorize?") {
				t.Errorf("opened %q", authURL)
				return
			}
			for i := 0; i < 50; i++ {
				resp, err := http.Get(authURL)
				if err == nil {
					_ = resp.Body.Close()
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		return nil
	}
	t.Cleanup(func() { MCPOAuthOpenBrowser = prev })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := MCPServer{Name: "fake", Cfg: MCPServerConfig{URL: f.srv.URL + "/mcp"}}
	if err := AuthorizeMCPServer(ctx, srv, nil); err != nil {
		t.Fatalf("AuthorizeMCPServer: %v", err)
	}
	if !MCPServerAuthorized(srv.Cfg.URL) {
		t.Fatal("no token was stored")
	}
}
