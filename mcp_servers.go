package main

import (
	"encoding/json"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/tools"
)

type mcpServerConfig = tools.MCPServerConfig
type namedMCPServer = tools.NamedMCPServer

const (
	mcpServerTypeStdio = tools.MCPServerTypeStdio
	mcpServerTypeHTTP  = tools.MCPServerTypeHTTP
	mcpServerTypeSSE   = tools.MCPServerTypeSSE
)

func resolveMCPServers(cfg askConfig, cwd string) []namedMCPServer {
	var c config.Config
	b, err := json.Marshal(cfg)
	if err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return tools.ResolveMCPServers(c, cwd)
}

func mcpToolAllowed(c mcpServerConfig, tool string) bool {
	return tools.MCPToolAllowed(c, tool)
}
