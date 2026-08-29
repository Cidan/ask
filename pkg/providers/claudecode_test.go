package providers

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/config"
)

func TestClaudeCode_Identity(t *testing.T) {
	var p ClaudeCode
	if p.ID() != "claude-code" || p.DisplayName() != "Claude Code" {
		t.Fatalf("identity = %q / %q", p.ID(), p.DisplayName())
	}
	if p.DefaultModel() != "default" {
		t.Errorf("default model = %q", p.DefaultModel())
	}
	if got := p.EffortOptions(); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("effort options = %v", got)
	}
	if _, ok := SecretField(p); ok {
		t.Error("Claude Code has no secret field — auth lives in the binary")
	}
}

func TestClaudeCode_RegisteredAndDefaultUnchanged(t *testing.T) {
	if _, ok := Get(ClaudeCodeProviderID); !ok {
		t.Fatal("claude-code must be registered")
	}
	// Adding it must not move the default provider off Vertex.
	if DefaultProviderID() != VertexProviderID {
		t.Errorf("default provider = %q, want vertex", DefaultProviderID())
	}
}

func TestClaudeCode_Configured_ChecksBinaryOnPath(t *testing.T) {
	var p ClaudeCode
	// A binary that certainly does not exist.
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "definitely-not-a-real-binary-xyz")
	if p.Configured(pc) {
		t.Error("Configured must be false when the binary is not on PATH")
	}
	// A binary that always exists on a POSIX test host.
	pc = config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")
	if !p.Configured(pc) {
		t.Error("Configured must be true when the binary resolves")
	}
}

func TestClaudeCode_BuildModel_FailsFastWithoutBinary(t *testing.T) {
	var p ClaudeCode
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "definitely-not-a-real-binary-xyz")
	if _, err := p.BuildModel(context.Background(), pc, "opus"); err == nil {
		t.Fatal("BuildModel must fail when the binary is missing")
	}
}

func TestClaudeCode_BuildModel_ReturnsCloser(t *testing.T) {
	var p ClaudeCode
	pc := config.ProviderConfig{}.WithField(ClaudeCodeFieldBinary, "sh")
	m, err := p.BuildModel(context.Background(), pc, "opus")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}
	if m.Name() != "opus" {
		t.Errorf("model name = %q, want opus", m.Name())
	}
	// No child has been spawned, so Close is a no-op that must not error.
	if c, ok := m.(interface{ Close() error }); !ok {
		t.Fatal("the model must be an io.Closer")
	} else if err := c.Close(); err != nil {
		t.Errorf("Close on an unspawned model: %v", err)
	}
}

func TestClaudeCode_CanonicalModelID(t *testing.T) {
	var p ClaudeCode
	if got := p.CanonicalModelID("", ""); got != "default" {
		t.Errorf("empty -> %q, want default", got)
	}
	if got := p.CanonicalModelID("", "opus"); got != "opus" {
		t.Errorf("empty with fallback -> %q, want opus", got)
	}
	// An unrecognized id passes through — the CLI accepts full names and aliases.
	if got := p.CanonicalModelID("claude-opus-5", ""); got != "claude-opus-5" {
		t.Errorf("full name -> %q", got)
	}
}

func TestClaudeCode_CatalogAndLimits(t *testing.T) {
	var p ClaudeCode
	if !p.SupportsImages("opus") {
		t.Error("Claude models support images")
	}
	if got := p.ContextWindow("opus[1m]"); got != 1_000_000 {
		t.Errorf("opus[1m] context = %d, want 1M", got)
	}
	if got := p.ContextWindow("haiku"); got != 200_000 {
		t.Errorf("haiku context = %d, want 200k", got)
	}
	opts := p.ModelOptions()
	if len(opts) == 0 || opts[0] != "default" {
		t.Errorf("model options = %v, want default first", opts)
	}
}

func TestCCArgv_LocksDownClaudeContext(t *testing.T) {
	argv := ccArgv("opus", "high", "/tmp/ask-claude-system-abc.txt", false)
	joined := strings.Join(argv, " ")
	// The three flags that overwrite Claude's tools with ask's.
	for _, want := range []string{
		"--tools ", "--mcp-config", "--strict-mcp-config",
		"--allowedTools mcp__ask", "--setting-sources ",
		"--no-session-persistence",
		"--system-prompt-file /tmp/ask-claude-system-abc.txt",
		"--model opus", "--effort high",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q; got %v", want, argv)
		}
	}
	// The prompt must ride --system-prompt-file, never inline as --system-prompt,
	// or a large prompt overflows the OS argv limit.
	if i := indexOf(argv, "--system-prompt"); i >= 0 {
		t.Errorf("argv must not carry the inline --system-prompt flag; got %v", argv)
	}
	if i := indexOf(argv, "--system-prompt-file"); i < 0 || argv[i+1] != "/tmp/ask-claude-system-abc.txt" {
		t.Errorf("--system-prompt-file must be followed by the temp path; got %v", argv)
	}
	// autoMemory is turned off in --settings.
	if !strings.Contains(joined, `"autoMemoryEnabled":false`) {
		t.Errorf("argv must disable auto memory; got %v", argv)
	}
	// --tools is the empty string (disable all built-ins).
	if i := indexOf(argv, "--tools"); i < 0 || argv[i+1] != "" {
		t.Errorf("--tools must be followed by an empty string; got %v", argv)
	}
	// The sdk MCP server is declared.
	mcpIdx := indexOf(argv, "--mcp-config")
	if mcpIdx < 0 {
		t.Fatal("--mcp-config missing")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(argv[mcpIdx+1]), &cfg); err != nil {
		t.Fatalf("mcp-config is not valid JSON: %v", err)
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	ask, _ := servers["ask"].(map[string]any)
	if ask["type"] != "sdk" {
		t.Errorf("ask server must be sdk type; got %v", ask)
	}
}

func TestWriteClaudeSystemPromptFile(t *testing.T) {
	const want = "You are ask.\nA sizeable system prompt.\n"
	path, err := writeClaudeSystemPromptFile(want)
	if err != nil {
		t.Fatalf("writeClaudeSystemPromptFile: %v", err)
	}
	defer os.Remove(path)
	if path == "" {
		t.Fatal("returned path is empty")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if string(got) != want {
		t.Errorf("temp file content = %q, want %q", got, want)
	}
	// Each invocation gets a distinct random id so concurrent children never
	// share a file.
	path2, err := writeClaudeSystemPromptFile(want)
	if err != nil {
		t.Fatalf("writeClaudeSystemPromptFile (2): %v", err)
	}
	defer os.Remove(path2)
	if path2 == path {
		t.Errorf("two invocations returned the same path %q", path)
	}
}

func TestCCArgv_DefaultModelOmitsModelFlag(t *testing.T) {
	argv := ccArgv("default", "", "/tmp/ask-claude-system-xyz.txt", false)
	if indexOf(argv, "--model") >= 0 {
		t.Errorf("the default model must not pass --model; got %v", argv)
	}
	if indexOf(argv, "--effort") >= 0 {
		t.Errorf("empty effort must not pass --effort; got %v", argv)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
