package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// githubRepo is the repository a GitHub call addresses. host is the
// GitHub instance it lives on; under gh auth it picks the gh account
// whose token authorises the call.
type githubRepo struct {
	host  string
	owner string
	name  string
}

const (
	githubDefaultHost = "github.com"
	githubGHTimeout   = 15 * time.Second
)

// runGH is a seam: tests swap it so no test spawns gh.
var runGH = runGHCLI

// runGHCLI runs the gh CLI in dir (the process cwd when empty) and
// returns its stdout. A failure carries gh's stderr and never its
// stdout, because `gh auth token` prints the credential there.
func runGHCLI(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		verb := "gh " + strings.Join(args[:min(2, len(args))], " ")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %s", verb, msg)
		}
		return nil, fmt.Errorf("%s: %w", verb, err)
	}
	return out, nil
}

// githubAuthConfigured reports whether c names a credential source:
// a saved PAT, or the gh CLI, whose login is only checked on use.
func githubAuthConfigured(c githubMCPConfig) bool {
	return c.Auth == githubAuthGH || c.Token != ""
}

// githubAuthSummary is the Project Options value for c's source.
func githubAuthSummary(c githubMCPConfig) string {
	if c.Auth == githubAuthGH {
		return "gh CLI"
	}
	return "PAT"
}

// githubProjectReachable is the Configured gate both GitHub providers
// share. A PAT project must also have a github.com origin. A gh
// project hands the repo to `gh repo view`, a network round trip that
// does not belong on the UI goroutine, so its failures surface in the
// screen's load-error modal instead.
func githubProjectReachable(c githubMCPConfig, cwd string) bool {
	switch {
	case c.Auth == githubAuthGH:
		return true
	case c.Token == "":
		return false
	}
	_, err := resolveGitHubRepo(c, cwd)
	return err == nil
}

// resolveGitHubTarget is the preamble of every GitHub provider call:
// the repository cwd addresses and the token that authorises it. A
// project without a credential source is errIssueProviderNotConfigured.
func resolveGitHubTarget(c githubMCPConfig, cwd string) (githubRepo, string, error) {
	if !githubAuthConfigured(c) {
		return githubRepo{}, "", errIssueProviderNotConfigured
	}
	repo, err := resolveGitHubRepo(c, cwd)
	if err != nil {
		return githubRepo{}, "", fmt.Errorf("resolve repo: %w", err)
	}
	token, err := githubToken(c, repo.host)
	if err != nil {
		return githubRepo{}, "", err
	}
	return repo, token, nil
}

// githubToken returns the credential for calls against host: the
// saved PAT, or the gh CLI's token for host (gh's default host when
// host is empty). gh is asked every time rather than cached, so a
// re-login or a refreshed token is picked up on the next call.
func githubToken(c githubMCPConfig, host string) (string, error) {
	if c.Auth != githubAuthGH {
		return c.Token, nil
	}
	args := []string{"auth", "token"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), githubGHTimeout)
	defer cancel()
	out, err := runGH(ctx, "", args...)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gh auth token: empty token")
	}
	return token, nil
}

// resolveGitHubRepo resolves cwd to the repository its GitHub calls
// address. Under PAT auth that is `git remote get-url origin`, which
// must be a github.com URL (HTTPS or SSH). Under gh auth it is
// whatever `gh repo view` picks: the first remote on a host gh is
// logged in to, preferring upstream, then github, then origin, under
// the repository's current name.
func resolveGitHubRepo(c githubMCPConfig, cwd string) (githubRepo, error) {
	if cwd == "" {
		return githubRepo{}, fmt.Errorf("cwd is empty")
	}
	if c.Auth == githubAuthGH {
		return ghRepos.resolve(cwd)
	}
	out, err := exec.Command("git", "-C", cwd, "remote", "get-url", "origin").Output()
	if err != nil {
		return githubRepo{}, fmt.Errorf("git remote: %w", err)
	}
	remote := strings.TrimSpace(string(out))
	owner, name, ok := parseGitHubRemoteURL(remote)
	if !ok {
		return githubRepo{}, fmt.Errorf("not a github remote: %q", remote)
	}
	return githubRepo{host: githubDefaultHost, owner: owner, name: name}, nil
}

// ghRepoCache memoises `gh repo view` per cwd, because it is a
// network round trip and every provider call (and IssueRef, on the UI
// goroutine) needs the repo. The key carries the checkout's remotes,
// so adding or editing one re-resolves. The lock is held across the
// gh call so concurrent column loads share one round trip.
type ghRepoCache struct {
	mu    sync.Mutex
	repos map[string]githubRepo
}

var ghRepos = &ghRepoCache{}

func (c *ghRepoCache) resolve(cwd string) (githubRepo, error) {
	remotes, _ := exec.Command("git", "-C", cwd, "remote", "-v").Output()
	key := cwd + "\x00" + string(remotes)
	c.mu.Lock()
	defer c.mu.Unlock()
	if repo, ok := c.repos[key]; ok {
		return repo, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), githubGHTimeout)
	defer cancel()
	out, err := runGH(ctx, cwd, "repo", "view", "--json", "owner,name,url")
	if err != nil {
		return githubRepo{}, err
	}
	repo, err := parseGHRepoView(out)
	if err != nil {
		return githubRepo{}, err
	}
	if c.repos == nil {
		c.repos = map[string]githubRepo{}
	}
	c.repos[key] = repo
	return repo, nil
}

func parseGHRepoView(out []byte) (githubRepo, error) {
	var v struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return githubRepo{}, fmt.Errorf("gh repo view: parse: %w", err)
	}
	u, err := url.Parse(v.URL)
	if err != nil || u.Host == "" || v.Owner.Login == "" || v.Name == "" {
		return githubRepo{}, fmt.Errorf("gh repo view: incomplete repository %q", v.URL)
	}
	return githubRepo{host: u.Host, owner: v.Owner.Login, name: v.Name}, nil
}

// projectGitHubMCP resolves the project-level GitHub MCP credentials
// for cwd into a wire-shape descriptor, or nil when the project has
// no credential source or gh cannot produce a token. Under gh auth
// the token is for the repo's host, or gh's default host when cwd has
// no repository gh can resolve yet.
func projectGitHubMCP(cfg askConfig, cwd string) *issueMCPServer {
	if cwd == "" {
		return nil
	}
	gh := loadProjectConfig(cfg, cwd).MCP.GitHub
	if !githubAuthConfigured(gh) {
		return nil
	}
	host := ""
	if gh.Auth == githubAuthGH {
		if repo, err := resolveGitHubRepo(gh, cwd); err == nil {
			host = repo.host
		} else {
			debugLog("project github mcp: %v", err)
		}
	}
	token, err := githubToken(gh, host)
	if err != nil {
		debugLog("project github mcp: %v", err)
		return nil
	}
	return &issueMCPServer{
		Name: "github",
		URL:  githubMCPEndpointOrDefault(gh),
		Headers: map[string]string{
			"Authorization": "Bearer " + token,
		},
	}
}
