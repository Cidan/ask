package engine

import (

	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/plugin/functioncallmodifier"
	"google.golang.org/adk/v2/plugin/retryandreflect"
	"google.golang.org/genai"
)

// DefaultPlugins returns the standard set of ADK plugins configured for ask.
func DefaultPlugins() []*plugin.Plugin {
	var plugins []*plugin.Plugin

	// 1. Retry and reflect plugin for automated in-turn tool error self-healing.
	if retryPlugin, err := NewRetryAndReflectPlugin(2); err == nil && retryPlugin != nil {
		plugins = append(plugins, retryPlugin)
	}


	return plugins
}

func isCoreCodingTool(toolName string) bool {
	switch toolName {
	case "read", "write", "edit", "glob", "grep", "ls", "bash", "job_output", "job_kill",
		"fetch", "todos", "ask_user_question", "end_turn", "web_search":
		return true
	default:
		return false
	}
}

// NewRetryAndReflectPlugin creates an ADK retryandreflect plugin with the specified max retries.
func NewRetryAndReflectPlugin(maxRetries int) (*plugin.Plugin, error) {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	return retryandreflect.New(
		retryandreflect.WithMaxRetries(maxRetries),
		retryandreflect.WithTrackingScope(retryandreflect.Invocation),
	)
}

// FunctionCallModifierOptions defines configuration for the functioncallmodifier plugin.

// NewFunctionCallModifierPlugin creates an ADK functioncallmodifier plugin with safety guards.
