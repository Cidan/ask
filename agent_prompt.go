package main

import (
	"github.com/Cidan/ask/pkg/engine"
)

const agentCoderPrompt = engine.AgentCoderPrompt

var (
	agentContextFileNames = engine.AgentContextFileNames
	agentContextFileCap   = engine.AgentContextFileCap
	agentGitStatus        = func(cwd string) string { return engine.AgentGitStatus(cwd) }
	agentContextFiles     = engine.AgentContextFiles
)

func buildAgentSystemPrompt(args ProviderSessionArgs) string {
	return engine.BuildSystemPrompt(engine.PromptOptions{
		Cwd:         args.Cwd,
		InWorkflow:  args.InWorkflow,
		GitStatusFn: agentGitStatus,
	})
}
