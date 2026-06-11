package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMCPServerConfig_EffectiveType(t *testing.T) {
	cases := []struct {
		cfg  mcpServerConfig
		want string
	}{
		{mcpServerConfig{Type: "stdio"}, mcpServerTypeStdio},
		{mcpServerConfig{Type: "http"}, mcpServerTypeHTTP},
		{mcpServerConfig{Type: "sse"}, mcpServerTypeSSE},
		{mcpServerConfig{Command: "npx"}, mcpServerTypeStdio},
		{mcpServerConfig{URL: "https://x"}, mcpServerTypeHTTP},
		{mcpServerConfig{Type: "bogus", Command: "npx"}, mcpServerTypeStdio},
	}
	for _, c := range cases {
		if got := c.cfg.effectiveType(); got != c.want {
			t.Errorf("effectiveType(%+v) = %q want %q", c.cfg, got, c.want)
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
		if got := expandMCPString(in); got != want {
			t.Errorf("expandMCPString(%q) = %q want %q", in, got, want)
		}
	}
}

func TestMCPServerConfig_Expanded(t *testing.T) {
	t.Setenv("ASK_TEST_TOKEN", "tok")
	c := mcpServerConfig{
		Command: "${ASK_TEST_MISSING:-npx}",
		Args:    []string{"-y", "server-${ASK_TEST_TOKEN}"},
		Env:     map[string]string{"KEY": "${ASK_TEST_TOKEN}"},
		URL:     "https://x/${ASK_TEST_TOKEN}",
		Headers: map[string]string{"Authorization": "Bearer ${ASK_TEST_TOKEN}"},
	}
	e := c.expanded()
	if e.Command != "npx" || e.Args[1] != "server-tok" || e.Env["KEY"] != "tok" ||
		e.URL != "https://x/tok" || e.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("expansion wrong: %+v", e)
	}
	// The original must stay untouched (expanded returns a copy).
	if c.Args[1] != "server-${ASK_TEST_TOKEN}" || c.Env["KEY"] != "${ASK_TEST_TOKEN}" {
		t.Errorf("expanded mutated its receiver: %+v", c)
	}
}

func TestResolveMCPServers_LayeringAndFilters(t *testing.T) {
	isolateHome(t)
	cwd := initGitRepo(t)

	// Layer 1: .mcp.json at the project root.
	dot := map[string]any{"mcpServers": map[string]any{
		"docs":   map[string]any{"type": "http", "url": "https://docs.example/mcp"},
		"legacy": map[string]any{"command": "old-server"},
	}}
	data, _ := json.Marshal(dot)
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Layer 2: user-global config overrides "docs" and adds "search".
	// Layer 3: project config disables "legacy" and adds "issues".
	cfg := askConfig{
		MCPServers: map[string]mcpServerConfig{
			"docs":   {Type: "http", URL: "https://global.example/mcp"},
			"search": {Command: "search-server"},
			"junk":   {}, // neither command nor url → dropped
		},
	}
	cfg = upsertProjectConfig(cfg, cwd, projectConfig{
		MCPServers: map[string]mcpServerConfig{
			"legacy": {Command: "old-server", Disabled: true},
			"issues": {Type: "http", URL: "https://issues.example/mcp"},
		},
	})

	got := resolveMCPServers(cfg, cwd)
	byName := map[string]mcpServerConfig{}
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
	// Stable name order.
	if got[0].Name != "docs" || got[1].Name != "issues" || got[2].Name != "search" {
		t.Errorf("order must be name-sorted: %v %v %v", got[0].Name, got[1].Name, got[2].Name)
	}
}

func TestResolveMCPServers_MalformedDotMCPJSONIgnored(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveMCPServers(askConfig{}, cwd); len(got) != 0 {
		t.Errorf("malformed .mcp.json must yield nothing: %+v", got)
	}
}

func TestMCPToolAllowed(t *testing.T) {
	c := mcpServerConfig{}
	if !mcpToolAllowed(c, "anything") {
		t.Error("no filters allows everything")
	}
	c = mcpServerConfig{EnabledTools: []string{"a", "b"}}
	if !mcpToolAllowed(c, "a") || mcpToolAllowed(c, "c") {
		t.Error("allowlist must gate")
	}
	c = mcpServerConfig{EnabledTools: []string{"a", "b"}, DisabledTools: []string{"b"}}
	if !mcpToolAllowed(c, "a") || mcpToolAllowed(c, "b") {
		t.Error("denylist applies after allowlist")
	}
	c = mcpServerConfig{DisabledTools: []string{"x"}}
	if mcpToolAllowed(c, "x") || !mcpToolAllowed(c, "y") {
		t.Error("denylist alone must gate")
	}
}
