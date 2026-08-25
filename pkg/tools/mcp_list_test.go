package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/plugin"
)

func writeMCPTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListMCPServers_PrecedenceAndOrigins(t *testing.T) {
	cwd := t.TempDir()
	// project .mcp.json defines "dup" and "filed".
	writeMCPTestFile(t, filepath.Join(cwd, ".mcp.json"),
		`{"mcpServers":{"dup":{"url":"https://file/dup"},"filed":{"url":"https://file/only"}}}`)

	cfg := config.Config{
		MCPServers: map[string]config.MCPServerConfig{
			"dup":   {URL: "https://user/dup"},
			"userd": {URL: "https://user/only"},
		},
		Projects: map[string]config.ProjectConfig{
			config.ProjectKey(cwd): {
				MCPServers: map[string]config.MCPServerConfig{
					"dup": {URL: "https://project/dup"},
				},
			},
		},
	}

	got := map[string]ResolvedMCPServer{}
	for _, r := range ListMCPServers(cfg, cwd) {
		got[r.Name] = r
	}
	if got["dup"].Origin != MCPOriginProject || got["dup"].Config.URL != "https://project/dup" {
		t.Errorf("per-project config must win for dup: %+v", got["dup"])
	}
	if got["filed"].Origin != MCPOriginProjectFile {
		t.Errorf("filed origin = %v, want .mcp.json", got["filed"].Origin)
	}
	if got["userd"].Origin != MCPOriginUser {
		t.Errorf("userd origin = %v, want user", got["userd"].Origin)
	}
	// Sorted by name.
	list := ListMCPServers(cfg, cwd)
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Fatalf("not sorted: %v", list)
		}
	}
}

func TestListMCPServers_OverridesBothScopes(t *testing.T) {
	cwd := t.TempDir()
	cfg := config.Config{
		MCPServers:  map[string]config.MCPServerConfig{"s": {URL: "https://s/mcp"}},
		MCPDisabled: map[string]bool{"s": true},
	}
	if !ListMCPServers(cfg, cwd)[0].Disabled {
		t.Fatal("user override must disable s")
	}
	// Project override wins over the user override.
	cfg.Projects = map[string]config.ProjectConfig{
		config.ProjectKey(cwd): {MCPDisabled: map[string]bool{"s": false}},
	}
	if ListMCPServers(cfg, cwd)[0].Disabled {
		t.Fatal("project override (false) must win over user override (true)")
	}
}

func TestResolveMCPServers_DropsDisabledAndExpands(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("MCP_TOKEN", "sekret")
	cfg := config.Config{
		MCPServers: map[string]config.MCPServerConfig{
			"on":  {URL: "https://on/${MCP_TOKEN}"},
			"off": {URL: "https://off/mcp", Disabled: true},
		},
	}
	out := ResolveMCPServers(cfg, cwd)
	if len(out) != 1 || out[0].Name != "on" {
		t.Fatalf("disabled must be dropped: %+v", out)
	}
	if out[0].Config.URL != "https://on/sekret" {
		t.Errorf("URL must be env-expanded: %s", out[0].Config.URL)
	}
}

func TestPluginMCPServers_ExpandsPluginRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkt := filepath.Join(home, "fixture")
	writeMCPTestFile(t, filepath.Join(mkt, ".claude-plugin", "marketplace.json"),
		`{"name":"mkt","owner":{"name":"t"},"plugins":[{"name":"p","source":"./plugins/p","description":"d"}]}`)
	writeMCPTestFile(t, filepath.Join(mkt, "plugins", "p", ".claude-plugin", "plugin.json"), `{"name":"p"}`)
	writeMCPTestFile(t, filepath.Join(mkt, "plugins", "p", "mcps", "x.json"),
		`{"mcpServers":{"x":{"command":"${CLAUDE_PLUGIN_ROOT}/bin/x","args":["${CLAUDE_PLUGIN_ROOT}/d"]}}}`)

	ctx := context.Background()
	if _, err := plugin.AddMarketplace(ctx, cwd, mkt, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InstallPlugin(ctx, cwd, plugin.Ref{Plugin: "p", Marketplace: "mkt"}, plugin.ScopeUser); err != nil {
		t.Fatal(err)
	}

	servers := PluginMCPServers(cwd)
	var x *pluginMCPServer
	for i := range servers {
		if servers[i].Name == "x" {
			x = &servers[i]
		}
	}
	if x == nil {
		t.Fatalf("plugin server x not found: %+v", servers)
	}
	if x.Plugin != "p@mkt" {
		t.Errorf("plugin ref = %q, want p@mkt", x.Plugin)
	}
	if strings.Contains(x.Config.Command, "CLAUDE_PLUGIN_ROOT") || !strings.HasSuffix(x.Config.Command, "/bin/x") {
		t.Errorf("command must expand ${CLAUDE_PLUGIN_ROOT}: %q", x.Config.Command)
	}
	if len(x.Config.Args) != 1 || strings.Contains(x.Config.Args[0], "CLAUDE_PLUGIN_ROOT") {
		t.Errorf("args must expand ${CLAUDE_PLUGIN_ROOT}: %v", x.Config.Args)
	}
}
