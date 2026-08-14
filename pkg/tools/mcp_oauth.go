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
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const MCPOAuthFlowTimeout = 3 * time.Minute

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

func LoadMCPOAuthToken(path string) (*oauth2.Token, error) {
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

func SaveMCPOAuthToken(path string, tok *oauth2.Token) error {
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
		_ = SaveMCPOAuthToken(p.path, tok)
	}
	return tok, nil
}

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

type MCPOAuthHandler struct {
	inner     *auth.AuthorizationCodeHandler
	tokenPath string
	callback  *MCPOAuthCallback

	mu     sync.Mutex
	source oauth2.TokenSource
}

func NewMCPOAuthHandler(serverURL string) (*MCPOAuthHandler, error) {
	tokenPath, err := MCPOAuthTokenPath(serverURL)
	if err != nil {
		return nil, err
	}
	cb, err := NewMCPOAuthCallback()
	if err != nil {
		return nil, err
	}
	inner, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   "ask",
				RedirectURIs: []string{cb.URL},
			},
		},
		AuthorizationCodeFetcher: cb.Fetch,
	})
	if err != nil {
		cb.Close()
		return nil, err
	}
	return &MCPOAuthHandler{inner: inner, tokenPath: tokenPath, callback: cb}, nil
}

func (h *MCPOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if src, err := h.inner.TokenSource(ctx); err == nil && src != nil {
		if h.source == nil {
			h.source = &PersistingTokenSource{inner: src, path: h.tokenPath}
		}
		return h.source, nil
	}
	if tok, err := LoadMCPOAuthToken(h.tokenPath); err == nil && tok.Valid() {
		return oauth2.StaticTokenSource(tok), nil
	}
	return nil, nil
}

func (h *MCPOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if err := h.inner.Authorize(ctx, req, resp); err != nil {
		return err
	}
	h.mu.Lock()
	h.source = nil
	h.mu.Unlock()
	if src, err := h.inner.TokenSource(ctx); err == nil && src != nil {
		if tok, err := src.Token(); err == nil {
			_ = SaveMCPOAuthToken(h.tokenPath, tok)
		}
	}
	return nil
}

func (h *MCPOAuthHandler) Close() {
	if h.callback != nil {
		h.callback.Close()
	}
}
