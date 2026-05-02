package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	_, err := p.ListIssues(context.Background(), projectConfig{}, "/tmp", nil, IssuePagination{Cursor: "", PerPage: 50})
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
	// New surface: ParseQuery / FormatQuery / KanbanColumns must
	// not crash and return inert defaults so the screen can fall
	// through to the "not configured" toast cleanly.
	if q, err := p.ParseQuery("anything"); q != nil || err != nil {
		t.Errorf("noneIssueProvider.ParseQuery should return (nil, nil), got (%v, %v)", q, err)
	}
	if got := p.FormatQuery(nil); got != "" {
		t.Errorf("noneIssueProvider.FormatQuery should be empty, got %q", got)
	}
	if got := p.QuerySyntaxHelp(); got == "" {
		t.Errorf("noneIssueProvider.QuerySyntaxHelp should be non-empty")
	}
	if cols := p.KanbanColumns(); len(cols) != 0 {
		t.Errorf("noneIssueProvider.KanbanColumns should be empty, got %d", len(cols))
	}
	if err := p.MoveIssue(context.Background(), projectConfig{}, "/tmp", issue{number: 1}, KanbanColumnSpec{}); !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("MoveIssue err=%v, want errIssueProviderNotConfigured", err)
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
	if !reflect.DeepEqual(got, pc) {
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

func TestProjectRoot_FindsGitDirectory(t *testing.T) {
	// Stand up a fake repo: TempDir with a .git directory inside.
	// Walking up from a subdir should land us back at the .git
	// parent — the canonical "project root".
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cmd", "x"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if got := projectRoot(filepath.Join(root, "cmd", "x")); got != root {
		t.Errorf("from subdir: got %q want %q", got, root)
	}
	if got := projectRoot(root); got != root {
		t.Errorf("from root itself: got %q want %q", got, root)
	}
}

func TestProjectRoot_WorktreeResolvesToMainRepo(t *testing.T) {
	// The ask-managed worktree case: main repo at <root>, with a
	// worktree dir under <root>/.claude/worktrees/foo. The
	// worktree's .git is a *file*, not a directory, so projectRoot
	// keeps walking up until it finds the main repo's .git
	// directory and returns the main root — both worktree and
	// main map to the same project key.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir main .git: %v", err)
	}
	wt := filepath.Join(root, ".claude", "worktrees", "foo")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"),
		[]byte("gitdir: "+root+"/.git/worktrees/foo\n"), 0o644); err != nil {
		t.Fatalf("write worktree .git file: %v", err)
	}
	if got := projectRoot(wt); got != root {
		t.Errorf("worktree should resolve to main root: got %q want %q", got, root)
	}
}

func TestProjectRoot_FallsBackToCwdWhenNoGit(t *testing.T) {
	tmp := t.TempDir()
	if got := projectRoot(tmp); got != tmp {
		t.Errorf("no .git anywhere should fall back to cwd: got %q want %q", got, tmp)
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
