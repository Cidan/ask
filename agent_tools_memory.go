package main

import (
	"context"
	"strings"

	"charm.land/fantasy"
)

type agentMemoryIndexParams struct {
	Text        string `json:"text" description:"the text to embed and store in long term memory"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentMemoryIndexTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"memory_index",
		"Store text in the project's long-term vector memory. Use this to record architectural decisions, solved bugs, learned facts, and important project conventions so they automatically surface in future sessions when relevant. Do NOT use this for code snippets or entire files (those are searched via grep/glob); use it for conceptual knowledge.",
		func(ctx context.Context, p agentMemoryIndexParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			text := strings.TrimSpace(p.Text)
			if text == "" {
				return fantasy.NewTextErrorResponse("text cannot be empty"), nil
			}

			if denied := env.requestApproval(ctx, "memory_index", map[string]any{
				"text":        p.Text,
				"description": p.Description,
			}); denied != nil {
				return *denied, nil
			}

			if err := memoryIndex(ctx, env.cwd, text); err != nil {
				return fantasy.NewTextErrorResponse("error: " + err.Error()), nil
			}

			return fantasy.NewTextResponse("successfully indexed memory"), nil
		},
	)
}
