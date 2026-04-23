package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// containsArg reports whether name appears anywhere in argv.
func containsArg(argv []string, name string) bool {
	for _, a := range argv {
		if a == name {
			return true
		}
	}
	return false
}

// argAfter returns the argument immediately following name, or "" if name
// is absent or last.
func argAfter(argv []string, name string) string {
	for i, a := range argv {
		if a == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func TestClaudeCLIArgs_Baseline(t *testing.T) {
	args := claudeCLIArgs(ProviderSessionArgs{}, false)
	for _, want := range []string{"-p", "--input-format", "stream-json", "--output-format", "--verbose"} {
		if !containsArg(args, want) {
			t.Errorf("claudeCLIArgs missing baseline flag %q: %v", want, args)
		}
	}
	// First flag should be -p so claude enters programmatic mode.
	if args[0] != "-p" {
		t.Errorf("first flag=%q want -p", args[0])
	}
	if argAfter(args, "--input-format") != "stream-json" {
		t.Errorf("--input-format should be stream-json: %v", args)
	}
	if argAfter(args, "--output-format") != "stream-json" {
		t.Errorf("--output-format should be stream-json: %v", args)
	}
}

func TestClaudeCLIArgs_ProbeStripsSessionFlags(t *testing.T) {
	fresh := claudeCLIArgs(ProviderSessionArgs{MCPPort: 1234}, false)
	probe := claudeCLIArgs(ProviderSessionArgs{MCPPort: 1234}, true)

	if !containsArg(fresh, "--include-partial-messages") {
		t.Errorf("live args should include --include-partial-messages: %v", fresh)
	}
	if containsArg(probe, "--include-partial-messages") {
		t.Errorf("probe args should NOT include --include-partial-messages: %v", probe)
	}
	if !containsArg(fresh, "--permission-prompt-tool") {
		t.Errorf("live args with MCPPort should include --permission-prompt-tool: %v", fresh)
	}
	if containsArg(probe, "--permission-prompt-tool") {
		t.Errorf("probe args should NOT include --permission-prompt-tool: %v", probe)
	}
}

func TestClaudeCLIArgs_SkipAllPermissions(t *testing.T) {
	on := claudeCLIArgs(ProviderSessionArgs{SkipAllPermissions: true}, false)
	if !containsArg(on, "--dangerously-skip-permissions") {
		t.Errorf("want --dangerously-skip-permissions when SkipAllPermissions=true: %v", on)
	}
	off := claudeCLIArgs(ProviderSessionArgs{SkipAllPermissions: false}, false)
	if containsArg(off, "--dangerously-skip-permissions") {
		t.Errorf("should not pass --dangerously-skip-permissions when off: %v", off)
	}
}

func TestClaudeCLIArgs_MCPConfigAndSettings(t *testing.T) {
	args := claudeCLIArgs(ProviderSessionArgs{MCPPort: 54321}, false)

	cfg := argAfter(args, "--mcp-config")
	if cfg == "" {
		t.Fatalf("--mcp-config missing: %v", args)
	}
	// Should be a JSON blob pointing at localhost:<port>.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("--mcp-config not JSON (%v): %s", err, cfg)
	}
	if !strings.Contains(cfg, "http://127.0.0.1:54321/") {
		t.Errorf("--mcp-config should embed the loopback URL with the port: %s", cfg)
	}

	if !containsArg(args, "--settings") {
		t.Errorf("--settings must be passed when MCPPort > 0: %v", args)
	}
	settings := argAfter(args, "--settings")
	if !strings.Contains(settings, "AskUserQuestion") || !strings.Contains(settings, "mcp__ask__ask_user_question") {
		t.Errorf("--settings payload should redirect AskUserQuestion to mcp bridge; got %s", settings)
	}

	// permission-prompt-tool should point at the approval_prompt MCP tool.
	if got := argAfter(args, "--permission-prompt-tool"); got != "mcp__ask__approval_prompt" {
		t.Errorf("--permission-prompt-tool=%q want mcp__ask__approval_prompt", got)
	}
}

func TestClaudeCLIArgs_NoMCPNoMCPConfig(t *testing.T) {
	args := claudeCLIArgs(ProviderSessionArgs{}, false)
	if containsArg(args, "--mcp-config") || containsArg(args, "--settings") {
		t.Errorf("without MCPPort we should not pass --mcp-config/--settings: %v", args)
	}
}

func TestClaudeCLIArgs_OllamaUsesOllamaModel(t *testing.T) {
	args := claudeCLIArgs(ProviderSessionArgs{Model: "ollama", OllamaModel: "llama3.2"}, false)
	if got := argAfter(args, "--model"); got != "llama3.2" {
		t.Errorf("--model for ollama want llama3.2, got %q; argv=%v", got, args)
	}
}

func TestClaudeCLIArgs_OllamaWithoutModelOmitsModelFlag(t *testing.T) {
	args := claudeCLIArgs(ProviderSessionArgs{Model: "ollama"}, false)
	if containsArg(args, "--model") {
		t.Errorf("--model should be absent when Model=ollama but OllamaModel empty: %v", args)
	}
}

func TestClaudeCLIArgs_ExplicitModelPassesThrough(t *testing.T) {
	args := claudeCLIArgs(ProviderSessionArgs{Model: "opus[1m]"}, false)
	if got := argAfter(args, "--model"); got != "opus[1m]" {
		t.Errorf("--model passed through: got %q", got)
	}
}

func TestClaudeCLIArgs_EmptyModelSkipsFlag(t *testing.T) {
	args := claudeCLIArgs(ProviderSessionArgs{}, false)
	if containsArg(args, "--model") {
		t.Errorf("empty Model should leave --model absent: %v", args)
	}
}

func TestClaudeCLIArgs_Effort(t *testing.T) {
	on := claudeCLIArgs(ProviderSessionArgs{Effort: "high"}, false)
	if got := argAfter(on, "--effort"); got != "high" {
		t.Errorf("--effort want high got %q", got)
	}
	off := claudeCLIArgs(ProviderSessionArgs{}, false)
	if containsArg(off, "--effort") {
		t.Errorf("empty Effort should skip --effort: %v", off)
	}
}

func TestClaudeCLIArgs_ResumeFlag(t *testing.T) {
	args := claudeCLIArgs(ProviderSessionArgs{SessionID: "s-1"}, false)
	if got := argAfter(args, "--resume"); got != "s-1" {
		t.Errorf("--resume want s-1 got %q", got)
	}
}

func TestClaudeCLIArgs_NeverPassesWorktreeFlag(t *testing.T) {
	// ask owns worktree lifecycle end-to-end now; claude's own
	// --worktree machinery must not be triggered regardless of input.
	dir := initGitRepo(t)
	t.Chdir(dir)
	for _, probe := range []bool{false, true} {
		for _, sid := range []string{"", "s-1"} {
			args := claudeCLIArgs(ProviderSessionArgs{
				Worktree: true, SessionID: sid,
			}, probe)
			if containsArg(args, "--worktree") {
				t.Errorf("probe=%v sid=%q: --worktree must never appear (ask manages worktrees): %v",
					probe, sid, args)
			}
		}
	}
}

func TestClaudeCLIArgs_ProbeDropsResume(t *testing.T) {
	probe := claudeCLIArgs(ProviderSessionArgs{SessionID: "s-1"}, true)
	if containsArg(probe, "--resume") {
		t.Errorf("probe args must not include --resume: %v", probe)
	}
}

func TestClaudeEnv_AlwaysSetsMCPTimeout(t *testing.T) {
	env := claudeEnv(ProviderSessionArgs{})
	if !hasEnv(env, "MCP_TIMEOUT=86400000") {
		t.Errorf("MCP_TIMEOUT must always be 86400000: %v", filterEnvKey(env, "MCP_TIMEOUT"))
	}
}

func TestClaudeEnv_OllamaInjectsEndpoint(t *testing.T) {
	env := claudeEnv(ProviderSessionArgs{Model: "ollama", OllamaHost: "localhost:11434"})
	if !hasEnvPrefix(env, "ANTHROPIC_BASE_URL=") {
		t.Errorf("expected ANTHROPIC_BASE_URL when Model=ollama")
	}
	if !hasEnv(env, "ANTHROPIC_AUTH_TOKEN=ollama") {
		t.Errorf("expected ANTHROPIC_AUTH_TOKEN=ollama")
	}
}

func TestClaudeEnv_NonOllamaOmitsOverride(t *testing.T) {
	env := claudeEnv(ProviderSessionArgs{Model: "opus"})
	if hasEnvPrefix(env, "ANTHROPIC_BASE_URL=") {
		t.Errorf("ANTHROPIC_BASE_URL should not be injected for non-ollama models")
	}
	if hasEnvPrefix(env, "ANTHROPIC_AUTH_TOKEN=") {
		// Host may already have one — only error if claudeEnv itself added it.
		// Check that it is not literally the ollama fingerprint value.
		for _, kv := range env {
			if kv == "ANTHROPIC_AUTH_TOKEN=ollama" {
				t.Errorf("claudeEnv should not inject ANTHROPIC_AUTH_TOKEN=ollama for non-ollama model")
			}
		}
	}
}

func hasEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func hasEnvPrefix(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func filterEnvKey(env []string, key string) []string {
	var out []string
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}

func TestOllamaBaseURL_PrependsScheme(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"localhost:11434", "http://localhost:11434"},
		{"http://example:1234", "http://example:1234"},
		{"HTTPS://HOST:9999", "HTTPS://HOST:9999"},
		{"  localhost:11434  ", "http://localhost:11434"},
	}
	for _, c := range cases {
		if got := ollamaBaseURL(c.in); got != c.want {
			t.Errorf("ollamaBaseURL(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestClaudeEnv_PreservesOSEnv(t *testing.T) {
	t.Setenv("ASK_TEST_MARKER_XYZ", "sentinel")
	env := claudeEnv(ProviderSessionArgs{})
	if !hasEnv(env, "ASK_TEST_MARKER_XYZ=sentinel") {
		t.Errorf("claudeEnv should preserve os.Environ; ASK_TEST_MARKER_XYZ missing")
	}
}

func TestClaudeCLIArgs_OrderStableForSameInput(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	a := claudeCLIArgs(ProviderSessionArgs{MCPPort: 7, Worktree: true, Effort: "high"}, false)
	b := claudeCLIArgs(ProviderSessionArgs{MCPPort: 7, Worktree: true, Effort: "high"}, false)
	if strings.Join(a, " ") != strings.Join(b, " ") {
		t.Errorf("claudeCLIArgs is nondeterministic:\na=%v\nb=%v", a, b)
	}
}

func TestClaudeCLIArgs_FreshSessionNoResume(t *testing.T) {
	dir := initGitRepo(t)
	t.Chdir(dir)
	args := claudeCLIArgs(ProviderSessionArgs{Worktree: true}, false)
	if containsArg(args, "--resume") {
		t.Errorf("fresh session should not add --resume: %v", args)
	}
}
