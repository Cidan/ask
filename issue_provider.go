package main

import (
	"context"
	"errors"
)

// IssueProvider is the abstraction over remote issue-tracking
// backends. The first concrete implementation is the GitHub MCP
// server; ClickUp / Linear / GitLab plug in alongside as they land.
//
// Providers are MCP-backed today: the wire protocol is the MCP
// streamable HTTP transport defined by the 2025-03-26 spec, and
// each backend exposes named tools the implementation calls. We
// don't go through the agent (claude/codex) for issue ops — the
// whole point is to avoid spending a turn on something the user
// can do directly. The methods below are the typed surface a
// per-provider client implements; the rest of the app is
// provider-agnostic.
//
// Cancellation: every method takes a context.Context. The issues
// screen wires it to a per-load handle so ctrl+o / tab close /
// /clear can interrupt an in-flight network round trip without
// stranding goroutines.
type IssueProvider interface {
	// ID returns the stable identifier used in the on-disk config
	// (e.g. "github", "clickup", "none"). MUST be lowercase, kebab-
	// or snake-cased — written into JSON.
	ID() string
	// DisplayName is the human-facing label shown in the /config
	// picker and the issues-screen header.
	DisplayName() string
	// Configured reports whether the provider has enough config
	// (token, endpoint, repo identity, …) to actually serve a list
	// request from cwd. The /config picker uses this to decide
	// whether the "configured" badge lights up; the issues screen
	// uses it to decide between rendering data and showing the
	// "Issues not configured for this project" toast.
	Configured(cfg projectConfig, cwd string) bool
	// ListIssues returns the open + most-recent issue collection for
	// the project rooted at cwd. The provider is responsible for
	// resolving cwd → backend identity (e.g. github needs owner/repo
	// from `git remote get-url origin`).
	ListIssues(ctx context.Context, cfg projectConfig, cwd string) ([]issue, error)
	// GetIssue returns one issue with description + comments
	// hydrated. Called when the user hits Enter on a list row to
	// open the detail view.
	GetIssue(ctx context.Context, cfg projectConfig, cwd string, number int) (issue, error)
}

// errIssueProviderNotConfigured is the canonical error returned by
// ListIssues / GetIssue when the project has no configured
// provider, or when the configured provider lacks credentials. The
// issues screen translates this specific error into the
// "Issues not configured for this project" toast — other errors
// surface verbatim so the user can see actual network / auth
// failures instead of a confusing "not configured" message.
var errIssueProviderNotConfigured = errors.New("issues not configured for this project")

// issueProviderRegistry is the canonical list of provider
// implementations available for the /config picker. Order matters:
// it's the order rows render in. "none" is always first so the
// picker can default-highlight it for unconfigured projects. New
// backends append to this slice.
var issueProviderRegistry = []IssueProvider{
	noneIssueProvider{},
	&githubIssueProvider{},
}

// issueProviderByID returns the registered provider with the given
// id, or noneIssueProvider when nothing matches (including the
// empty string for unconfigured projects). Never returns nil — the
// "none" instance handles every unconfigured-path call cleanly.
func issueProviderByID(id string) IssueProvider {
	for _, p := range issueProviderRegistry {
		if p.ID() == id {
			return p
		}
	}
	return noneIssueProvider{}
}

// activeIssueProvider is the convenience accessor the issues screen
// uses on every load: read the saved per-project config, look up
// the matching provider, return it. Always non-nil.
func activeIssueProvider(cfg askConfig, cwd string) (IssueProvider, projectConfig) {
	pc := loadProjectConfig(cfg, cwd)
	return issueProviderByID(pc.Issues.Provider), pc
}

// noneIssueProvider is the explicit "issues are not configured"
// implementation. ID is the empty string so it matches the default
// projectConfig.Issues.Provider, which means a fresh project with
// no on-disk entry routes to this without any extra plumbing.
type noneIssueProvider struct{}

func (noneIssueProvider) ID() string                                 { return "" }
func (noneIssueProvider) DisplayName() string                        { return "None (issues disabled)" }
func (noneIssueProvider) Configured(projectConfig, string) bool      { return false }
func (noneIssueProvider) ListIssues(context.Context, projectConfig, string) ([]issue, error) {
	return nil, errIssueProviderNotConfigured
}
func (noneIssueProvider) GetIssue(context.Context, projectConfig, string, int) (issue, error) {
	return issue{}, errIssueProviderNotConfigured
}
