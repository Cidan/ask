package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// agentMCPServer describes one MCP server the agent session should
// attach: ask's own loopback bridge (linear/workflow tools) and the
// project-level GitHub MCP both flow through here.
type agentMCPServer struct {
	name    string
	url     string
	headers map[string]string
	// skip filters out tools the harness already provides natively
	// (ask_user_question, end_turn) so the model never sees duplicates.
	skip map[string]bool
}

// agentMCPConnectTimeout bounds the connect+list handshake per server;
// a slow remote must not stall session start indefinitely.
const agentMCPConnectTimeout = 15 * time.Second

// headerRoundTripper injects static headers (Authorization for the
// GitHub MCP) into every request.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// connectAgentMCP attaches one MCP server and wraps its tools as
// fantasy tools named mcp__<server>__<tool> (the same namespace claude
// sees, so prompts and habits transfer). The returned closer tears the
// session down; both are nil when the connect or list fails — callers
// log and continue without the server rather than failing the session.
func connectAgentMCP(ctx context.Context, srv agentMCPServer) ([]fantasy.AgentTool, func(), error) {
	connectCtx, cancel := context.WithTimeout(ctx, agentMCPConnectTimeout)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "ask-agent", Version: "1.0.0"}, nil)
	httpClient := &http.Client{}
	if len(srv.headers) > 0 {
		httpClient.Transport = headerRoundTripper{headers: srv.headers}
	}
	session, err := client.Connect(connectCtx, &mcp.StreamableClientTransport{
		Endpoint:             srv.url,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", srv.name, err)
	}
	listed, err := session.ListTools(connectCtx, nil)
	if err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("list tools on %s: %w", srv.name, err)
	}
	var tools []fantasy.AgentTool
	for _, t := range listed.Tools {
		if srv.skip[t.Name] {
			continue
		}
		tools = append(tools, newMCPAgentTool(srv.name, session, t))
	}
	closer := func() { _ = session.Close() }
	return tools, closer, nil
}

// mcpAgentTool adapts one server-side MCP tool to fantasy's AgentTool.
// NewAgentTool's reflection schema can't express arbitrary remote
// schemas, so this implements the interface directly and passes the
// server's input schema through verbatim.
type mcpAgentTool struct {
	name        string
	description string
	properties  map[string]any
	required    []string
	session     *mcp.ClientSession
	remoteName  string
	opts        fantasy.ProviderOptions
}

func newMCPAgentTool(serverName string, session *mcp.ClientSession, t *mcp.Tool) *mcpAgentTool {
	properties := map[string]any{}
	var required []string
	if schema, ok := t.InputSchema.(map[string]any); ok {
		if props, ok := schema["properties"].(map[string]any); ok {
			properties = props
		}
		if reqs, ok := schema["required"].([]any); ok {
			for _, r := range reqs {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}
	}
	return &mcpAgentTool{
		name:        fmt.Sprintf("mcp__%s__%s", serverName, t.Name),
		description: t.Description,
		properties:  properties,
		required:    required,
		session:     session,
		remoteName:  t.Name,
	}
}

func (m *mcpAgentTool) Info() fantasy.ToolInfo {
	required := m.required
	if required == nil {
		required = []string{}
	}
	return fantasy.ToolInfo{
		Name:        m.name,
		Description: m.description,
		Parameters:  m.properties,
		Required:    required,
	}
}

func (m *mcpAgentTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args map[string]any
	if strings.TrimSpace(params.Input) != "" {
		if err := json.Unmarshal([]byte(params.Input), &args); err != nil {
			return fantasy.NewTextErrorResponse("invalid parameters: " + err.Error()), nil
		}
	}
	res, err := m.session.CallTool(ctx, &mcp.CallToolParams{Name: m.remoteName, Arguments: args})
	if err != nil {
		return fantasy.NewTextErrorResponse(m.name + ": " + err.Error()), nil
	}
	var out strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	body := out.String()
	if strings.TrimSpace(body) == "" {
		body = "(empty result)"
	}
	if res.IsError {
		return fantasy.NewTextErrorResponse(body), nil
	}
	return fantasy.NewTextResponse(truncateMiddle(body)), nil
}

func (m *mcpAgentTool) ProviderOptions() fantasy.ProviderOptions        { return m.opts }
func (m *mcpAgentTool) SetProviderOptions(opts fantasy.ProviderOptions) { m.opts = opts }
