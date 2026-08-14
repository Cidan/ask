package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

type agentBashParams = tools.BashParams
type agentJobOutputParams = tools.JobOutputParams
type agentJobKillParams = tools.JobKillParams

var agentRunShell = tools.RunShell

func agentBashTool(env *agentToolEnv) fantasy.AgentTool {
	tools.RunShell = agentRunShell
	return tools.BashTool(env)
}
func agentJobOutputTool(env *agentToolEnv) fantasy.AgentTool { return tools.JobOutputTool(env) }
func agentJobKillTool(env *agentToolEnv) fantasy.AgentTool   { return tools.JobKillTool(env) }

func agentSafeShellCommand(command string) bool { return tools.SafeShellCommand(command) }
func validateSudoCommand(command string) error  { return tools.ValidateSudoCommand(command) }
