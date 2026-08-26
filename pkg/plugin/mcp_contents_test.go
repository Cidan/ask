package plugin

import (
	"path/filepath"
	"testing"
)

func TestResolveContents_MCPFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"root":{"url":"https://e/mcp"}}}`)
	writeFile(t, filepath.Join(dir, "mcps", "a.json"), `{"mcpServers":{"a":{"url":"https://a/mcp"}}}`)
	writeFile(t, filepath.Join(dir, "mcps", "b.json"), `{"mcpServers":{"b":{"url":"https://b/mcp"}}}`)
	writeFile(t, filepath.Join(dir, "mcps", "notes.txt"), "ignored")

	c := ResolveContents(dir, nil, nil)
	want := []string{
		filepath.Join(dir, ".mcp.json"),
		filepath.Join(dir, "mcps", "a.json"),
		filepath.Join(dir, "mcps", "b.json"),
	}
	if len(c.MCPFiles) != len(want) {
		t.Fatalf("MCPFiles = %v, want %v", c.MCPFiles, want)
	}
	for i, w := range want {
		if c.MCPFiles[i] != w {
			t.Errorf("MCPFiles[%d] = %s, want %s", i, c.MCPFiles[i], w)
		}
	}
}

func TestResolveContents_MCPManifestInlineObjectHonored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"),
		`{"name":"p","mcpServers":{"x":{"url":"https://e/mcp"}}}`)
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"root":{"url":"https://e/mcp"}}}`)

	m, err := ReadPluginManifest(dir)
	if err != nil {
		t.Fatalf("an inline mcpServers object must not break manifest parsing: %v", err)
	}
	if m == nil || m.Name != "p" {
		t.Fatalf("manifest: %+v", m)
	}
	if string(m.MCPServers.Inline) != `{"x":{"url":"https://e/mcp"}}` {
		t.Errorf("manifest inline mcpServers not preserved: %q", string(m.MCPServers.Inline))
	}
	c := ResolveContents(dir, nil, m)
	found := false
	for _, f := range c.MCPFiles {
		if f == filepath.Join(dir, ".mcp.json") {
			found = true
		}
	}
	if !found {
		t.Errorf("root .mcp.json must still resolve: %v", c.MCPFiles)
	}
	if len(c.InlineMCP) != 1 || string(c.InlineMCP[0]) != `{"x":{"url":"https://e/mcp"}}` {
		t.Errorf("inline mcpServers must be surfaced on Contents: %v", c.InlineMCP)
	}
}

// TestResolveContents_MCPInlineOnly is the Slack-style plugin: the server is
// declared inline in the marketplace entry with no .mcp.json file on disk.
// Before the fix this dropped the server entirely.
func TestResolveContents_MCPInlineOnly(t *testing.T) {
	dir := t.TempDir()
	entry := &Entry{Name: "slack"}
	if err := jsonUnmarshalInto(`{"slack":{"type":"http","url":"https://mcp.slack.com/mcp"}}`, &entry.MCPServers); err != nil {
		t.Fatal(err)
	}
	c := ResolveContents(dir, entry, nil)
	if len(c.MCPFiles) != 0 {
		t.Errorf("no files were written, MCPFiles must be empty: %v", c.MCPFiles)
	}
	if len(c.InlineMCP) != 1 {
		t.Fatalf("inline-only entry must surface exactly one inline object: %v", c.InlineMCP)
	}
	if string(c.InlineMCP[0]) != `{"slack":{"type":"http","url":"https://mcp.slack.com/mcp"}}` {
		t.Errorf("inline object mangled: %q", string(c.InlineMCP[0]))
	}
	if c.Empty() {
		t.Error("an inline-only MCP plugin must not report empty contents")
	}
}

func jsonUnmarshalInto(s string, f *MCPServersField) error {
	return f.UnmarshalJSON([]byte(s))
}

func TestResolveContents_MCPManifestPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"),
		`{"name":"p","mcpServers":["extra/servers.json"]}`)
	writeFile(t, filepath.Join(dir, "extra", "servers.json"), `{"mcpServers":{"x":{"url":"https://e/mcp"}}}`)

	m, err := ReadPluginManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := ResolveContents(dir, nil, m)
	want := filepath.Join(dir, "extra", "servers.json")
	found := false
	for _, f := range c.MCPFiles {
		if f == want {
			found = true
		}
	}
	if !found {
		t.Errorf("manifest mcpServers path must resolve: %v", c.MCPFiles)
	}
}
