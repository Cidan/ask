package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// mcpOAuthFlowTimeout bounds how long we wait for the user to finish
// the browser authorization before the connect gives up.
const mcpOAuthFlowTimeout = 3 * time.Minute

// mcpOAuthOpenBrowser launches the user's browser at the authorization
// URL. Swappable in tests so the flow can run headless.
var mcpOAuthOpenBrowser = func(authURL string) error {
	return exec.Command("xdg-open", authURL).Start()
}

// mcpOAuthTokenPath maps a server URL onto its token file:
// ~/.config/ask/mcp-oauth/<host>-<urlhash>.json (0600). Keyed by URL,
// not server name — names are user-local and collide across projects.
func mcpOAuthTokenPath(serverURL string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	host := "server"
	if u, err := url.Parse(serverURL); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	sum := sha256.Sum256([]byte(serverURL))
	name := fmt.Sprintf("%s-%s.json", host, hex.EncodeToString(sum[:8]))
	return filepath.Join(home, ".config", "ask", "mcp-oauth", name), nil
}

func loadMCPOAuthToken(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func saveMCPOAuthToken(path string, tok *oauth2.Token) error {
	if tok == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tok-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// persistingTokenSource saves every token it sees (initial grants and
// refreshes both flow through Token()).
type persistingTokenSource struct {
	inner oauth2.TokenSource
	path  string
	mu    sync.Mutex
	last  string
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil || tok == nil {
		return tok, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.AccessToken != p.last {
		p.last = tok.AccessToken
		if err := saveMCPOAuthToken(p.path, tok); err != nil {
			debugLog("mcp oauth: persist token: %v", err)
		}
	}
	return tok, nil
}

// mcpOAuthCallback is a one-shot loopback listener for the
// authorization redirect. It is bound before the handler is built so
// the redirect URL (with its port) is known up front.
type mcpOAuthCallback struct {
	listener net.Listener
	url      string
	result   chan *auth.AuthorizationResult
	once     sync.Once
}

func newMCPOAuthCallback() (*mcpOAuthCallback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	cb := &mcpOAuthCallback{
		listener: ln,
		url:      fmt.Sprintf("http://%s/callback", ln.Addr().String()),
		result:   make(chan *auth.AuthorizationResult, 1),
	}
	srv := &http.Server{Handler: http.HandlerFunc(cb.handle)}
	go func() { _ = srv.Serve(ln) }()
	return cb, nil
}

func (cb *mcpOAuthCallback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code, state := q.Get("code"), q.Get("state")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<html><body><p>ask is authorized — you can close this tab.</p></body></html>"))
	cb.once.Do(func() {
		cb.result <- &auth.AuthorizationResult{Code: code, State: state}
	})
}

func (cb *mcpOAuthCallback) close() { _ = cb.listener.Close() }

// fetch implements auth.AuthorizationCodeFetcher: open the browser and
// wait for the redirect (or give up after mcpOAuthFlowTimeout).
func (cb *mcpOAuthCallback) fetch(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	if err := mcpOAuthOpenBrowser(args.URL); err != nil {
		return nil, fmt.Errorf("open browser: %w", err)
	}
	timer := time.NewTimer(mcpOAuthFlowTimeout)
	defer timer.Stop()
	select {
	case res := <-cb.result:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("timed out waiting for the browser authorization")
	}
}

// askMCPOAuthHandler decorates the SDK's authorization-code handler
// with on-disk token persistence. A stored, still-valid token is
// served without any browser interaction; once it expires the next 401
// re-runs the flow (the SDK keeps the refresh configuration internal,
// so cross-restart refresh is a fresh authorization — acceptable for
// the long-lived tokens MCP servers issue).
type askMCPOAuthHandler struct {
	inner     *auth.AuthorizationCodeHandler
	tokenPath string
	callback  *mcpOAuthCallback

	mu     sync.Mutex
	source oauth2.TokenSource
}

// newMCPOAuthHandler builds the OAuth stack for one server: loopback
// callback listener, dynamic client registration (every compliant
// remote supports DCR; preregistration can be layered into the config
// later if a server demands it), and the persistence wrapper.
func newMCPOAuthHandler(serverURL string) (*askMCPOAuthHandler, error) {
	tokenPath, err := mcpOAuthTokenPath(serverURL)
	if err != nil {
		return nil, err
	}
	cb, err := newMCPOAuthCallback()
	if err != nil {
		return nil, err
	}
	inner, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   "ask",
				RedirectURIs: []string{cb.url},
			},
		},
		AuthorizationCodeFetcher: cb.fetch,
	})
	if err != nil {
		cb.close()
		return nil, err
	}
	return &askMCPOAuthHandler{inner: inner, tokenPath: tokenPath, callback: cb}, nil
}

func (h *askMCPOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if src, err := h.inner.TokenSource(ctx); err == nil && src != nil {
		if h.source == nil {
			h.source = &persistingTokenSource{inner: src, path: h.tokenPath}
		}
		return h.source, nil
	}
	// No in-memory grant yet: serve the stored token while it lives.
	if tok, err := loadMCPOAuthToken(h.tokenPath); err == nil && tok.Valid() {
		return oauth2.StaticTokenSource(tok), nil
	}
	return nil, nil
}

func (h *askMCPOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if err := h.inner.Authorize(ctx, req, resp); err != nil {
		return err
	}
	// A fresh grant landed: reset the wrapper so the next TokenSource
	// call wraps (and persists) the new inner source.
	h.mu.Lock()
	h.source = nil
	h.mu.Unlock()
	if src, err := h.inner.TokenSource(ctx); err == nil && src != nil {
		if tok, err := src.Token(); err == nil {
			if err := saveMCPOAuthToken(h.tokenPath, tok); err != nil {
				debugLog("mcp oauth: persist grant: %v", err)
			}
		}
	}
	return nil
}

func (h *askMCPOAuthHandler) close() {
	if h.callback != nil {
		h.callback.close()
	}
}
