package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/genai"
)

const SearchToolsDescription = `Search the tool registry for tools that are not listed in your tool definitions.

Beyond your core tools, ask keeps a registry of additional tools — issue tracking (linear_*) and external MCP integrations (mcp__<server>__<tool>). They are real, callable tools; they are just not included in your tool definitions to keep your context small. (The ask-built-in workflow_* tools and clear_plans are core exceptions — they live on the wire, not in the registry, because the two-stage workflow guard forces the model to call them directly.)

Query syntax: "*" lists every registry tool; a trailing * does prefix matching (e.g. "linear_*"); anything else is a case-insensitive substring match against tool names and descriptions. Each result carries the tool's name, description, and full input_schema — everything needed to call it through invoke_tool.`

const InvokeToolDescription = `Invoke a tool from the tool registry by name.

Registry tools (discovered via search_tools) are called through this tool: pass the registry tool's exact name as tool_name and its arguments as the params object, matching the input_schema search_tools returned. The result is the underlying tool's own result, returned verbatim. Core tools are NOT callable this way — call them directly.`

type SearchToolsParams struct {
	Query       string `json:"query" description:"\"*\" lists every registry tool; a trailing * does prefix matching (e.g. linear_*); anything else is a case-insensitive substring match on tool names and descriptions"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

type SearchToolsEntry struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// SearchToolsTool builds the search_tools core tool.
func SearchToolsTool(registry func() []Tool) Tool {
	return NewTool(
		"search_tools",
		SearchToolsDescription,
		func(_ context.Context, p SearchToolsParams) (ToolResponse, error) {
			tools := registry()
			matches := make([]SearchToolsEntry, 0, len(tools))
			var allNames []string
			for _, t := range tools {
				info := t.Info()
				allNames = append(allNames, info.Name)
				if !searchToolsMatch(p.Query, info) {
					continue
				}
				required := info.Required
				if required == nil {
					required = []string{}
				}
				matches = append(matches, SearchToolsEntry{
					Name:        info.Name,
					Description: info.Description,
					InputSchema: map[string]any{
						"type":       "object",
						"properties": info.Parameters,
						"required":   required,
					},
				})
			}
			sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
			if len(matches) == 0 {
				sort.Strings(allNames)
				if len(allNames) == 0 {
					return NewTextResponse("the tool registry is empty — no additional tools are configured"), nil
				}
				return NewTextResponse(fmt.Sprintf(
					"no registry tools matched %q; available tools: %s",
					p.Query, strings.Join(allNames, ", "))), nil
			}
			body, err := json.Marshal(matches)
			if err != nil {
				return NewTextErrorResponse("search_tools: " + err.Error()), nil
			}
			return NewTextResponse(TruncateMiddle(string(body))), nil
		},
	)
}

func searchToolsMatch(query string, info ToolInfo) bool {
	q := strings.TrimSpace(query)
	if q == "" || q == "*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(q, "*"); ok {
		return strings.HasPrefix(strings.ToLower(info.Name), strings.ToLower(prefix))
	}
	q = strings.ToLower(q)
	return strings.Contains(strings.ToLower(info.Name), q) ||
		strings.Contains(strings.ToLower(info.Description), q)
}

type InvokeToolParams struct {
	ToolName    string         `json:"tool_name"`
	Params      map[string]any `json:"params"`
	Description string         `json:"description"`
}

type invokeToolImpl struct {
	registry func() []Tool
	isCore   func(name string) bool
	env      *ToolEnv
}

// InvokeToolTool builds the invoke_tool core tool.
func InvokeToolTool(registry func() []Tool, isCore func(string) bool, env *ToolEnv) Tool {
	return &invokeToolImpl{registry: registry, isCore: isCore, env: env}
}

func (t *invokeToolImpl) Name() string        { return "invoke_tool" }
func (t *invokeToolImpl) Description() string { return InvokeToolDescription }

func (t *invokeToolImpl) Info() ToolInfo {
	return ToolInfo{
		Name:        "invoke_tool",
		Description: InvokeToolDescription,
		Parameters: map[string]any{
			"tool_name": map[string]any{
				"type":        "string",
				"description": "exact name of the registry tool to invoke, as returned by search_tools",
			},
			"params": map[string]any{
				"type":                 "object",
				"description":          "the tool's input arguments, matching the input_schema search_tools returned for it",
				"additionalProperties": true,
			},
			"description": map[string]any{
				"type":        "string",
				"description": ToolPhraseFieldDoc,
			},
		},
		Required: []string{"tool_name", "description"},
	}
}

func (t *invokeToolImpl) Declaration() *genai.FunctionDeclaration {
	info := t.Info()
	schemaObj := map[string]any{
		"type":       "object",
		"properties": info.Parameters,
		"required":   info.Required,
	}
	return &genai.FunctionDeclaration{
		Name:                 info.Name,
		Description:          info.Description,
		ParametersJsonSchema: schemaObj,
	}
}

func (t *invokeToolImpl) Run(ctx context.Context, args map[string]any) (ToolResponse, error) {
	name, _ := args["tool_name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return NewTextErrorResponse("tool_name is required"), nil
	}
	var inner Tool
	for _, candidate := range t.registry() {
		if candidate.Info().Name == name {
			inner = candidate
			break
		}
	}
	if inner == nil {
		if t.isCore != nil && t.isCore(name) {
			return NewTextErrorResponse(name + " is a core tool — call it directly, not through invoke_tool"), nil
		}
		return NewTextErrorResponse("unknown tool " + name + " — use search_tools to discover what the registry offers"), nil
	}

	params, _ := args["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	info := inner.Info()
	desc, _ := args["description"].(string)
	if _, has := params["description"]; !has && desc != "" && requiresField(info, "description") {
		params["description"] = desc
	}
	for _, required := range info.Required {
		if _, ok := params[required]; !ok {
			return NewTextErrorResponse(fmt.Sprintf(
				"missing required parameter %q for %s — check its input_schema via search_tools", required, name)), nil
		}
	}
	return inner.Run(ctx, params)
}

func requiresField(info ToolInfo, field string) bool {
	for _, r := range info.Required {
		if r == field {
			return true
		}
	}
	return false
}

// UnwrapInvokeToolCall maps an invoke_tool call to the inner tool for transcript display.
func UnwrapInvokeToolCall(input map[string]any) (string, map[string]any) {
	name, _ := input["tool_name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return "invoke_tool", input
	}
	params, _ := input["params"].(map[string]any)
	display := make(map[string]any, len(params)+1)
	for k, v := range params {
		display[k] = v
	}
	if _, ok := display["description"]; !ok {
		if phrase, _ := input["description"].(string); phrase != "" {
			display["description"] = phrase
		}
	}
	return name, display
}
