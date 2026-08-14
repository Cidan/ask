package main

import (
	"github.com/Cidan/ask/pkg/tools"
	"golang.org/x/oauth2"
)

const mcpOAuthFlowTimeout = tools.MCPOAuthFlowTimeout

var mcpOAuthOpenBrowser = tools.MCPOAuthOpenBrowser

func mcpOAuthTokenPath(serverURL string) (string, error) {
	return tools.MCPOAuthTokenPath(serverURL)
}

func loadMCPOAuthToken(path string) (*oauth2.Token, error) {
	return tools.LoadMCPOAuthToken(path)
}

func saveMCPOAuthToken(path string, tok *oauth2.Token) error {
	return tools.SaveMCPOAuthToken(path, tok)
}

type persistingTokenSource struct {
	inner *tools.PersistingTokenSource
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	return p.inner.Token()
}

type mcpOAuthCallback struct {
	inner *tools.MCPOAuthCallback
	url   string
}

func newMCPOAuthCallback() (*mcpOAuthCallback, error) {
	cb, err := tools.NewMCPOAuthCallback()
	if err != nil {
		return nil, err
	}
	return &mcpOAuthCallback{inner: cb, url: cb.URL}, nil
}

func (cb *mcpOAuthCallback) close() {
	if cb.inner != nil {
		cb.inner.Close()
	}
}

type askMCPOAuthHandler = tools.MCPOAuthHandler

func newMCPOAuthHandler(serverURL string) (*askMCPOAuthHandler, error) {
	return tools.NewMCPOAuthHandler(serverURL)
}
