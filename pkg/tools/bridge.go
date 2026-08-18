package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"
)

// NativeBridgeTool adapts an MCP handler core to our Tool interface.
func NativeBridgeTool[In, Out any](name, description string,
	run func(ctx context.Context, in In) (*mcp.CallToolResult, Out, error),
) Tool {
	properties := map[string]any{}
	var required []string
	if schema, err := jsonschema.For[In](nil); err == nil {
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
	if _, exists := properties["description"]; !exists {
		properties["description"] = map[string]any{
			"type":        "string",
			"description": ToolPhraseFieldDoc,
		}
		required = append(required, "description")
	}
	return &BridgeTool[In, Out]{
		NameVal:        name,
		DescriptionVal: description,
		PropertiesVal:  properties,
		RequiredVal:    required,
		RunVal:         run,
	}
}

// FlattenNullableTypes drops "null" from type arrays in json schema maps.
func FlattenNullableTypes(v any) {
	switch n := v.(type) {
	case map[string]any:
		if t, ok := n["type"].([]any); ok {
			nonNull := make([]any, 0, len(t))
			for _, x := range t {
				if s, ok := x.(string); !ok || s != "null" {
					nonNull = append(nonNull, x)
				}
			}
			switch len(nonNull) {
			case 0:
				delete(n, "type")
			case 1:
				n["type"] = nonNull[0]
			default:
				n["type"] = nonNull
			}
		}
		for _, child := range n {
			FlattenNullableTypes(child)
		}
	case []any:
		for _, item := range n {
			FlattenNullableTypes(item)
		}
	}
}

type BridgeTool[In, Out any] struct {
	NameVal        string
	DescriptionVal string
	PropertiesVal  map[string]any
	RequiredVal    []string
	RunVal         func(ctx context.Context, in In) (*mcp.CallToolResult, Out, error)
}

func (t *BridgeTool[In, Out]) Name() string        { return t.NameVal }
func (t *BridgeTool[In, Out]) Description() string { return t.DescriptionVal }
func (t *BridgeTool[In, Out]) Info() ToolInfo {
	required := t.RequiredVal
	if required == nil {
		required = []string{}
	}
	return ToolInfo{
		Name:        t.NameVal,
		Description: t.DescriptionVal,
		Parameters:  t.PropertiesVal,
		Required:    required,
	}
}

func (t *BridgeTool[In, Out]) Declaration() *genai.FunctionDeclaration {
	schemaObj := map[string]any{
		"type":       "object",
		"properties": t.PropertiesVal,
	}
	if len(t.RequiredVal) > 0 {
		schemaObj["required"] = t.RequiredVal
	}
	return &genai.FunctionDeclaration{
		Name:                 t.NameVal,
		Description:          t.DescriptionVal,
		ParametersJsonSchema: schemaObj,
	}
}

func (t *BridgeTool[In, Out]) Run(ctx context.Context, args map[string]any) (ToolResponse, error) {
	var in In
	if len(args) > 0 {
		raw, err := json.Marshal(args)
		if err != nil {
			return NewTextErrorResponse("invalid parameters: " + err.Error()), nil
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return NewTextErrorResponse("invalid parameters: " + err.Error()), nil
		}
	}
	res, out, err := t.RunVal(ctx, in)
	if err != nil {
		return NewTextErrorResponse(t.NameVal + ": " + err.Error()), nil
	}
	body := MCPResultText(res)
	if res != nil && res.IsError {
		if strings.TrimSpace(body) == "" {
			body = "(empty error result)"
		}
		return NewTextErrorResponse(body), nil
	}
	if j, err := json.Marshal(out); err == nil {
		js := string(j)
		switch {
		case js == "{}" || js == "null":
		case strings.TrimSpace(body) == "" || body == js:
			body = js
		default:
			body = body + "\n" + js
		}
	}
	if strings.TrimSpace(body) == "" {
		body = "(empty result)"
	}
	return NewTextResponse(TruncateMiddle(body)), nil
}

func MCPResultText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
