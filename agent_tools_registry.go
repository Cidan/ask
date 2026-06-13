package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
)

// agent_tools_registry.go is the deferred tool registry surface:
// search_tools + invoke_tool. The wire tool definitions sent to the
// model every turn are the core tools ONLY — everything else (the
// native bridge twins, every MCP server tool) lives in the deferred
// registry, discoverable through search_tools and callable through
// invoke_tool. This keeps the per-turn context cost flat no matter
// how many MCP servers the user wires in, and keeps the wire toolset
// byte-stable for anthropic's cached tool block.
//
// ⚠ New tools go into the registry, NEVER into the core list in
// setupAgentSessionTools, unless there is a deliberate, documented
// exception (see "Tool registry vs core tools" in CLAUDE.md).

const agentSearchToolsDescription = `Search the tool registry for tools that are not listed in your tool definitions.

Beyond your core tools, ask keeps a registry of additional tools — issue tracking (linear_*), workflow management (workflow_*), and external MCP integrations (mcp__<server>__<tool>). They are real, callable tools; they are just not included in your tool definitions to keep your context small.

Query syntax: "*" lists every registry tool; a trailing * does prefix matching (e.g. "linear_*"); anything else is a case-insensitive substring match against tool names and descriptions. Each result carries the tool's name, description, and full input_schema — everything needed to call it through invoke_tool.`

const agentInvokeToolDescription = `Invoke a tool from the tool registry by name.

Registry tools (discovered via search_tools) are called through this tool: pass the registry tool's exact name as tool_name and its arguments as the params object, matching the input_schema search_tools returned. The result is the underlying tool's own result, returned verbatim. Core tools are NOT callable this way — call them directly.`

