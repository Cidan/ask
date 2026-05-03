package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestCodexCLIArgs_Defaults(t *testing.T) {
	want := []string{"app-server", "--listen", "stdio://"}
	got := codexCLIArgs(ProviderSessionArgs{})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codexCLIArgs(zero) = %v, want %v", got, want)
	}
}

func TestCodexCLIArgs_IgnoresArgsForNow(t *testing.T) {
	// MVP: config-option plumbing is deliberately out of scope. Prove every
	// populated field flows through unchanged so a future PR can plug in -c
	// overrides without hunting for surprise coupling.
	want := codexCLIArgs(ProviderSessionArgs{})
	got := codexCLIArgs(ProviderSessionArgs{
		Cwd:                "/tmp/x",
		MCPPort:            9999,
		Model:              "gpt-5",
		Effort:             "high",
		OllamaHost:         "localhost:11434",
		OllamaModel:        "llama3",
		SkipAllPermissions: true,
		Worktree:           true,
		SessionID:          "sess-1",
		ResumeCwd:          "/tmp/y",
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codexCLIArgs should ignore session args for MVP\n got=%v\nwant=%v", got, want)
	}
}

func TestCodexCLIArgs_UsesAppServerCommand(t *testing.T) {
	args := codexCLIArgs(ProviderSessionArgs{})
	if len(args) < 1 || args[0] != "app-server" {
		t.Fatalf("first arg must be the app-server subcommand (got %v)", args)
	}
}

func TestCodexCLIArgs_ListensOnStdio(t *testing.T) {
	// stdio:// is the default but we pass it explicitly so behavior can't
	// silently flip if the default changes upstream. This is what the MVP
	// relies on for the JSON-RPC pipe.
	args := codexCLIArgs(ProviderSessionArgs{})
	if argAfter(args, "--listen") != "stdio://" {
		t.Fatalf("--listen must be stdio://, got %v", args)
	}
}

func TestCodexCLIArgs_AddedDirsEmitWritableRootsOverride(t *testing.T) {
	args := codexCLIArgs(ProviderSessionArgs{
		AddedDirs: []string{"/a", "/b"},
	})
	got := argAfter(args, "-c")
	want := `sandbox_workspace_write.writable_roots=["/a","/b"]`
	if got != want {
		t.Fatalf("-c override = %q want %q; argv=%v", got, want, args)
	}
}

func TestCodexCLIArgs_AddedDirsAbsentOnEmpty(t *testing.T) {
	args := codexCLIArgs(ProviderSessionArgs{})
	if containsArg(args, "-c") {
		t.Errorf("empty AddedDirs should leave -c absent: %v", args)
	}
}

func TestCodexCLIArgs_AddedDirsQuoteEscaped(t *testing.T) {
	// Paths with special TOML characters must be quoted so codex's
	// parser doesn't choke. strconv.Quote handles \ and " for us.
	args := codexCLIArgs(ProviderSessionArgs{
		AddedDirs: []string{`/with "quotes"`, `/with\backslash`},
	})
	got := argAfter(args, "-c")
	if !strings.Contains(got, `"/with \"quotes\""`) || !strings.Contains(got, `"/with\\backslash"`) {
		t.Errorf("-c override should quote-escape paths; got %q; argv=%v", got, args)
	}
}

// ProjectMCP must land as a -c mcp_servers.<name>.url override pair so
// codex's TOML config picks up an HTTP MCP server at launch — same
// access ask uses for ctrl+i. Bearer auth goes via env (codex's MCP
// config takes only an env-var name, not the literal token).
func TestCodexCLIArgs_ProjectMCPEmitsURLAndBearerOverrides(t *testing.T) {
	args := codexCLIArgs(ProviderSessionArgs{
		ProjectMCP: &issueMCPServer{
			Name:    "github",
			URL:     "https://api.githubcopilot.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer ghp_secret"},
		},
	})
	var sawURL, sawBearer bool
	for i, a := range args {
		if a != "-c" || i+1 >= len(args) {
			continue
		}
		v := args[i+1]
		switch {
		case strings.HasPrefix(v, "mcp_servers.github.url="):
			sawURL = true
			if !strings.Contains(v, `"https://api.githubcopilot.com/mcp"`) {
				t.Errorf("url override should TOML-quote the value: %q", v)
			}
		case strings.HasPrefix(v, "mcp_servers.github.bearer_token_env_var="):
			sawBearer = true
			if !strings.Contains(v, `"`+codexProjectMCPBearerEnv+`"`) {
				t.Errorf("bearer_token_env_var should point at %s, got %q",
					codexProjectMCPBearerEnv, v)
			}
		}
	}
	if !sawURL {
		t.Errorf("missing mcp_servers.github.url override; argv=%v", args)
	}
	if !sawBearer {
		t.Errorf("missing mcp_servers.github.bearer_token_env_var override; argv=%v", args)
	}
}

