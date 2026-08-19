package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// Aliases to engine types
type ToolResponse = engine.ToolResponse
type ToolInfo = engine.ToolInfo
type Tool = engine.Tool

var (
	NewTextResponse      = engine.NewTextResponse
	NewTextErrorResponse = engine.NewTextErrorResponse
)

// TypedTool implements Tool using Go parameter types and wraps an ADK functiontool.
type TypedTool[T any] struct {
	name        string
	description string
	properties  map[string]any
	required    []string
	handler     func(ctx context.Context, params T) (ToolResponse, error)
	adkTool     tool.Tool
}

func NewTool[T any](name, description string, handler func(ctx context.Context, params T) (ToolResponse, error)) Tool {
	properties := map[string]any{}
	var required []string
	if schema, err := jsonschema.For[T](nil); err == nil {
		if raw, err := json.Marshal(schema); err == nil {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				if props, ok := m["properties"].(map[string]any); ok {
					properties = props
				}
				if reqs, ok := m["required"].([]any); ok {
					for _, r := range reqs {
						if s, ok := r.(string); ok {
							required = append(required, s)
						}
					}
				}
			}
		}
	}
	FlattenNullableTypes(properties)

	adkTool, _ := functiontool.New[T, any](
		functiontool.Config{
			Name:        name,
			Description: description,
		},
		func(actx agent.Context, params T) (any, error) {
			resp, err := handler(actx, params)
			if err != nil {
				return nil, err
			}
			if resp.IsError {
				return map[string]any{
					"result":   resp.Content,
					"is_error": true,
				}, nil
			}
			return map[string]any{
				"result": resp.Content,
			}, nil
		},
	)

	return &TypedTool[T]{
		name:        name,
		description: description,
		properties:  properties,
		required:    required,
		handler:     handler,
		adkTool:     adkTool,
	}
}

func (t *TypedTool[T]) Name() string        { return t.name }
func (t *TypedTool[T]) Description() string { return t.description }
func (t *TypedTool[T]) IsLongRunning() bool { return false }
func (t *TypedTool[T]) ADKTool() tool.Tool {
	if t.adkTool != nil {
		return t.adkTool
	}
	return t
}

func (t *TypedTool[T]) Info() ToolInfo {
	return ToolInfo{
		Name:        t.name,
		Description: t.description,
		Parameters:  t.properties,
		Required:    t.required,
	}
}

func (t *TypedTool[T]) Declaration() *genai.FunctionDeclaration {
	schemaObj := map[string]any{
		"type":       "object",
		"properties": t.properties,
	}
	if len(t.required) > 0 {
		schemaObj["required"] = t.required
	}
	return &genai.FunctionDeclaration{
		Name:                 t.name,
		Description:          t.description,
		ParametersJsonSchema: schemaObj,
	}
}

func (t *TypedTool[T]) Run(ctx context.Context, args map[string]any) (ToolResponse, error) {
	var params T
	if len(args) > 0 {
		raw, err := json.Marshal(args)
		if err != nil {
			return NewTextErrorResponse("failed to marshal tool arguments: " + err.Error()), nil
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return NewTextErrorResponse("invalid parameters: " + err.Error()), nil
		}
	}
	return t.handler(ctx, params)
}

func RunToolWithJSON(ctx context.Context, t Tool, inputJSON string) (ToolResponse, error) {
	args := make(map[string]any)
	if strings.TrimSpace(inputJSON) != "" {
		if err := json.Unmarshal([]byte(inputJSON), &args); err != nil {
			return NewTextErrorResponse("invalid parameters: " + err.Error()), nil
		}
	}
	return t.Run(ctx, args)
}