type agentSearchToolsParams struct {
	Query       string `json:"query" description:"\"*\" lists every registry tool; a trailing * does prefix matching (e.g. linear_*); anything else is a case-insensitive substring match on tool names and descriptions"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// searchToolsEntry is one search result: everything the model needs
// to construct a valid invoke_tool call.
type searchToolsEntry struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// agentSearchToolsTool builds the search_tools core tool. registry
// returns the current deferred tool set (it is re-read on every call
// so MCP tools/list_changed refreshes are picked up live).
func agentSearchToolsTool(registry func() []fantasy.AgentTool) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"search_tools",
		agentSearchToolsDescription,
		func(_ context.Context, p agentSearchToolsParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			tools := registry()
			matches := make([]searchToolsEntry, 0, len(tools))
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
				matches = append(matches, searchToolsEntry{
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
					return fantasy.NewTextResponse("the tool registry is empty — no additional tools are configured"), nil
				}
				return fantasy.NewTextResponse(fmt.Sprintf(
					"no registry tools matched %q; available tools: %s",
					p.Query, strings.Join(allNames, ", "))), nil
			}
			body, err := json.Marshal(matches)
			if err != nil {
				return fantasy.NewTextErrorResponse("search_tools: " + err.Error()), nil
			}
			return fantasy.NewTextResponse(truncateMiddle(string(body))), nil
		},
	)
}

// searchToolsMatch implements the three query forms: "*"/empty (all),
// trailing-* prefix, and case-insensitive substring on name or
// description.
func searchToolsMatch(query string, info fantasy.ToolInfo) bool {
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

type agentInvokeToolParams struct {
	ToolName    string         `json:"tool_name"`
	Params      map[string]any `json:"params"`
	Description string         `json:"description"`
}

// agentInvokeTool implements fantasy.AgentTool by hand: the params
// field is a free-form object whose real schema is the inner tool's
// (returned by search_tools), and fantasy's reflection-based schema
// generator cannot express "any object" cleanly for map[string]any.
type agentInvokeTool struct {
	registry func() []fantasy.AgentTool
	isCore   func(name string) bool
	env      *agentToolEnv
	opts     fantasy.ProviderOptions
}

// agentInvokeToolTool builds the invoke_tool core tool. registry
// returns the current deferred tool set; isCore reports whether a
// name belongs to the core tools so the error message can steer the
// model back to a direct call. env (optional) lets the invoke path
// disarm the todos workflow guard when workflow_list is called.
func agentInvokeToolTool(registry func() []fantasy.AgentTool, isCore func(string) bool, env *agentToolEnv) fantasy.AgentTool {
	return &agentInvokeTool{registry: registry, isCore: isCore, env: env}
}

func (t *agentInvokeTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "invoke_tool",
		Description: agentInvokeToolDescription,
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
				"description": toolPhraseFieldDoc,
			},
		},
		Required: []string{"tool_name", "description"},
	}
}

func (t *agentInvokeTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var p agentInvokeToolParams
	if err := json.Unmarshal([]byte(call.Input), &p); err != nil {
		return fantasy.NewTextErrorResponse("invalid parameters: " + err.Error()), nil
	}
	name := strings.TrimSpace(p.ToolName)
	if name == "" {
		return fantasy.NewTextErrorResponse("tool_name is required"), nil
	}
	var inner fantasy.AgentTool
	for _, candidate := range t.registry() {
		if candidate.Info().Name == name {
			inner = candidate
			break
		}
	}
	if inner == nil {
		if t.isCore != nil && t.isCore(name) {
			return fantasy.NewTextErrorResponse(name + " is a core tool — call it directly, not through invoke_tool"), nil
		}
		return fantasy.NewTextErrorResponse("unknown tool " + name + " — use search_tools to discover what the registry offers"), nil
	}

	params := p.Params
	if params == nil {
		params = map[string]any{}
	}
	info := inner.Info()
	// Native registry tools require the user-facing phrase param; let
	// the invoke-level phrase satisfy it so the model doesn't have to
	// author it twice. Tools that don't declare description as
	// required (MCP servers) never get the extra field — an injected
	// unknown key could fail server-side schema validation.
	if _, has := params["description"]; !has && p.Description != "" && requiresField(info, "description") {
		params["description"] = p.Description
	}
	// Fantasy validates required fields against the wire tool
	// definitions only — a deferred tool never reaches the wire, so
	// the check must be replicated here. Without it a missing param
	// silently zero-values through the inner tool's unmarshal.
	for _, required := range info.Required {
		if _, ok := params[required]; !ok {
			return fantasy.NewTextErrorResponse(fmt.Sprintf(
				"missing required parameter %q for %s — check its input_schema via search_tools", required, name)), nil
		}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return fantasy.NewTextErrorResponse("invalid params: " + err.Error()), nil
	}
	// Looking at the project's workflows disarms the first-stage todos
	// workflow guard; actually running one disarms the second-stage
	// decision guard. The model has done what each guard asks, so its
	// task list goes through unimpeded from here on.
	if t.env != nil {
		switch name {
		case "workflow_list":
			t.env.markWorkflowsChecked()
		case "workflow_run":
			t.env.markWorkflowRunDispatched()
		}
	}
	return inner.Run(ctx, fantasy.ToolCall{
		ID:    call.ID,
		Name:  name,
		Input: string(raw),
	})
}

func (t *agentInvokeTool) ProviderOptions() fantasy.ProviderOptions { return t.opts }
func (t *agentInvokeTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.opts = opts
}

func requiresField(info fantasy.ToolInfo, field string) bool {
	for _, r := range info.Required {
		if r == field {
			return true
		}
	}
	return false
}

// unwrapInvokeToolCall maps an invoke_tool input onto the inner
// registry call for display: the transcript and status line show the
// real tool (name + params), never the invoke_tool plumbing. The
// invoke-level phrase backfills params["description"] so registry
// calls render with a headline like every native call. Inputs that
// don't carry a usable tool_name fall back to the raw invoke_tool
// rendering rather than guessing.
func unwrapInvokeToolCall(input map[string]any) (string, map[string]any) {
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