// Public/unauthenticated MCP servers (no Authorization header) skip
// the bearer override but still emit the url. Otherwise codex would
// refuse the launch citing a missing env var.
func TestCodexCLIArgs_ProjectMCPWithoutAuthOmitsBearer(t *testing.T) {
	args := codexCLIArgs(ProviderSessionArgs{
		ProjectMCP: &issueMCPServer{
			Name: "public",
			URL:  "https://example.com/mcp",
		},
	})
	for i, a := range args {
		if a == "-c" && i+1 < len(args) &&
			strings.Contains(args[i+1], "bearer_token_env_var") {
			t.Errorf("bearer_token_env_var should be absent without an auth header; argv=%v", args)
		}
	}
}

// codexEnv must export the bearer token under codexProjectMCPBearerEnv
// so the codex subprocess can read it. The token MUST appear only in
// the env (process-private) — never on argv (visible via ps).
func TestCodexEnv_ExportsBearerToken(t *testing.T) {
	env := codexEnv(ProviderSessionArgs{
		ProjectMCP: &issueMCPServer{
			Headers: map[string]string{"Authorization": "Bearer ghp_secret"},
		},
	})
	want := codexProjectMCPBearerEnv + "=ghp_secret"
	var saw bool
	for _, e := range env {
		if e == want {
			saw = true
		}
	}
	if !saw {
		t.Errorf("env should carry %q; got tail=%v", want, env[len(env)-min(len(env), 3):])
	}
}

func TestCodexEnv_NoProjectMCP_LeavesEnvUntouched(t *testing.T) {
	t.Setenv(codexProjectMCPBearerEnv, "stale-token")
	env := codexEnv(ProviderSessionArgs{})
	for _, e := range env {
		if strings.HasPrefix(e, codexProjectMCPBearerEnv+"=") {
			t.Errorf("no ProjectMCP should leave %s unset; saw %q", codexProjectMCPBearerEnv, e)
		}
	}
}

// codexCLIArgs must surface the github MCP override whenever the
// project has a token saved, even when Issues.Provider is empty.
// Mirror of TestClaudeCLIArgs_ProjectMCPIndependentOfIssueProvider —
// proves the inversion holds across both providers.
func TestCodexCLIArgs_ProjectMCPIndependentOfIssueProvider(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	cfg, _ := loadConfig()
	cfg = upsertProjectConfig(cfg, cwd, projectConfig{
		MCP: projectMCPConfig{GitHub: githubMCPConfig{Token: "ghp_secret"}},
		// Issues.Provider deliberately left empty.
	})
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("seed saveConfig: %v", err)
	}
	args := codexCLIArgs(ProviderSessionArgs{
		ProjectMCP: projectGitHubMCP(cwd),
	})
	var sawURL bool
	for i, a := range args {
		if a == "-c" && i+1 < len(args) &&
			strings.HasPrefix(args[i+1], "mcp_servers.github.url=") {
			sawURL = true
		}
	}
	if !sawURL {
		t.Errorf("github MCP url override must be injected when MCP token is set even without an issue provider; argv=%v", args)
	}
}

func TestCodexEnv_PublicProjectMCPDoesNotLeakInheritedBearer(t *testing.T) {
	t.Setenv(codexProjectMCPBearerEnv, "stale-token")
	env := codexEnv(ProviderSessionArgs{
		ProjectMCP: &issueMCPServer{
			Name: "public",
			URL:  "https://example.com/mcp",
		},
	})
	for _, e := range env {
		if strings.HasPrefix(e, codexProjectMCPBearerEnv+"=") {
			t.Errorf("public ProjectMCP should not inherit %s; saw %q", codexProjectMCPBearerEnv, e)
		}
	}
}
