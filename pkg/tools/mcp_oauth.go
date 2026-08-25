package tools

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
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const MCPOAuthFlowTimeout = 3 * time.Minute

// ErrMCPInteractiveAuthRequired is returned by a non-interactive handler's
// Authorize when a server demands OAuth. Startup (background) connections use
// it to mark a server "needs auth" instead of opening a browser and blocking.
var ErrMCPInteractiveAuthRequired = errors.New("mcp: interactive OAuth authorization required")

var MCPOAuthOpenBrowser = func(authURL string) error {
	return exec.Command("xdg-open", authURL).Start()
}

func MCPOAuthTokenPath(serverURL string) (string, error) {
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

// savedMCPToken is the on-disk OAuth state for one MCP server: the tokens
// plus the resolved client registration and endpoints, so a later start can
// refresh silently (headless) without another browser round-trip. Its JSON
// field names for the token fields match oauth2.Token's tags, so an older
// token file that only round-tripped an oauth2.Token still loads here.
type savedMCPToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"client_secret,omitempty"`
	AuthURL      string    `json:"auth_url,omitempty"`
	TokenURL     string    `json:"token_url,omitempty"`
	AuthStyle    int       `json:"auth_style,omitempty"`
}

func (s *savedMCPToken) token() *oauth2.Token {
	if s == nil || s.AccessToken == "" {
		return nil
	}
	return &oauth2.Token{
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		TokenType:    s.TokenType,
		Expiry:       s.Expiry,
	}
}

func (s *savedMCPToken) oauthConfig() *oauth2.Config {
	if s == nil || s.ClientID == "" || s.TokenURL == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     s.ClientID,
		ClientSecret: s.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   s.AuthURL,
			TokenURL:  s.TokenURL,
			AuthStyle: oauth2.AuthStyle(s.AuthStyle),
		},
	}
}

func savedFromToken(cfg *oauth2.Config, tok *oauth2.Token) *savedMCPToken {
	out := &savedMCPToken{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
	}
	if cfg != nil {
		out.ClientID = cfg.ClientID
		out.ClientSecret = cfg.ClientSecret
		out.AuthURL = cfg.Endpoint.AuthURL
		out.TokenURL = cfg.Endpoint.TokenURL
		out.AuthStyle = int(cfg.Endpoint.AuthStyle)
	}
	return out
}

func loadSavedMCPToken(path string) (*savedMCPToken, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t savedMCPToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func saveSavedMCPToken(path string, t *savedMCPToken) error {
	if t == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(t)
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

// LoadMCPOAuthToken reads the stored access/refresh token for path.
func LoadMCPOAuthToken(path string) (*oauth2.Token, error) {
	t, err := loadSavedMCPToken(path)
	if err != nil {
		return nil, err
	}
	tok := t.token()
	if tok == nil {
		return nil, errors.New("no token stored")
	}
	return tok, nil
}

// SaveMCPOAuthToken persists an access/refresh token (no client registration).
func SaveMCPOAuthToken(path string, tok *oauth2.Token) error {
	if tok == nil {
		return nil
	}
	return saveSavedMCPToken(path, savedFromToken(nil, tok))
}

// MCPServerAuthorized reports whether a valid (unexpired) OAuth token is
// stored on disk for serverURL.
func MCPServerAuthorized(serverURL string) bool {
	path, err := MCPOAuthTokenPath(serverURL)
	if err != nil {
		return false
	}
	t, err := loadSavedMCPToken(path)
	if err != nil || t == nil {
		return false
	}
	tok := t.token()
	return tok != nil && tok.Valid()
}

// ForgetMCPServerAuth deletes the stored OAuth token for serverURL (sign out).
func ForgetMCPServerAuth(serverURL string) error {
	path, err := MCPOAuthTokenPath(serverURL)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PersistingTokenSource wraps a token source and writes the token to disk on
// change. Retained for callers/tests that persist a plain access/refresh
// token without a captured client registration.
type PersistingTokenSource struct {
	inner oauth2.TokenSource
	path  string
	mu    sync.Mutex
	last  string
}

func (p *PersistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil || tok == nil {
		return tok, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.AccessToken != p.last {
		p.last = tok.AccessToken
		_ = saveSavedMCPToken(p.path, savedFromToken(nil, tok))
	}
	return tok, nil
}

// persistingClientTokenSource wraps a token source and persists the token
// together with its client registration + endpoints (from cfg) on change,
// so refreshes stay headless across restarts.
type persistingClientTokenSource struct {
	inner oauth2.TokenSource
	cfg   *oauth2.Config
	path  string
	mu    sync.Mutex
	last  string
}

func (p *persistingClientTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil || tok == nil {
		return tok, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.AccessToken != p.last {
		p.last = tok.AccessToken
		_ = saveSavedMCPToken(p.path, savedFromToken(p.cfg, tok))
	}
	return tok, nil
}

// MCPOAuthCallback is the eager loopback receiver used directly by tests and
// as a simple one-shot capture. The handler uses a lazy variant so
// auto-attaching to many servers never holds a listener per server.
type MCPOAuthCallback struct {
	listener net.Listener
	URL      string
	result   chan *auth.AuthorizationResult
	once     sync.Once
}

func NewMCPOAuthCallback() (*MCPOAuthCallback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	cb := &MCPOAuthCallback{
		listener: ln,
		URL:      fmt.Sprintf("http://%s/callback", ln.Addr().String()),
		result:   make(chan *auth.AuthorizationResult, 1),
	}
	srv := &http.Server{Handler: http.HandlerFunc(cb.handle)}
	go func() { _ = srv.Serve(ln) }()
	return cb, nil
}

func (cb *MCPOAuthCallback) handle(w http.ResponseWriter, r *http.Request) {
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

func (cb *MCPOAuthCallback) Close() { _ = cb.listener.Close() }

func (cb *MCPOAuthCallback) Fetch(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	if err := MCPOAuthOpenBrowser(args.URL); err != nil {
		return nil, fmt.Errorf("open browser: %w", err)
	}
	timer := time.NewTimer(MCPOAuthFlowTimeout)
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

// lazyOAuthCallback resolves (but does not hold) a loopback port at
// construction and binds the listener only while an interactive
// authorization is actually running.
type lazyOAuthCallback struct {
	port int
	url  string

	mu     sync.Mutex
	server *http.Server
	result chan *auth.AuthorizationResult
	once   sync.Once
}

func newLazyOAuthCallback() (*lazyOAuthCallback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return &lazyOAuthCallback{
		port: port,
		url:  fmt.Sprintf("http://127.0.0.1:%d/callback", port),
	}, nil
}

func (cb *lazyOAuthCallback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code, state := q.Get("code"), q.Get("state")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<html><body><p>ask is authorized — you can close this tab.</p></body></html>"))
	cb.mu.Lock()
	res := cb.result
	cb.mu.Unlock()
	cb.once.Do(func() {
		if res != nil {
			res <- &auth.AuthorizationResult{Code: code, State: state}
		}
	})
}

func (cb *lazyOAuthCallback) bind() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.server != nil {
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cb.port))
	if err != nil {
		return fmt.Errorf("bind OAuth callback port %d: %w", cb.port, err)
	}
	cb.result = make(chan *auth.AuthorizationResult, 1)
	cb.once = sync.Once{}
	srv := &http.Server{Handler: http.HandlerFunc(cb.handle)}
	cb.server = srv
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (cb *lazyOAuthCallback) release() {
	cb.mu.Lock()
	srv := cb.server
	cb.server = nil
	cb.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

func (cb *lazyOAuthCallback) Fetch(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	if err := cb.bind(); err != nil {
		return nil, err
	}
	defer cb.release()
	if err := MCPOAuthOpenBrowser(args.URL); err != nil {
		return nil, fmt.Errorf("open browser: %w", err)
	}
	cb.mu.Lock()
	res := cb.result
	cb.mu.Unlock()
	timer := time.NewTimer(MCPOAuthFlowTimeout)
	defer timer.Stop()
	select {
	case r := <-res:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("timed out waiting for the browser authorization")
	}
}

// MCPOAuthHandler implements auth.OAuthHandler for an http/sse MCP server:
// SDK authorization-code + PKCE + dynamic client registration, with the
// resolved client registration and token persisted for headless refresh. A
// non-interactive handler (startup) returns ErrMCPInteractiveAuthRequired on
// a 401 instead of opening a browser, so a missing token surfaces as a
// needs-auth state; the user triggers the interactive flow explicitly.
type MCPOAuthHandler struct {
	serverURL   string
	tokenPath   string
	interactive bool
	inner       *auth.AuthorizationCodeHandler
	callback    *lazyOAuthCallback
	redirect    string
}

// NewMCPOAuthHandler builds an OAuth handler for an http/sse MCP server.
// interactive=false makes Authorize return ErrMCPInteractiveAuthRequired
// instead of opening a browser. A saved token (with its client registration)
// is restored so refreshes and restarts stay headless.
func NewMCPOAuthHandler(serverURL string, interactive bool) (*MCPOAuthHandler, error) {
	tokenPath, err := MCPOAuthTokenPath(serverURL)
	if err != nil {
		return nil, err
	}
	cb, err := newLazyOAuthCallback()
	if err != nil {
		return nil, err
	}
	h := &MCPOAuthHandler{
		serverURL:   serverURL,
		tokenPath:   tokenPath,
		interactive: interactive,
		callback:    cb,
		redirect:    cb.url,
	}

	saved, _ := loadSavedMCPToken(tokenPath)

	newTokenSource := func(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		_ = saveSavedMCPToken(tokenPath, savedFromToken(cfg, tok))
		base := cfg.TokenSource(ctx, tok)
		return &persistingClientTokenSource{inner: base, cfg: cfg, path: tokenPath}, nil
	}

	acCfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              h.redirect,
		AuthorizationCodeFetcher: h.fetchCode,
		RequestRefreshToken:      true,
		NewTokenSource:           newTokenSource,
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   "ask",
				RedirectURIs: []string{h.redirect},
				GrantTypes:   []string{"authorization_code", "refresh_token"},
			},
		},
	}
	if saved != nil && saved.ClientID != "" {
		acCfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: saved.ClientID}
		if saved.ClientSecret != "" {
			acCfg.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: saved.ClientSecret}
		}
	}
	if oc := saved.oauthConfig(); oc != nil {
		if tok := saved.token(); tok != nil {
			base := oc.TokenSource(context.Background(), tok)
			acCfg.InitialTokenSource = &persistingClientTokenSource{inner: base, cfg: oc, path: tokenPath}
		}
	}

	inner, err := auth.NewAuthorizationCodeHandler(acCfg)
	if err != nil {
		return nil, err
	}
	h.inner = inner
	return h, nil
}

func (h *MCPOAuthHandler) fetchCode(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	return h.callback.Fetch(ctx, args)
}

func (h *MCPOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if src, err := h.inner.TokenSource(ctx); err == nil && src != nil {
		return src, nil
	}
	if saved, err := loadSavedMCPToken(h.tokenPath); err == nil {
		if tok := saved.token(); tok != nil && tok.Valid() {
			return oauth2.StaticTokenSource(tok), nil
		}
	}
	return nil, nil
}

func (h *MCPOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if !h.interactive {
		return ErrMCPInteractiveAuthRequired
	}
	return h.inner.Authorize(ctx, req, resp)
}

func (h *MCPOAuthHandler) Close() {
	if h.callback != nil {
		h.callback.release()
	}
}

// AuthorizeMCPServer runs the interactive OAuth flow for one server by making
// a single connection that forces the 401 -> discovery -> DCR -> browser ->
// token-exchange path, then persisting the token. Used by the Ctrl+S
// browser's "authorize" action; it does not require a live session.
func AuthorizeMCPServer(ctx context.Context, srv MCPServer) error {
	if srv.Cfg.EffectiveType() == MCPServerTypeStdio {
		return fmt.Errorf("%s: stdio servers do not use OAuth", srv.Name)
	}
	if srv.Cfg.URL == "" {
		return fmt.Errorf("%s: no URL to authorize", srv.Name)
	}
	oauth, err := NewMCPOAuthHandler(srv.Cfg.URL, true)
	if err != nil {
		return err
	}
	defer oauth.Close()
	transport, err := MCPTransportFor(srv, oauth)
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ask-agent", Version: "1.0.0"}, nil)
	cctx, cancel := context.WithTimeout(ctx, MCPOAuthFlowTimeout+MCPConnectTimeout)
	defer cancel()
	session, err := client.Connect(cctx, transport, nil)
	if err != nil {
		return err
	}
	_ = session.Close()
	if !MCPServerAuthorized(srv.Cfg.URL) {
		return fmt.Errorf("%s: authorization did not yield a token", srv.Name)
	}
	return nil
}
