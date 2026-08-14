package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServer describes one MCP server to attach.
type MCPServer struct {
	Name string
	Cfg  MCPServerConfig
	Skip map[string]bool
}

const (
	MCPConnectTimeout = 15 * time.Second
	MCPPingTimeout    = 5 * time.Second
)

func (s MCPServer) ConnectTimeout() time.Duration {
	if s.Cfg.TimeoutSeconds > 0 {
		return time.Duration(s.Cfg.TimeoutSeconds) * time.Second
	}
	return MCPConnectTimeout
}

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

// MCPTransportFor builds the wire transport for one server.
var MCPTransportFor = func(srv MCPServer, oauth *MCPOAuthHandler) (mcp.Transport, error) {
	switch srv.Cfg.EffectiveType() {
	case MCPServerTypeStdio:
		cmd := exec.Command(srv.Cfg.Command, srv.Cfg.Args...)
		cmd.Env = os.Environ()
		for k, v := range srv.Cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	case MCPServerTypeSSE:
		httpClient := &http.Client{}
		if len(srv.Cfg.Headers) > 0 {
			httpClient.Transport = headerRoundTripper{headers: srv.Cfg.Headers}
		}
		return &mcp.SSEClientTransport{Endpoint: srv.Cfg.URL, HTTPClient: httpClient}, nil
	default:
		httpClient := &http.Client{}
		if len(srv.Cfg.Headers) > 0 {
			httpClient.Transport = headerRoundTripper{headers: srv.Cfg.Headers}
		}
		t := &mcp.StreamableClientTransport{
			Endpoint:   srv.Cfg.URL,
			HTTPClient: httpClient,
		}
		if oauth != nil {
			t.OAuthHandler = oauth
		}
		return t, nil
	}
}

// MCPManager owns all MCP server attachments for a session.
type MCPManager struct {
	tabID          int
	imagesOK       func() bool
	onToolsChanged func()
	interaction    engine.InteractionHandler

	mu    sync.Mutex
	conns []*mcpServerConn
}

// NewMCPManager creates a new MCPManager.
func NewMCPManager(tabID int, imagesOK func() bool, onToolsChanged func(), interaction engine.InteractionHandler) *MCPManager {
	if imagesOK == nil {
		imagesOK = func() bool { return false }
	}
	if interaction == nil {
		interaction = engine.HeadlessInteractionHandler{}
	}
	return &MCPManager{
		tabID:          tabID,
		imagesOK:       imagesOK,
		onToolsChanged: onToolsChanged,
		interaction:    interaction,
	}
}

// AttachAll connects all servers concurrently.
func (m *MCPManager) AttachAll(ctx context.Context, servers []MCPServer) {
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv MCPServer) {
			defer wg.Done()
			_ = m.Attach(ctx, srv)
		}(srv)
	}
	wg.Wait()
}

// Attach connects a single server.
func (m *MCPManager) Attach(ctx context.Context, srv MCPServer) error {
	conn := &mcpServerConn{mgr: m, srv: srv}
	if srv.Cfg.OAuth && srv.Cfg.EffectiveType() != MCPServerTypeStdio {
		oauth, err := NewMCPOAuthHandler(srv.Cfg.URL)
		if err != nil {
			return fmt.Errorf("oauth setup %s: %w", srv.Name, err)
		}
		conn.oauth = oauth
	}
	connectCtx, cancel := context.WithTimeout(ctx, srv.ConnectTimeout())
	defer cancel()
	if err := conn.connect(connectCtx); err != nil {
		conn.close()
		return err
	}
	if err := conn.refreshTools(connectCtx); err != nil {
		conn.close()
		return fmt.Errorf("list tools on %s: %w", srv.Name, err)
	}
	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()
	return nil
}

// Tools returns the snapshot of all current tools across all connected servers.
func (m *MCPManager) Tools() []fantasy.AgentTool {
	m.mu.Lock()
	conns := append([]*mcpServerConn(nil), m.conns...)
	m.mu.Unlock()
	var out []fantasy.AgentTool
	for _, c := range conns {
		out = append(out, c.currentTools()...)
	}
	return out
}

