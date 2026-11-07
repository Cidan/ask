package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ghCall struct {
	dir  string
	args []string
}

// fakeGH swaps the runGH seam for reply and gives the test its own
// `gh repo view` cache. It returns the recorded invocations.
func fakeGH(t *testing.T, reply func(dir string, args []string) ([]byte, error)) func() []ghCall {
	t.Helper()
	prevRun, prevRepos := runGH, ghRepos
	t.Cleanup(func() { runGH, ghRepos = prevRun, prevRepos })
	ghRepos = &ghRepoCache{}
	var (
		mu    sync.Mutex
		calls []ghCall
	)
	runGH = func(_ context.Context, dir string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, ghCall{dir: dir, args: append([]string(nil), args...)})
		mu.Unlock()
		return reply(dir, args)
	}
	return func() []ghCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]ghCall(nil), calls...)
	}
}

// ghReplies answers `gh repo view` with repoJSON and `gh auth token`
// with whatever tokens() returns at call time.
func ghReplies(repoJSON string, tokens func() string) func(string, []string) ([]byte, error) {
	return func(_ string, args []string) ([]byte, error) {
		switch strings.Join(args[:2], " ") {
		case "repo view":
			return []byte(repoJSON), nil
		case "auth token":
			return []byte(tokens() + "\n"), nil
		}
		return nil, errors.New("unexpected gh call")
	}
}

func countGH(calls []ghCall, verb string) int {
	n := 0
	for _, c := range calls {
		if strings.Join(c.args[:2], " ") == verb {
			n++
		}
	}
	return n
}

const ghRepoViewCidanAsk = `{"name":"ask","owner":{"id":"MDQ6","login":"Cidan"},"url":"https://github.com/Cidan/ask"}`

func TestRunGH_SeamDefaultsToCLI(t *testing.T) {
	if reflect.ValueOf(runGH).Pointer() != reflect.ValueOf(runGHCLI).Pointer() {
		t.Fatal("runGH seam defaults away from runGHCLI")
	}
}

