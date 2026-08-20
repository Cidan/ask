package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/functiontool"
)

// NativeBridgeTool adapts an MCP handler core to an ADK Tool.
func NativeBridgeTool[In, Out any](name, description string,
	run func(ctx context.Context, in In) (*mcp.CallToolResult, Out, error),
) Tool {
	var inputSchema *jsonschema.Schema
	if s, err := jsonschema.For[In](nil); err == nil {
		inputSchema = s
		if inputSchema.Properties == nil {
			inputSchema.Properties = map[string]*jsonschema.Schema{}
		}
		if _, exists := inputSchema.Properties["description"]; !exists {
			inputSchema.Properties["description"] = &jsonschema.Schema{
				Type:        "string",
				Description: ToolPhraseFieldDoc,
			}
			inputSchema.Required = append(inputSchema.Required, "description")
		}
	}

	adkTool, err := functiontool.New[In, any](
		functiontool.Config{
			Name:        name,
			Description: description,
			InputSchema: inputSchema,
		},
		func(actx agent.Context, in In) (any, error) {
			res, out, err := run(actx, in)
			if err != nil {
				return map[string]any{
					"result":   name + ": " + err.Error(),
					"is_error": true,
				}, nil
			}
			body := MCPResultText(res)
			if res != nil && res.IsError {
				if strings.TrimSpace(body) == "" {
					body = "(empty error result)"
				}
				return map[string]any{
					"result":   body,
					"is_error": true,
				}, nil
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
			return map[string]any{
				"result": TruncateMiddle(body),
			}, nil
		},
	)
	if err != nil {
		panic("failed to create bridge tool " + name + ": " + err.Error())
	}
	return adkTool
}

// FlattenNullableTypes drops "null" from type arrays in json schema maps for backward compatibility.
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
