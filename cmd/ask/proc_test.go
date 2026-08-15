package main

import (
	"testing"
)

// projectGitHubMCP must return a wired-up descriptor whenever the
// project has a GitHub MCP token saved — even when the issues
// backend is None. The whole point of decoupling the MCP slot from
// the issues slot is so the chat agent can have GitHub access
// without the user needing to opt in to the issues UI as well.
func TestProjectGitHubMCP_ReturnsDescriptorWhenTokenSetEvenWithIssuesDisabled(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, cwd, projectConfig{
		MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "ghp_secret"}},
		// Issues.Provider deliberately left empty — chat agent should
		// still get the GitHub MCP entry.
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	got := projectGitHubMCP(cwd)
	if got == nil {
		t.Fatalf("projectGitHubMCP should return a descriptor when a token is set, regardless of Issues.Provider")
	}
	if got.Name != "github" {
		t.Errorf("Name=%q want github", got.Name)
	}
	if got.URL != githubMCPDefaultEndpoint {
		t.Errorf("URL=%q want default %q", got.URL, githubMCPDefaultEndpoint)
	}
	if got.Headers["Authorization"] != "Bearer ghp_secret" {
		t.Errorf("Authorization header missing or wrong: %+v", got.Headers)
	}
}

// projectGitHubMCP must return nil when no token is configured.
// Otherwise the chat agent's MCP roster would carry a half-wired
// github entry that errors on every tool call.
func TestProjectGitHubMCP_NilWhenTokenAbsent(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	// No saved config at all — projectGitHubMCP must default to nil.
	if got := projectGitHubMCP(cwd); got != nil {
		t.Errorf("projectGitHubMCP should be nil with no config; got %+v", got)
	}
	// Even if the project entry exists with the issue provider set
	// to github, an empty token is still nil.
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, cwd, projectConfig{
		Issues: issuesConfig{Provider: "github"},
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	if got := projectGitHubMCP(cwd); got != nil {
		t.Errorf("projectGitHubMCP should be nil when token is empty; got %+v", got)
	}
}

// projectGitHubMCP honours an explicitly configured custom endpoint
// (GHE-style) so users on enterprise hosts are not forced through the
// public copilot endpoint.
func TestProjectGitHubMCP_HonoursCustomEndpoint(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, cwd, projectConfig{
		MCP: projectMCPConfig{
			GitHub: githubMCPConfig{
				Token:    "ghp_secret",
				Endpoint: "https://ghe.example/mcp",
			},
		},
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	got := projectGitHubMCP(cwd)
	if got == nil {
		t.Fatalf("projectGitHubMCP should return a descriptor")
	}
	if got.URL != "https://ghe.example/mcp" {
		t.Errorf("URL=%q want %q", got.URL, "https://ghe.example/mcp")
	}
}

// projectGitHubMCP returns nil when cwd is empty so callers that
// don't yet know their working directory don't blow up. The model's
// initial sessionArgs build can hit this path before cwd is set.
func TestProjectGitHubMCP_NilOnEmptyCwd(t *testing.T) {
	isolateHome(t)
	if got := projectGitHubMCP(""); got != nil {
		t.Errorf("projectGitHubMCP(\"\") should be nil; got %+v", got)
	}
}
