package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"
	"google.golang.org/genai"
)

// MCPServer describes one MCP server to attach.
type MCPServer struct {
	Name string
	Cfg  MCPServerConfig
	Skip map[string]bool
}

// MCPServerStatusKind is a server's live connection/auth state in a session.
type MCPServerStatusKind int

const (
	MCPStatusConnected MCPServerStatusKind = iota
	MCPStatusNeedsAuth
	MCPStatusError
)

// MCPStatus is a snapshot of one server's live state in a session.
type MCPStatus struct {
	Name   string
	Kind   MCPServerStatusKind
	Detail string // error text when Kind == MCPStatusError
}

// oauthWanted decides whether to attach an OAuth handler to a server: an
// explicit request (oauth: true), or an http/sse server that is not already
// using header-based auth (a 401 then means it wants OAuth). This is the
// "just-in-time for all http/sse" behavior; header-authed servers (e.g. a
// PAT) are left alone unless they opt in.
func oauthWanted(c MCPServerConfig) bool {
	if c.EffectiveType() == MCPServerTypeStdio {
		return false
	}
	if c.OAuth {
		return true
	}
	return len(c.Headers) == 0
}

// sameMCPConfig reports whether two server configs would connect identically.
func sameMCPConfig(a, b MCPServerConfig) bool {
	return reflect.DeepEqual(a, b)
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

// oauthRoundTripper injects the bearer token from an MCPOAuthHandler and, on
// a 401, runs Authorize then retries once. Used for the SSE transport, whose
// SDK client has no native OAuthHandler hook.
type oauthRoundTripper struct {
	base    http.RoundTripper
	handler *MCPOAuthHandler
}

func (rt *oauthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	if src, err := rt.handler.TokenSource(req.Context()); err == nil && src != nil {
		if tok, err := src.Token(); err == nil && tok != nil {
			tok.SetAuthHeader(req)
		}
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if aerr := rt.handler.Authorize(req.Context(), req, resp); aerr != nil {
		return resp, nil // surface the 401 (e.g. the needs-auth path)
	}
	if src, err := rt.handler.TokenSource(req.Context()); err == nil && src != nil {
		if tok, err := src.Token(); err == nil && tok != nil {
			tok.SetAuthHeader(req)
			return base.RoundTrip(req)
		}
	}
	return resp, nil
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
		var base http.RoundTripper = http.DefaultTransport
		if len(srv.Cfg.Headers) > 0 {
			base = headerRoundTripper{base: base, headers: srv.Cfg.Headers}
		}
		if oauth != nil {
			base = &oauthRoundTripper{base: base, handler: oauth}
		}
		httpClient.Transport = base
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
	tabID           int
	imagesOK        func() bool
	onToolsChanged  func()
	onStatusChanged func()
	interaction     engine.InteractionHandler

	mu     sync.Mutex
	conns  []*mcpServerConn
	states map[string]MCPStatus // by server name; includes needs-auth/error servers
}

// NewMCPManager creates a new MCPManager. onStatusChanged fires whenever a
// server's connection/auth state changes (nil is allowed).
func NewMCPManager(tabID int, imagesOK func() bool, onToolsChanged, onStatusChanged func(), interaction engine.InteractionHandler) *MCPManager {
	if imagesOK == nil {
		imagesOK = func() bool { return false }
	}
	if interaction == nil {
		interaction = engine.HeadlessInteractionHandler{}
	}
	return &MCPManager{
		tabID:           tabID,
		imagesOK:        imagesOK,
		onToolsChanged:  onToolsChanged,
		onStatusChanged: onStatusChanged,
		interaction:     interaction,
		states:          map[string]MCPStatus{},
	}
}

func (m *MCPManager) setStatus(name string, kind MCPServerStatusKind, detail string) {
	m.mu.Lock()
	if m.states == nil {
		m.states = map[string]MCPStatus{}
	}
	m.states[name] = MCPStatus{Name: name, Kind: kind, Detail: detail}
	m.mu.Unlock()
	if m.onStatusChanged != nil {
		m.onStatusChanged()
	}
}

// Statuses returns a snapshot of every tracked server's live state.
func (m *MCPManager) Statuses() []MCPStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MCPStatus, 0, len(m.states))
	for _, s := range m.states {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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

// Attach connects a single server. A server that needs interactive OAuth is
// tracked as needs-auth (not a hard failure); other failures are tracked as
// errors; success is tracked as connected.
func (m *MCPManager) Attach(ctx context.Context, srv MCPServer) error {
	conn := &mcpServerConn{mgr: m, srv: srv}
	if oauthWanted(srv.Cfg) {
		oauth, err := NewMCPOAuthHandler(srv.Cfg.URL, false) // startup: non-interactive
		if err != nil {
			m.setStatus(srv.Name, MCPStatusError, err.Error())
			return fmt.Errorf("oauth setup %s: %w", srv.Name, err)
		}
		conn.oauth = oauth
	}
	connectCtx, cancel := context.WithTimeout(ctx, srv.ConnectTimeout())
	defer cancel()
	if err := conn.connect(connectCtx); err != nil {
		conn.close()
		if errors.Is(err, ErrMCPInteractiveAuthRequired) {
			m.setStatus(srv.Name, MCPStatusNeedsAuth, "")
			return nil
		}
		m.setStatus(srv.Name, MCPStatusError, err.Error())
		return err
	}
	if err := conn.refreshTools(connectCtx); err != nil {
		conn.close()
		m.setStatus(srv.Name, MCPStatusError, err.Error())
		return fmt.Errorf("list tools on %s: %w", srv.Name, err)
	}
	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()
	m.setStatus(srv.Name, MCPStatusConnected, "")
	return nil
}

// Detach closes and removes the connection (and tracked state) for a server
// by name. No-op if absent.
func (m *MCPManager) Detach(name string) {
	m.mu.Lock()
	var kept, closing []*mcpServerConn
	for _, c := range m.conns {
		if c.srv.Name == name {
			closing = append(closing, c)
		} else {
			kept = append(kept, c)
		}
	}
	m.conns = kept
	delete(m.states, name)
	m.mu.Unlock()
	for _, c := range closing {
		c.close()
	}
	if len(closing) > 0 {
		m.toolsChanged()
	}
	if m.onStatusChanged != nil {
		m.onStatusChanged()
	}
}

// Reconcile brings the live connection set in line with the desired servers:
// attaches any that are new, detaches any no longer desired, and reattaches
// one whose config changed. Called when the browser toggles a server or an
// authorization completes.
func (m *MCPManager) Reconcile(ctx context.Context, desired []MCPServer) {
	want := map[string]MCPServer{}
	for _, s := range desired {
		want[s.Name] = s
	}
	m.mu.Lock()
	have := map[string]MCPServerConfig{}
	for _, c := range m.conns {
		have[c.srv.Name] = c.srv.Cfg
	}
	tracked := make([]string, 0, len(m.states))
	for n := range m.states {
		tracked = append(tracked, n)
	}
	m.mu.Unlock()

	for name := range have {
		if _, ok := want[name]; !ok {
			m.Detach(name)
		}
	}
	for _, name := range tracked {
		if _, ok := want[name]; !ok {
			m.Detach(name)
		}
	}
	for name, srv := range want {
		if cfg, ok := have[name]; ok && sameMCPConfig(cfg, srv.Cfg) {
			continue
		}
		if _, ok := have[name]; ok {
			m.Detach(name)
		}
		_ = m.Attach(ctx, srv)
	}
	m.toolsChanged()
}

// Toolsets returns all active ADK mcptoolset.Toolset instances.
func (m *MCPManager) Toolsets() []tool.Toolset {
	m.mu.Lock()
	conns := append([]*mcpServerConn(nil), m.conns...)
	m.mu.Unlock()
	var out []tool.Toolset
	for _, c := range conns {
		if ts := c.getToolset(); ts != nil {
			out = append(out, ts)
		}
	}
	return out
}

// Tools returns the snapshot of all current tools across all connected servers.
func (m *MCPManager) Tools() []Tool {
	m.mu.Lock()
	conns := append([]*mcpServerConn(nil), m.conns...)
	m.mu.Unlock()
	var out []Tool
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
	toolset tool.Toolset
	tools   []Tool
}

func (c *mcpServerConn) getToolset() tool.Toolset {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.toolset
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

	filter := func(ctx agent.ReadonlyContext, t tool.Tool) bool {
		if c.srv.Skip != nil && c.srv.Skip[t.Name()] {
			return false
		}
		return MCPToolAllowed(c.srv.Cfg, t.Name())
	}

	tsTransport, err := MCPTransportFor(c.srv, c.oauth)
	if err != nil {
		_ = session.Close()
		return fmt.Errorf("transport for mcptoolset %s: %w", c.srv.Name, err)
	}

	ts, err := mcptoolset.New(mcptoolset.Config{
		Client:     client,
		Transport:  tsTransport,
		ToolFilter: filter,
	})
	if err != nil {
		_ = session.Close()
		return fmt.Errorf("mcptoolset %s: %w", c.srv.Name, err)
	}

	c.mu.Lock()
	c.session = session
	c.toolset = ts
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
	tools := make([]Tool, 0, len(listed.Tools))
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

func (c *mcpServerConn) currentTools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Tool(nil), c.tools...)
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
	c.toolset = nil
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

func (m *mcpAgentTool) Name() string        { return m.name }
func (m *mcpAgentTool) Description() string { return m.description }
func (m *mcpAgentTool) IsLongRunning() bool { return false }
func (m *mcpAgentTool) Info() ToolInfo {
	required := m.required
	if required == nil {
		required = []string{}
	}
	return ToolInfo{
		Name:        m.name,
		Description: m.description,
		Parameters:  m.properties,
		Required:    required,
	}
}

func (m *mcpAgentTool) Declaration() *genai.FunctionDeclaration {
	schemaObj := map[string]any{
		"type":       "object",
		"properties": m.properties,
	}
	if len(m.required) > 0 {
		schemaObj["required"] = m.required
	}
	return &genai.FunctionDeclaration{
		Name:                 m.name,
		Description:          m.description,
		ParametersJsonSchema: schemaObj,
	}
}

func (m *mcpAgentTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return nil
}

func (m *mcpAgentTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	session, err := m.conn.ensure(ctx)
	if err != nil {
		return map[string]any{"result": m.name + ": server unavailable: " + err.Error(), "is_error": true}, nil
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: m.remoteName, Arguments: args})
	if err != nil {
		if session, rerr := m.conn.ensure(ctx); rerr == nil {
			res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: m.remoteName, Arguments: args})
		}
		if err != nil {
			return map[string]any{"result": m.name + ": " + err.Error(), "is_error": true}, nil
		}
	}
	resp := m.convertResult(res)
	if resp.IsError {
		return map[string]any{"result": resp.Content, "is_error": true}, nil
	}
	return map[string]any{"result": resp.Content}, nil
}

func (m *mcpAgentTool) convertResult(res *mcp.CallToolResult) ToolResponse {
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
		return NewTextErrorResponse(body)
	}
	if imageData != nil {
		if !m.conn.mgr.imagesOK() {
			fmt.Fprintf(&out, "[image result omitted — the current model has no vision: %s, %d bytes]",
				imageMIME, len(imageData))
			body = out.String()
		}
	}
	if strings.TrimSpace(body) == "" {
		body = "(empty result)"
	}
	return NewTextResponse(TruncateMiddle(body))
}
