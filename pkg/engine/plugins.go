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

	// 2. Function call modifier plugin for parameter injection when configured.
	// Defaults to inactive so native tools using functiontool.New with ParametersJsonSchema
	// are not corrupted by the plugin's decl.Parameters initialization.
	if modPlugin, err := NewFunctionCallModifierPlugin(FunctionCallModifierOptions{
		Predicate: func(toolName string) bool {
			return false
		},
	}); err == nil && modPlugin != nil {
		plugins = append(plugins, modPlugin)
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
type FunctionCallModifierOptions struct {
	Predicate           func(toolName string) bool
	Args                map[string]*genai.Schema
	OverrideDescription func(originalDescription string) string
}

// NewFunctionCallModifierPlugin creates an ADK functioncallmodifier plugin with safety guards.
func NewFunctionCallModifierPlugin(opts FunctionCallModifierOptions) (*plugin.Plugin, error) {
	pred := opts.Predicate
	if pred == nil {
		pred = func(toolName string) bool {
			return len(opts.Args) > 0 || opts.OverrideDescription != nil
		}
	}
	cfg := functioncallmodifier.FunctionCallModifierConfig{
		Predicate:           pred,
		Args:                opts.Args,
		OverrideDescription: opts.OverrideDescription,
	}
	return functioncallmodifier.NewPlugin(cfg)
}
