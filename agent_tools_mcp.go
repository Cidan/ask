package main

import (
	"context"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

type agentMCPServer = tools.MCPServer
type mcpManager struct {
	*tools.MCPManager
}

func newMCPManager(tabID int, imagesOK func() bool, onToolsChanged func()) *mcpManager {
	return &mcpManager{
		MCPManager: tools.NewMCPManager(tabID, imagesOK, onToolsChanged, globalTUIInteractionHandler),
	}
}

func (m *mcpManager) attach(ctx context.Context, srv agentMCPServer) error {
	return m.Attach(ctx, srv)
}

func (m *mcpManager) attachAll(ctx context.Context, srvs []agentMCPServer) {
	m.AttachAll(ctx, srvs)
}

func (m *mcpManager) tools() []fantasy.AgentTool {
	return m.Tools()
}

func (m *mcpManager) close() {
	m.Close()
}
