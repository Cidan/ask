package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/plugin"
)

func TestMCPServerConfig_EffectiveType(t *testing.T) {
	cases := []struct {
		cfg  MCPServerConfig
		want string
	}{
		{MCPServerConfig{Type: "stdio"}, MCPServerTypeStdio},
		{MCPServerConfig{Type: "http"}, MCPServerTypeHTTP},
		{MCPServerConfig{Type: "sse"}, MCPServerTypeSSE},
		{MCPServerConfig{Command: "npx"}, MCPServerTypeStdio},
		{MCPServerConfig{URL: "https://x"}, MCPServerTypeHTTP},
		{MCPServerConfig{Type: "bogus", Command: "npx"}, MCPServerTypeStdio},
	}
	for _, c := range cases {
		if got := c.cfg.EffectiveType(); got != c.want {
			t.Errorf("EffectiveType(%+v) = %q want %q", c.cfg, got, c.want)
		}
	}
}

func TestExpandMCPString(t *testing.T) {
	t.Setenv("ASK_TEST_TOKEN", "sekrit")
	t.Setenv("ASK_TEST_EMPTY", "")
	cases := map[string]string{
		"plain":                          "plain",
		"Bearer ${ASK_TEST_TOKEN}":       "Bearer sekrit",
		"$ASK_TEST_TOKEN":                "sekrit",
		"${ASK_TEST_MISSING:-fallback}":  "fallback",
		"${ASK_TEST_EMPTY:-fallback}":    "fallback",
		"${ASK_TEST_TOKEN:-unused}":      "sekrit",
		"no dollars at all, fast path!!": "no dollars at all, fast path!!",
	}
	for in, want := range cases {
		if got := ExpandMCPString(in); got != want {
			t.Errorf("ExpandMCPString(%q) = %q want %q", in, got, want)
		}
	}
}

func TestMCPServerConfig_Expanded(t *testing.T) {
	t.Setenv("ASK_TEST_TOKEN", "tok")
	c := MCPServerConfig{
		Command: "${ASK_TEST_MISSING:-npx}",
		Args:    []string{"-y", "server-${ASK_TEST_TOKEN}"},
		Env:     map[string]string{"KEY": "${ASK_TEST_TOKEN}"},
		URL:     "https://x/${ASK_TEST_TOKEN}",
		Headers: map[string]string{"Authorization": "Bearer ${ASK_TEST_TOKEN}"},
	}
	e := c.Expanded()
	if e.Command != "npx" || e.Args[1] != "server-tok" || e.Env["KEY"] != "tok" ||
		e.URL != "https://x/tok" || e.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("expansion wrong: %+v", e)
	}
	if c.Args[1] != "server-${ASK_TEST_TOKEN}" || c.Env["KEY"] != "${ASK_TEST_TOKEN}" {
		t.Errorf("expanded mutated its receiver: %+v", c)
	}
}

func TestResolveMCPServers_LayeringAndFilters(t *testing.T) {
	cwd := t.TempDir()
	os.MkdirAll(filepath.Join(cwd, ".git"), 0o755)

	dot := map[string]any{"mcpServers": map[string]any{
		"docs":   map[string]any{"type": "http", "url": "https://docs.example/mcp"},
		"legacy": map[string]any{"command": "old-server"},
	}}
	data, _ := json.Marshal(dot)
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		MCPServers: map[string]config.MCPServerConfig{
			"docs":   {Type: "http", URL: "https://global.example/mcp"},
			"search": {Command: "search-server"},
			"junk":   {},
		},
		Projects: map[string]config.ProjectConfig{
			cwd: {
				MCPServers: map[string]config.MCPServerConfig{
					"legacy": {Command: "old-server", Disabled: true},
					"issues": {Type: "http", URL: "https://issues.example/mcp"},
				},
			},
		},
	}

	got := ResolveMCPServers(cfg, cwd)
	byName := map[string]MCPServerConfig{}
	for _, s := range got {
		byName[s.Name] = s.Config
	}
	if len(got) != 3 {
		t.Fatalf("want docs+search+issues, got %d: %+v", len(got), byName)
	}
	if byName["docs"].URL != "https://global.example/mcp" {
		t.Errorf("global must override .mcp.json: %+v", byName["docs"])
	}
	if _, ok := byName["legacy"]; ok {
		t.Error("project Disabled must remove a lower-layer server")
	}
	if _, ok := byName["junk"]; ok {
		t.Error("entries with neither command nor url must be dropped")
	}
	if got[0].Name != "docs" || got[1].Name != "issues" || got[2].Name != "search" {
		t.Errorf("order must be name-sorted: %v %v %v", got[0].Name, got[1].Name, got[2].Name)
	}
}

func TestMCPServersFromContents_InlineOnly(t *testing.T) {
	// The Slack-style plugin: server declared inline, no .mcp.json file.
	c := plugin.Contents{
		InlineMCP: []json.RawMessage{
			json.RawMessage(`{"slack":{"type":"http","url":"https://mcp.slack.com/mcp"}}`),
		},
	}
	servers := mcpServersFromContents(c)
	got, ok := servers["slack"]
	if !ok {
		t.Fatalf("inline slack server not decoded: %+v", servers)
	}
	if got.EffectiveType() != MCPServerTypeHTTP || got.URL != "https://mcp.slack.com/mcp" {
		t.Errorf("inline server decoded wrong: %+v", got)
	}
	if n := PluginContentsMCPCount(c); n != 1 {
		t.Errorf("PluginContentsMCPCount = %d, want 1", n)
	}
}

func TestMCPServersFromContents_FileAndInlineMerge(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "servers.json")
	if err := os.WriteFile(file, []byte(`{"mcpServers":{"a":{"url":"https://file/a"},"shared":{"url":"https://file/shared"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := plugin.Contents{
		MCPFiles: []string{file},
		InlineMCP: []json.RawMessage{
			json.RawMessage(`{"b":{"url":"https://inline/b"},"shared":{"url":"https://inline/shared"}}`),
		},
	}
	servers := mcpServersFromContents(c)
	if len(servers) != 3 {
		t.Fatalf("want a+b+shared, got %d: %+v", len(servers), servers)
	}
	if servers["a"].URL != "https://file/a" || servers["b"].URL != "https://inline/b" {
		t.Errorf("file and inline servers must both appear: %+v", servers)
	}
	if servers["shared"].URL != "https://inline/shared" {
		t.Errorf("inline must win on a name clash: %+v", servers["shared"])
	}
	if n := PluginContentsMCPCount(c); n != 3 {
		t.Errorf("PluginContentsMCPCount = %d, want 3", n)
	}
}

func TestMCPToolAllowed(t *testing.T) {
	c := MCPServerConfig{}
	if !MCPToolAllowed(c, "anything") {
		t.Error("no filters allows everything")
	}
	c = MCPServerConfig{EnabledTools: []string{"a", "b"}}
	if !MCPToolAllowed(c, "a") || MCPToolAllowed(c, "c") {
		t.Error("allowlist must gate")
	}
	c = MCPServerConfig{EnabledTools: []string{"a", "b"}, DisabledTools: []string{"b"}}
	if !MCPToolAllowed(c, "a") || MCPToolAllowed(c, "b") {
		t.Error("denylist applies after allowlist")
	}
	c = MCPServerConfig{DisabledTools: []string{"x"}}
	if MCPToolAllowed(c, "x") || !MCPToolAllowed(c, "y") {
		t.Error("denylist alone must gate")
	}
}
