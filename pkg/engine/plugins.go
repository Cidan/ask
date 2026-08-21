package engine

import (
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/plugin/retryandreflect"
)

// DefaultPlugins returns the standard set of ADK plugins configured for ask.
//
// functioncallmodifier is deliberately absent. It exists to inject
// synthetic arguments into tool declarations at request time; ask needed
// that for the required `description` phrase, which is now a real field
// on every native tool's params struct, so there is nothing left to
// inject. It was registered here with a predicate that always returned
// false ever since PR #132 disabled it to stop it clobbering
// ParametersJsonSchema — a plugin that could never fire.
//
// Bridge tools (linear_*, workflow_*) still have `description` added to
// their input schema in pkg/tools/bridge.go, because their input types
// come from the MCP handler cores and do not carry the field. That is a
// one-time schema build at construction, not per-request AST surgery.
func DefaultPlugins() []*plugin.Plugin {
	var plugins []*plugin.Plugin

	// Retry and reflect: when a tool returns a Go error, hand the model
	// corrective guidance and let it retry in the same turn instead of
	// surrendering. Every ask tool reports failure as a real error, which
	// is what OnToolErrorCallback keys on.
	if retryPlugin, err := NewRetryAndReflectPlugin(2); err == nil && retryPlugin != nil {
		plugins = append(plugins, retryPlugin)
	}

	return plugins
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
