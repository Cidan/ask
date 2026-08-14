package main

import (
	"context"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

type agentGlobParams = tools.GlobParams
type agentGrepParams = tools.GrepParams
type agentLsParams = tools.LsParams

var agentRgPath = tools.RgPath

func agentGlobTool(env *agentToolEnv) fantasy.AgentTool { return tools.GlobTool(env) }
func agentGrepTool(env *agentToolEnv) fantasy.AgentTool { return tools.GrepTool(env) }
func agentLsTool(env *agentToolEnv) fantasy.AgentTool   { return tools.LsTool(env) }

func agentGlobMatch(pattern, rel string) bool { return tools.GlobMatch(pattern, rel) }
func expandBraces(s string) []string          { return tools.ExpandBraces(s) }
func agentGrepRun(ctx context.Context, rgPath string, p agentGrepParams, root string) (string, string) {
	return tools.GrepRun(ctx, rgPath, p, root)
}