// Close closes all server connections.
func (m *MCPManager) Close() {
	m.mu.Lock()
	conns := m.conns
	m.conns = nil
	m.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func (m *MCPManager) toolsChanged() {
	if m.onToolsChanged != nil {
		m.onToolsChanged()
	}
}

type mcpServerConn struct {
	mgr   *MCPManager
	srv   MCPServer
	oauth *MCPOAuthHandler

	mu      sync.Mutex
	session *mcp.ClientSession
	tools   []fantasy.AgentTool
}

func (c *mcpServerConn) connect(ctx context.Context) error {
	transport, err := MCPTransportFor(c.srv, c.oauth)
	if err != nil {
		return fmt.Errorf("transport %s: %w", c.srv.Name, err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ask-agent", Version: "1.0.0"}, &mcp.ClientOptions{
		ElicitationHandler: c.handleElicitation,
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			go c.onToolListChanged()
		},
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.srv.Name, err)
	}
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	return nil
}

func (c *mcpServerConn) ensure(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session != nil {
		pingCtx, cancel := context.WithTimeout(ctx, MCPPingTimeout)
		err := session.Ping(pingCtx, nil)
		cancel()
		if err == nil {
			return session, nil
		}
		_ = session.Close()
		c.mu.Lock()
		if c.session == session {
			c.session = nil
		}
		c.mu.Unlock()
	}
	connectCtx, cancel := context.WithTimeout(ctx, c.srv.ConnectTimeout())
	defer cancel()
	if err := c.connect(connectCtx); err != nil {
		return nil, err
	}
	_ = c.refreshTools(connectCtx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session, nil
}

func (c *mcpServerConn) refreshTools(ctx context.Context) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return fmt.Errorf("%s: no session", c.srv.Name)
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	tools := make([]fantasy.AgentTool, 0, len(listed.Tools))
	for _, t := range listed.Tools {
		if c.srv.Skip[t.Name] || !MCPToolAllowed(c.srv.Cfg, t.Name) {
			continue
		}
		tools = append(tools, newMCPAgentTool(c, t))
	}
	c.mu.Lock()
	c.tools = tools
	c.mu.Unlock()
	return nil
}

func (c *mcpServerConn) currentTools() []fantasy.AgentTool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fantasy.AgentTool(nil), c.tools...)
}

func (c *mcpServerConn) onToolListChanged() {
	ctx, cancel := context.WithTimeout(context.Background(), MCPConnectTimeout)
	defer cancel()
	if err := c.refreshTools(ctx); err != nil {
		return
	}
	c.mgr.toolsChanged()
}

func (c *mcpServerConn) close() {
	c.mu.Lock()
	session := c.session
	c.session = nil
	c.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
	if c.oauth != nil {
		c.oauth.Close()
	}
}