// Under gh auth the repo is whatever `gh repo view` names (host
// included) and the token is gh's for that host, asked on every call.
func TestResolveGitHubTarget_GHAuthUsesRepoViewAndHostToken(t *testing.T) {
	calls := fakeGH(t, ghReplies(
		`{"name":"tool","owner":{"login":"acme"},"url":"https://ghe.example.com/acme/tool"}`,
		func() string { return "gho_enterprise" }))
	cwd := t.TempDir()
	gh := githubMCPConfig{Auth: githubAuthGH}

	repo, token, err := resolveGitHubTarget(gh, cwd)
	if err != nil {
		t.Fatalf("resolveGitHubTarget: %v", err)
	}
	if repo != (githubRepo{host: "ghe.example.com", owner: "acme", name: "tool"}) {
		t.Errorf("repo=%+v", repo)
	}
	if token != "gho_enterprise" {
		t.Errorf("token=%q want gho_enterprise", token)
	}
	got := calls()
	want := []ghCall{
		{dir: cwd, args: []string{"repo", "view", "--json", "owner,name,url"}},
		{dir: "", args: []string{"auth", "token", "--hostname", "ghe.example.com"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gh calls=%+v\nwant %+v", got, want)
	}

	if _, _, err := resolveGitHubTarget(gh, cwd); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	got = calls()
	if countGH(got, "repo view") != 1 {
		t.Errorf("repo view must be cached across calls; calls=%+v", got)
	}
	if countGH(got, "auth token") != 2 {
		t.Errorf("auth token must be asked on every call; calls=%+v", got)
	}
}

// A changed remote set re-resolves the repo; an unchanged one does not.
func TestGHRepoCache_RemotesChangeReResolves(t *testing.T) {
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/Cidan/ask")
	calls := fakeGH(t, ghReplies(ghRepoViewCidanAsk, func() string { return "gho_x" }))
	gh := githubMCPConfig{Auth: githubAuthGH}

	for range 2 {
		if _, err := resolveGitHubRepo(gh, dir); err != nil {
			t.Fatalf("resolveGitHubRepo: %v", err)
		}
	}
	if n := countGH(calls(), "repo view"); n != 1 {
		t.Fatalf("repo view calls=%d want 1 with unchanged remotes", n)
	}
	runGit(t, dir, "remote", "add", "upstream", "https://github.com/upstream/ask")
	if _, err := resolveGitHubRepo(gh, dir); err != nil {
		t.Fatalf("resolveGitHubRepo after remote add: %v", err)
	}
	if n := countGH(calls(), "repo view"); n != 2 {
		t.Errorf("repo view calls=%d want 2 after the remotes changed", n)
	}
}

// gh failures surface verbatim (wrapped in the resolve step) and are
// not cached, so the next call retries.
func TestResolveGitHubTarget_GHFailureSurfacesAndRetries(t *testing.T) {
	failRepo := true
	calls := fakeGH(t, func(_ string, args []string) ([]byte, error) {
		if args[0] == "repo" && failRepo {
			return nil, errors.New("gh repo view: none of the git remotes point to a known GitHub host")
		}
		if args[0] == "auth" {
			return nil, errors.New("gh auth token: no oauth token found for github.com")
		}
		return []byte(ghRepoViewCidanAsk), nil
	})
	cwd := t.TempDir()
	gh := githubMCPConfig{Auth: githubAuthGH}

	_, _, err := resolveGitHubTarget(gh, cwd)
	if err == nil || !strings.Contains(err.Error(), "resolve repo: gh repo view: none of the git remotes") {
		t.Fatalf("err=%v want the wrapped gh repo view failure", err)
	}
	if errors.Is(err, errIssueProviderNotConfigured) {
		t.Error("a gh failure must not collapse into the generic not-configured error")
	}
	failRepo = false
	_, _, err = resolveGitHubTarget(gh, cwd)
	if err == nil || !strings.Contains(err.Error(), "no oauth token found") {
		t.Fatalf("err=%v want the gh auth token failure", err)
	}
	if n := countGH(calls(), "repo view"); n != 2 {
		t.Errorf("a failed repo view must not be cached; repo view calls=%d", n)
	}
}

// PAT auth never touches gh: a blank token is the not-configured
// sentinel, and a set one goes out as-is against the origin remote.
func TestResolveGitHubTarget_PATAuthNeverCallsGH(t *testing.T) {
	calls := fakeGH(t, func(string, []string) ([]byte, error) {
		return nil, errors.New("gh must not run under PAT auth")
	})
	if _, _, err := resolveGitHubTarget(githubMCPConfig{}, t.TempDir()); !errors.Is(err, errIssueProviderNotConfigured) {
		t.Errorf("blank PAT err=%v want errIssueProviderNotConfigured", err)
	}
	dir := initGitRepo(t)
	runGit(t, dir, "remote", "add", "origin", "git@github.com:Cidan/ask.git")
	repo, token, err := resolveGitHubTarget(githubMCPConfig{Token: "ghp_pat"}, dir)
	if err != nil {
		t.Fatalf("resolveGitHubTarget: %v", err)
	}
	if repo != (githubRepo{host: "github.com", owner: "Cidan", name: "ask"}) || token != "ghp_pat" {
		t.Errorf("repo=%+v token=%q", repo, token)
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("gh calls under PAT auth: %+v", got)
	}
}

// The Configured gate for a gh project does no gh work on the UI
// goroutine; a PAT project still needs a token and a github.com origin.
func TestGitHubProjectReachable(t *testing.T) {
	calls := fakeGH(t, func(string, []string) ([]byte, error) {
		return nil, errors.New("gh must not run in the Configured gate")
	})
	if !githubProjectReachable(githubMCPConfig{Auth: githubAuthGH}, t.TempDir()) {
		t.Error("gh auth should be reachable without a token or remote check")
	}
	if githubProjectReachable(githubMCPConfig{}, t.TempDir()) {
		t.Error("blank PAT should not be reachable")
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("gh calls in the Configured gate: %+v", got)
	}
}

func TestParseGHRepoView_RejectsIncompleteRepository(t *testing.T) {
	for _, body := range []string{
		`{"name":"ask","owner":{"login":""},"url":"https://github.com/Cidan/ask"}`,
		`{"name":"","owner":{"login":"Cidan"},"url":"https://github.com/Cidan/ask"}`,
		`{"name":"ask","owner":{"login":"Cidan"},"url":""}`,
		`not json`,
	} {
		if repo, err := parseGHRepoView([]byte(body)); err == nil {
			t.Errorf("parseGHRepoView(%s)=%+v, want an error", body, repo)
		}
	}
}

// Both GitHub providers dial the MCP server with gh's token under gh
// auth, and a token gh re-issued rebuilds the cached session.
func TestGitHubProviders_GHAuthDialsWithGHToken(t *testing.T) {
	withFakeGitHubIssueMCPEndpoint(t)
	inner := dialGitHubMCPSessionFn
	var (
		mu     sync.Mutex
		dialed []string
	)
	dialGitHubMCPSessionFn = func(ctx context.Context, endpoint, token string, timeout time.Duration) (*mcp.ClientSession, error) {
		mu.Lock()
		dialed = append(dialed, token)
		mu.Unlock()
		return inner(ctx, endpoint, token, timeout)
	}
	token := "gho_first"
	fakeGH(t, ghReplies(ghRepoViewCidanAsk, func() string { return token }))
	cwd := t.TempDir()
	cfg := projectConfig{
		Issues: issuesConfig{Provider: "github"},
		MCP:    projectMCPConfig{GitHub: githubMCPConfig{Auth: githubAuthGH}},
	}

	p := &githubIssueProvider{}
	t.Cleanup(func() {
		if p.session != nil {
			_ = p.session.Close()
		}
	})
	if !p.Configured(cfg, cwd) {
		t.Fatal("issue provider should be configured under gh auth without a PAT")
	}
	for range 2 {
		page, err := p.ListIssues(context.Background(), cfg, cwd, nil, IssuePagination{PerPage: 10})
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		if len(page.Issues) != 1 || page.Issues[0].number != 7 {
			t.Fatalf("issues=%+v", page.Issues)
		}
	}
	token = "gho_second"
	if _, err := p.GetIssue(context.Background(), cfg, cwd, 7); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	ref, err := p.IssueRef(cfg, cwd, issue{number: 7})
	if err != nil || ref.Project != "Cidan/ask" {
		t.Errorf("IssueRef=%+v err=%v want project Cidan/ask", ref, err)
	}
	mu.Lock()
	got := append([]string(nil), dialed...)
	mu.Unlock()
	if want := []string{"gho_first", "gho_second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dialed tokens=%v want %v (one session per token)", got, want)
	}

	pr := &githubPRProvider{}
	if !pr.Configured(cfg, cwd) {
		t.Error("PR provider should be configured under gh auth without a PAT")
	}
	if ref, err := pr.IssueRef(cfg, cwd, issue{number: 3}); err != nil || ref.Project != "Cidan/ask" {
		t.Errorf("PR IssueRef=%+v err=%v want project Cidan/ask", ref, err)
	}
}

func TestPRProvider_GHAuthMergeDialsWithGHToken(t *testing.T) {
	withFakeGitHubMCPEndpoint(t)
	inner := dialGitHubMCPSessionFn
	var dialed string
	dialGitHubMCPSessionFn = func(ctx context.Context, endpoint, token string, timeout time.Duration) (*mcp.ClientSession, error) {
		dialed = token
		return inner(ctx, endpoint, token, timeout)
	}
	fakeGH(t, ghReplies(ghRepoViewCidanAsk, func() string { return "gho_pr" }))
	cfg := projectConfig{MCP: projectMCPConfig{GitHub: githubMCPConfig{Auth: githubAuthGH}}}
	p := &githubPRProvider{}
	t.Cleanup(func() {
		if p.session != nil {
			_ = p.session.Close()
		}
	})
	if err := p.Merge(context.Background(), cfg, t.TempDir(), issue{number: 1}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if dialed != "gho_pr" {
		t.Errorf("dialed token=%q want gho_pr", dialed)
	}
}

// The agent's GitHub MCP descriptor carries gh's token under gh auth:
// for the repo's host when gh resolves one, else gh's default host.
// No token from gh means no descriptor.
func TestProjectGitHubMCP_GHAuth(t *testing.T) {
	cwd := t.TempDir()
	cfg := upsertProjectConfig(askConfig{}, cwd, projectConfig{
		MCP: projectMCPConfig{GitHub: githubMCPConfig{Auth: githubAuthGH}},
	})

	calls := fakeGH(t, ghReplies(ghRepoViewCidanAsk, func() string { return "gho_agent" }))
	got := projectGitHubMCP(cfg, cwd)
	if got == nil || got.Headers["Authorization"] != "Bearer gho_agent" || got.URL != githubMCPDefaultEndpoint {
		t.Fatalf("descriptor=%+v want the default endpoint with gh's token", got)
	}
	if c := calls(); len(c) != 2 || !reflect.DeepEqual(c[1].args, []string{"auth", "token", "--hostname", "github.com"}) {
		t.Errorf("gh calls=%+v want repo view then auth token for github.com", c)
	}

	calls = fakeGH(t, func(_ string, args []string) ([]byte, error) {
		if args[0] == "repo" {
			return nil, errors.New("gh repo view: no git remotes found")
		}
		return []byte("gho_default\n"), nil
	})
	got = projectGitHubMCP(cfg, cwd)
	if got == nil || got.Headers["Authorization"] != "Bearer gho_default" {
		t.Fatalf("descriptor=%+v want gh's default-host token", got)
	}
	if c := calls(); len(c) != 2 || !reflect.DeepEqual(c[1].args, []string{"auth", "token"}) {
		t.Errorf("gh calls=%+v want auth token without --hostname", c)
	}

	fakeGH(t, func(string, []string) ([]byte, error) {
		return nil, errors.New("gh auth token: no oauth token found")
	})
	if got := projectGitHubMCP(cfg, cwd); got != nil {
		t.Errorf("descriptor=%+v want nil when gh has no token", got)
	}
}

// The project GitHub MCP server is attached only when the session asks
// for it, and resolved from the config handed to the session.
func TestAgentSessionMCPServers_ProjectMCPFlag(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	cfg := upsertProjectConfig(askConfig{}, cwd, projectConfig{
		MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "ghp_agent"}},
	})
	if got := agentSessionMCPServers(ProviderSessionArgs{Cwd: cwd}, cfg); len(got) != 0 {
		t.Errorf("servers=%+v want none without ProjectMCP", got)
	}
	got := agentSessionMCPServers(ProviderSessionArgs{Cwd: cwd, ProjectMCP: true}, cfg)
	if len(got) != 1 || got[0].Name != "github" ||
		got[0].Cfg.URL != githubMCPDefaultEndpoint ||
		got[0].Cfg.Headers["Authorization"] != "Bearer ghp_agent" {
		t.Errorf("servers=%+v want the project github server", got)
	}
}
