package tools

import (
	"context"
	"strings"
	"testing"
)

func TestOAuthWanted(t *testing.T) {
	cases := []struct {
		name string
		cfg  MCPServerConfig
		want bool
	}{
		{"stdio", MCPServerConfig{Command: "x"}, false},
		{"http explicit", MCPServerConfig{Type: MCPServerTypeHTTP, URL: "u", OAuth: true}, true},
		{"http no headers", MCPServerConfig{Type: MCPServerTypeHTTP, URL: "u"}, true},
		{"http with headers", MCPServerConfig{Type: MCPServerTypeHTTP, URL: "u", Headers: map[string]string{"Authorization": "Bearer x"}}, false},
		{"sse no headers", MCPServerConfig{Type: MCPServerTypeSSE, URL: "u"}, true},
	}
	for _, c := range cases {
		if got := oauthWanted(c.cfg); got != c.want {
			t.Errorf("%s: oauthWanted = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMCPManagerReconcile_AttachDetach(t *testing.T) {
	_, ts1 := newEchoMCPServer(t)
	_, ts2 := newEchoMCPServer(t)
	toolsChanged := 0
	statusChanged := 0
	mgr := NewMCPManager(1, func() bool { return true },
		func() { toolsChanged++ },
		func() { statusChanged++ },
		nil,
	)
	defer mgr.Close()
	ctx := context.Background()

	a := MCPServer{Name: "a", Cfg: MCPServerConfig{Type: MCPServerTypeHTTP, URL: ts1.URL}}
	b := MCPServer{Name: "b", Cfg: MCPServerConfig{Type: MCPServerTypeHTTP, URL: ts2.URL}}

	mgr.Reconcile(ctx, []MCPServer{a})
	if !hasStatus(mgr, "a", MCPStatusConnected) {
		t.Fatalf("a must be connected: %+v", mgr.Statuses())
	}
	if countToolPrefix(mgr, "mcp__a__") == 0 {
		t.Fatal("a's tools must be present")
	}

	mgr.Reconcile(ctx, []MCPServer{a, b})
	if !hasStatus(mgr, "b", MCPStatusConnected) || len(mgr.Statuses()) != 2 {
		t.Fatalf("both must be connected: %+v", mgr.Statuses())
	}

	mgr.Reconcile(ctx, []MCPServer{b})
	if hasStatus(mgr, "a", MCPStatusConnected) {
		t.Fatalf("a must be detached: %+v", mgr.Statuses())
	}
	if countToolPrefix(mgr, "mcp__a__") != 0 {
		t.Fatal("a's tools must be gone after detach")
	}
	if countToolPrefix(mgr, "mcp__b__") == 0 {
		t.Fatal("b's tools must remain")
	}
	if toolsChanged == 0 || statusChanged == 0 {
		t.Fatalf("callbacks must fire: tools=%d status=%d", toolsChanged, statusChanged)
	}
}

func hasStatus(m *MCPManager, name string, kind MCPServerStatusKind) bool {
	for _, s := range m.Statuses() {
		if s.Name == name && s.Kind == kind {
			return true
		}
	}
	return false
}

func countToolPrefix(m *MCPManager, prefix string) int {
	n := 0
	for _, tool := range m.Tools() {
		if strings.HasPrefix(tool.Name(), prefix) {
			n++
		}
	}
	return n
}
