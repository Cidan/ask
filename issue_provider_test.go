package main

import (
	"context"
	"errors"
	"testing"
)

func TestIssueProviderByID_KnownProvidersResolve(t *testing.T) {
	cases := []struct {
		id       string
		wantID   string
		wantName string
	}{
		{"", "", "None (issues disabled)"},
		{"none", "", "None (issues disabled)"}, // unknown id falls back to none
		{"github", "github", "GitHub Issues"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p := issueProviderByID(tc.id)
			if p.ID() != tc.wantID {
				t.Errorf("ID()=%q want %q", p.ID(), tc.wantID)
			}
			if p.DisplayName() != tc.wantName {
				t.Errorf("DisplayName()=%q want %q", p.DisplayName(), tc.wantName)
			}
		})
	}
}

func TestNoneIssueProvider_ReturnsNotConfigured(t *testing.T) {
	p := noneIssueProvider{}
	_, err := p.ListIssues(context.Background(), projectConfig{}, "/tmp")
	if !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("ListIssues err=%v, want errIssueProviderNotConfigured", err)
	}
	_, err = p.GetIssue(context.Background(), projectConfig{}, "/tmp", 1)
	if !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("GetIssue err=%v, want errIssueProviderNotConfigured", err)
	}
	if p.Configured(projectConfig{}, "/tmp") {
		t.Errorf("noneIssueProvider should never report Configured")
	}
}

func TestActiveIssueProvider_DefaultIsNone(t *testing.T) {
	cfg := askConfig{}
	p, _ := activeIssueProvider(cfg, "/some/project")
	if p.ID() != "" {
		t.Errorf("default cfg should resolve to noneIssueProvider, got %q", p.ID())
	}
}

func TestActiveIssueProvider_RoutesToConfiguredProvider(t *testing.T) {
	cfg := askConfig{
		Projects: map[string]projectConfig{
			"/proj": {Issues: issuesConfig{Provider: "github"}},
		},
	}
	p, pc := activeIssueProvider(cfg, "/proj")
	if p.ID() != "github" {
		t.Errorf("provider=%q want github", p.ID())
	}
	if pc.Issues.Provider != "github" {
		t.Errorf("returned project config provider=%q want github", pc.Issues.Provider)
	}
}

func TestProjectConfig_RoundTripPersistsViaUpsert(t *testing.T) {
	cfg := askConfig{}
	pc := projectConfig{
		Issues: issuesConfig{
			Provider: "github",
			GitHub:   githubIssuesConfig{Token: "ghp_secret", Endpoint: "https://example.com/mcp"},
		},
	}
	cfg = upsertProjectConfig(cfg, "/home/me/proj", pc)
	got := loadProjectConfig(cfg, "/home/me/proj")
	if got != pc {
		t.Errorf("round-trip mismatch:\n got:  %+v\n want: %+v", got, pc)
	}
}

func TestProjectConfig_UpsertWithZeroValueDeletes(t *testing.T) {
	cfg := askConfig{
		Projects: map[string]projectConfig{
			"/p": {Issues: issuesConfig{Provider: "github"}},
		},
	}
	cfg = upsertProjectConfig(cfg, "/p", projectConfig{})
	if _, ok := cfg.Projects["/p"]; ok {
		t.Errorf("zero-value upsert should delete the entry")
	}
}

func TestProjectKey_NormalizesTrailingSlash(t *testing.T) {
	a := projectKey("/home/me/proj/")
	b := projectKey("/home/me/proj")
	if a != b {
		t.Errorf("trailing slash should normalise: %q vs %q", a, b)
	}
}

func TestProjectKey_EmptyReturnsEmpty(t *testing.T) {
	if got := projectKey(""); got != "" {
		t.Errorf("empty cwd → %q want empty", got)
	}
}

func TestGitHubEndpointDefault_AppliesWhenBlank(t *testing.T) {
	got := githubEndpointOrDefault(githubIssuesConfig{})
	if got != githubIssuesDefaultEndpoint {
		t.Errorf("default = %q want %q", got, githubIssuesDefaultEndpoint)
	}
}

func TestGitHubEndpointDefault_RespectsExplicit(t *testing.T) {
	got := githubEndpointOrDefault(githubIssuesConfig{Endpoint: "https://ghe.example/mcp"})
	if got != "https://ghe.example/mcp" {
		t.Errorf("explicit endpoint not preserved: %q", got)
	}
}
