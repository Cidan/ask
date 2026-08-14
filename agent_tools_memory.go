package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/memory"
)

func agentMemoryIndexTool(env *agentToolEnv) fantasy.AgentTool {
	return memory.MemoryIndexTool(env.Cwd, env.RequestApproval)
}