func (c *mcpServerConn) handleElicitation(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	params := req.Params
	if params == nil {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	if params.URL != "" {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	props, required := elicitationSchemaProperties(params.RequestedSchema)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	engineQs := make([]engine.Question, 0, len(names)+1)
	if len(names) == 0 {
		engineQs = append(engineQs, engine.Question{
			Kind:   "pick_one",
			Prompt: params.Message,
			Options: []engine.QuestionOption{
				{Label: "Accept"},
				{Label: "Decline"},
			},
		})
	}
	for _, name := range names {
		engineQs = append(engineQs, elicitationEngineQuestion(c.srv.Name, params.Message, name, props[name], required[name]))
	}

	if c.mgr.interaction == nil {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}

	resp, err := c.mgr.interaction.AskQuestion(ctx, c.mgr.tabID, engineQs)
	if err != nil || resp.Headless {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	if resp.Cancelled {
		return &mcp.ElicitResult{Action: "cancel"}, nil
	}

	if len(names) == 0 {
		if len(resp.Answers) > 0 && len(resp.Answers[0].Picks) > 0 && resp.Answers[0].Picks[0] == "Accept" {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
		}
		return &mcp.ElicitResult{Action: "decline"}, nil
	}

	content := map[string]any{}
	for i, name := range names {
		if i < len(resp.Answers) {
			if v, ok := elicitationAnswerValue(props[name], resp.Answers[i]); ok {
				content[name] = v
			}
		}
	}
	return &mcp.ElicitResult{Action: "accept", Content: content}, nil
}

func elicitationSchemaProperties(schema any) (map[string]map[string]any, map[string]bool) {
	props := map[string]map[string]any{}
	required := map[string]bool{}
	root, ok := schema.(map[string]any)
	if !ok {
		return props, required
	}
	if raw, ok := root["properties"].(map[string]any); ok {
		for name, v := range raw {
			if pm, ok := v.(map[string]any); ok {
				props[name] = pm
			} else {
				props[name] = map[string]any{}
			}
		}
	}
	if reqs, ok := root["required"].([]any); ok {
		for _, r := range reqs {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	return props, required
}

func elicitationEngineQuestion(server, message, name string, prop map[string]any, req bool) engine.Question {
	prompt := fmt.Sprintf("[%s] %s — %s", server, message, name)
	if desc, ok := prop["description"].(string); ok && desc != "" {
		prompt += " (" + desc + ")"
	}
	if req {
		prompt += " (required)"
	}
	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		opts := make([]engine.QuestionOption, 0, len(enum))
		for _, e := range enum {
			opts = append(opts, engine.QuestionOption{Label: fmt.Sprintf("%v", e)})
		}
		return engine.Question{Kind: "pick_one", Prompt: prompt, Options: opts}
	}
	if t, _ := prop["type"].(string); t == "boolean" {
		return engine.Question{
			Kind:   "pick_one",
			Prompt: prompt,
			Options: []engine.QuestionOption{
				{Label: "yes"},
				{Label: "no"},
			},
		}
	}
	return engine.Question{Kind: "pick_one", Prompt: prompt, AllowCustom: true}
}

func elicitationAnswerValue(prop map[string]any, ans engine.QuestionAnswer) (any, bool) {
	raw := ans.Custom
	if raw == "" && len(ans.Picks) > 0 {
		raw = ans.Picks[0]
	}
	if raw == "" {
		return nil, false
	}
	switch t, _ := prop["type"].(string); t {
	case "boolean":
		return raw == "yes" || raw == "true", true
	case "number":
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f, true
		}
		return nil, false
	case "integer":
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n, true
		}
		return nil, false
	default:
		return raw, true
	}
}

type mcpAgentTool struct {
	name        string
	description string
	properties  map[string]any
	required    []string
	conn        *mcpServerConn
	remoteName  string
	opts        fantasy.ProviderOptions
}

func newMCPAgentTool(conn *mcpServerConn, t *mcp.Tool) *mcpAgentTool {
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
		name:        fmt.Sprintf("mcp__%s__%s", conn.srv.Name, t.Name),
		description: t.Description,
		properties:  properties,
		required:    required,
		conn:        conn,
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
	session, err := m.conn.ensure(ctx)
	if err != nil {
		return fantasy.NewTextErrorResponse(m.name + ": server unavailable: " + err.Error()), nil
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: m.remoteName, Arguments: args})
	if err != nil {
		if session, rerr := m.conn.ensure(ctx); rerr == nil {
			res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: m.remoteName, Arguments: args})
		}
		if err != nil {
			return fantasy.NewTextErrorResponse(m.name + ": " + err.Error()), nil
		}
	}
	return m.convertResult(res), nil
}

func (m *mcpAgentTool) convertResult(res *mcp.CallToolResult) fantasy.ToolResponse {
	var out strings.Builder
	var imageData []byte
	var imageMIME string
	for _, c := range res.Content {
		switch tc := c.(type) {
		case *mcp.TextContent:
			out.WriteString(tc.Text)
		case *mcp.ImageContent:
			if imageData == nil {
				imageData = tc.Data
				imageMIME = tc.MIMEType
			}
		case *mcp.AudioContent:
			fmt.Fprintf(&out, "(audio result: %s, %d bytes)", tc.MIMEType, len(tc.Data))
		case *mcp.EmbeddedResource:
			if tc.Resource == nil {
				continue
			}
			if tc.Resource.Text != "" {
				out.WriteString(tc.Resource.Text)
			} else {
				fmt.Fprintf(&out, "(binary resource %s: %s, %d bytes)",
					tc.Resource.URI, tc.Resource.MIMEType, len(tc.Resource.Blob))
			}
		case *mcp.ResourceLink:
			fmt.Fprintf(&out, "(resource link: %s)", tc.URI)
		}
	}
	body := out.String()
	if res.IsError {
		if strings.TrimSpace(body) == "" {
			body = "(empty error result)"
		}
		return fantasy.NewTextErrorResponse(body)
	}
	if imageData != nil {
		if m.conn.mgr.imagesOK() {
			return fantasy.NewImageResponse(imageData, imageMIME)
		}
		fmt.Fprintf(&out, "[image result omitted — the current model has no vision: %s, %d bytes]",
			imageMIME, len(imageData))
		body = out.String()
	}
	if strings.TrimSpace(body) == "" {
		body = "(empty result)"
	}
	return fantasy.NewTextResponse(TruncateMiddle(body))
}

func (m *mcpAgentTool) ProviderOptions() fantasy.ProviderOptions        { return m.opts }
func (m *mcpAgentTool) SetProviderOptions(opts fantasy.ProviderOptions) { m.opts = opts }
