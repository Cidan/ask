package tools

import (
	"encoding/json"
	"strings"

	"github.com/Cidan/ask/pkg/engine"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/functiontool"
)

// Aliases to engine types
type ToolResponse = engine.ToolResponse
type ToolInfo = engine.ToolInfo
type Tool = engine.Tool

var (
	NewTextResponse      = engine.NewTextResponse
	NewTextErrorResponse = engine.NewTextErrorResponse
	ExtractToolInfo      = engine.ExtractToolInfo
	RunADKTool           = engine.RunADKTool
)

// NewTypedTool creates a native ADK tool from typed request and response
// structs. ADK infers BOTH schemas from T and R — functiontool.New calls
// resolvedSchema[TArgs] and resolvedSchema[TResults] — so the tool
// declaration carries a real output schema instead of an empty one.
//
// Handlers report failure by returning a Go error, not by setting a flag
// on the result. That is also what ADK's OnToolErrorCallback keys on.
func NewTypedTool[T, R any](name, description string, handler func(ctx agent.Context, params T) (R, error)) Tool {
	adkTool, err := functiontool.New[T, R](
		functiontool.Config{
			Name:        name,
			Description: description,
		},
		handler,
	)
	if err != nil {
		panic("failed to create tool " + name + ": " + err.Error())
	}
	return adkTool
}

// RunToolWithJSON executes a Tool by parsing a JSON arguments string.
func RunToolWithJSON(ctx agent.Context, t Tool, inputJSON string) (ToolResponse, error) {
	args := make(map[string]any)
	if strings.TrimSpace(inputJSON) != "" {
		if err := json.Unmarshal([]byte(inputJSON), &args); err != nil {
			return NewTextErrorResponse("invalid parameters: " + err.Error()), nil
		}
	}
	info := ExtractToolInfo(t)
	if info.Parameters != nil {
		if _, has := info.Parameters["description"]; has {
			if _, ok := args["description"]; !ok {
				args["description"] = "test execution"
			}
		}
	}
	res, err := RunADKTool(ctx, t, args)
	if err != nil {
		return NewTextErrorResponse(err.Error()), nil
	}
	if res == nil {
		return NewTextResponse(""), nil
	}
	content, _ := res["result"].(string)
	isErr, _ := res["is_error"].(bool)
	if content == "" && len(res) > 0 {
		raw, _ := json.Marshal(res)
		content = string(raw)
	}
	return ToolResponse{
		Content: content,
		IsError: isErr,
	}, nil
}
