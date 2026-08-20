package tools

import (
	"context"
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

// NewTool creates a native ADK Tool backed by functiontool.New.
func NewTool[T any](name, description string, handler func(ctx context.Context, params T) (ToolResponse, error)) Tool {
	adkTool, err := functiontool.New[T, any](
		functiontool.Config{
			Name:        name,
			Description: description,
		},
		func(actx agent.Context, params T) (any, error) {
			resp, err := handler(actx, params)
			if err != nil {
				return nil, err
			}
			res := map[string]any{
				"result": resp.Content,
			}
			if resp.IsError {
				res["is_error"] = true
			}
			if resp.StopTurn {
				res["stop_turn"] = true
			}
			return res, nil
		},
	)
	if err != nil {
		panic("failed to create tool " + name + ": " + err.Error())
	}
	return adkTool
}

// RunToolWithJSON executes a Tool by parsing a JSON arguments string.
func RunToolWithJSON(ctx context.Context, t Tool, inputJSON string) (ToolResponse, error) {
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
	stopTurn, _ := res["stop_turn"].(bool)
	if content == "" && len(res) > 0 {
		raw, _ := json.Marshal(res)
		content = string(raw)
	}
	return ToolResponse{
		Content:  content,
		IsError:  isErr,
		StopTurn: stopTurn,
	}, nil
}
