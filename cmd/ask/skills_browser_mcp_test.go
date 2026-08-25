package main

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/tools"
	"golang.org/x/oauth2"
)

func mcpRowByName(rows []skillsRow, name string) (skillsRow, bool) {
	for _, r := range rows {
		if r.kind == skillsRowMCP && r.mcp.name == name {
			return r, true
		}
	}
	return skillsRow{}, false
}

func saveMCPServers(t *testing.T, servers map[string]config.MCPServerConfig) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MCPServers = servers
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsBrowser_MCPGroupListed(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	saveMCPServers(t, map[string]config.MCPServerConfig{
		"alpha": {URL: "https://alpha/mcp"},
		"beta":  {Command: "betacmd", Disabled: true},
	})
	m = m.openSkillsBrowser()
	rows := m.skillsBrowser.rows()

	header := false
	for _, r := range rows {
		if r.kind == skillsRowHeader && r.title == "MCP Servers" {
			header = true
		}
	}
	if !header {
		t.Fatalf("Installed lens must contain an MCP Servers group: %v", skillsRowTitles(rows))
	}
	alpha, ok := mcpRowByName(rows, "alpha")
	if !ok || alpha.mcp.disabled || alpha.mcp.transport != tools.MCPServerTypeHTTP {
		t.Fatalf("alpha row wrong: %+v ok=%v", alpha.mcp, ok)
	}
	beta, ok := mcpRowByName(rows, "beta")
	if !ok || !beta.mcp.disabled || beta.mcp.transport != tools.MCPServerTypeStdio {
		t.Fatalf("beta row wrong: %+v ok=%v", beta.mcp, ok)
	}
}

func TestSkillsBrowser_ToggleWritesOverrideAndBroadcasts(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	saveMCPServers(t, map[string]config.MCPServerConfig{"alpha": {URL: "https://alpha/mcp"}})
	m = m.openSkillsBrowser()
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowMCP && r.mcp.name == "alpha" })

	// space opens the user/project scope chooser.
	m = stepKey(t, m, tea.KeyPressMsg{Code: ' ', Text: " "})
	if m.skillsBrowser.editor == nil || m.skillsBrowser.editor.kind != skillsEditorScope {
		t.Fatalf("space must open the scope chooser: %+v", m.skillsBrowser.editor)
	}
	// choose "user" scope.
	mi, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	m = mi.(model)
	m, next := runSkillsOp(t, m, cmd)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MCPDisabled["alpha"] {
		t.Fatalf("toggle must persist a user-scope disable override: %+v", cfg.MCPDisabled)
	}
	broadcast := false
	for _, msg := range drainBatch(t, next) {
		if _, ok := msg.(mcpServersChangedMsg); ok {
			broadcast = true
		}
	}
	if !broadcast {
		t.Fatal("toggle must broadcast mcpServersChangedMsg so live sessions reconcile")
	}
	// The rebuilt row now reads as disabled.
	if row, ok := mcpRowByName(m.skillsBrowser.rows(), "alpha"); !ok || !row.mcp.disabled {
		t.Fatalf("row must reflect the disable: %+v ok=%v", row.mcp, ok)
	}
}

func TestSkillsBrowser_SignOutForgetsToken(t *testing.T) {
	m, _, _ := skillsBrowserFixture(t)
	saveMCPServers(t, map[string]config.MCPServerConfig{"alpha": {Type: tools.MCPServerTypeHTTP, URL: "https://alpha/mcp"}})
	path, err := tools.MCPOAuthTokenPath("https://alpha/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.SaveMCPOAuthToken(path, &oauth2.Token{AccessToken: "at", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if !mcpServerAuthorized("https://alpha/mcp") {
		t.Fatal("precondition: token must be stored")
	}
	m = m.openSkillsBrowser()
	m = moveSkillsCursorTo(t, m, func(r skillsRow) bool { return r.kind == skillsRowMCP && r.mcp.name == "alpha" })
	mi, cmd := m.Update(pressKey('d', tea.ModCtrl))
	m = mi.(model)
	m, _ = runSkillsOp(t, m, cmd)
	if mcpServerAuthorized("https://alpha/mcp") {
		t.Fatal("sign out must forget the stored token")
	}
}
