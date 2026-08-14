package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

type agentReadParams = tools.ReadParams
type agentWriteParams = tools.WriteParams
type agentEditParams = tools.EditParams

func agentReadTool(env *agentToolEnv) fantasy.AgentTool  { return tools.ReadTool(env) }
func agentWriteTool(env *agentToolEnv) fantasy.AgentTool { return tools.WriteTool(env) }
func agentEditTool(env *agentToolEnv) fantasy.AgentTool  { return tools.EditTool(env) }
