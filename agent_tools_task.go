package main

import (
	"context"
	"strings"

	"charm.land/fantasy"
)

const agentTaskToolDescription = `Launch a read-only research sub-agent for broad searches across the codebase. Give it one self-contained question or search task; it explores with read/glob/grep/ls and returns a final report. Use it when you need a fan-out search whose intermediate results would clutter your context — not for single lookups you can do directly.`

const agentTaskSystemPrompt = `You are a read-only research sub-agent inside a coding tool. You have the read, glob, grep, and ls tools — no shell, no editing, no network.

Investigate the task you are given thoroughly: search broadly, read the relevant files, and chase cross-references until you can answer with confidence. Your final message is returned verbatim to the calling agent as data, so make it a complete, self-contained report: state the answer first, then the supporting evidence as file_path:line_number references. Report honestly when something cannot be found.`

// agentTaskMaxSteps is a hard backstop on sub-agent looping; loop
// detection is the primary guard, this just bounds the worst case.
const agentTaskMaxSteps = 50

type agentTaskParams struct {
	Prompt string `json:"prompt" description:"the self-contained research task for the sub-agent, including everything it needs to know"`
}

// agentTaskTool spawns a child fantasy agent on the same model with a
// read-only tool set. The model getter is a closure so the tool list
// can be built before the session's LanguageModel exists.
func agentTaskTool(env *agentToolEnv, model func() fantasy.LanguageModel) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"task",
		agentTaskToolDescription,
		func(ctx context.Context, p agentTaskParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			prompt := strings.TrimSpace(p.Prompt)
			if prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			lm := model()
			if lm == nil {
				return fantasy.NewTextErrorResponse("sub-agent model unavailable"), nil
			}
			sub := fantasy.NewAgent(lm,
				fantasy.WithSystemPrompt(agentTaskSystemPrompt),
				fantasy.WithTools(
					agentReadTool(env),
					agentGlobTool(env),
					agentGrepTool(env),
					agentLsTool(env),
				),
				fantasy.WithStopConditions(
					agentLoopDetectionCondition(),
					fantasy.StepCountIs(agentTaskMaxSteps),
				),
			)
			result, err := sub.Generate(ctx, fantasy.AgentCall{Prompt: prompt})
			if err != nil {
				return fantasy.NewTextErrorResponse("sub-agent failed: " + err.Error()), nil
			}
			report := strings.TrimSpace(result.Response.Content.Text())
			if report == "" {
				return fantasy.NewTextErrorResponse("sub-agent returned no report"), nil
			}
			return fantasy.NewTextResponse(report), nil
		},
	)
}
