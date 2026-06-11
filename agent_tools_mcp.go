package main

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
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// agentMCPServer describes one MCP server the agent session should
// attach: ask's own loopback bridge (linear/workflow tools), the
// project-level GitHub MCP, and every user-configured server
// (mcp_servers.go) all flow through here.
type agentMCPServer struct {
	name string
	cfg  mcpServerConfig
	// skip filters out tools the harness already provides natively
	// (ask_user_question, end_turn) so the model never sees duplicates.
	skip map[string]bool
}

// agentMCPConnectTimeout bounds the connect+list handshake per server;
// a slow remote must not stall session start indefinitely.
const agentMCPConnectTimeout = 15 * time.Second

// agentMCPPingTimeout bounds the liveness probe before each tool call.
const agentMCPPingTimeout = 5 * time.Second

func (s agentMCPServer) connectTimeout() time.Duration {
	if s.cfg.TimeoutSeconds > 0 {
		return time.Duration(s.cfg.TimeoutSeconds) * time.Second
	}
	return agentMCPConnectTimeout
}

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

// mcpTransportFor builds the wire transport for one server. Swappable
// in tests so the whole manager can run against in-memory transports.
var mcpTransportFor = func(srv agentMCPServer, oauth *askMCPOAuthHandler) (mcp.Transport, error) {
	switch srv.cfg.effectiveType() {
	case mcpServerTypeStdio:
		cmd := exec.Command(srv.cfg.Command, srv.cfg.Args...)
		cmd.Env = os.Environ()
		for k, v := range srv.cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	case mcpServerTypeSSE:
		httpClient := &http.Client{}
		if len(srv.cfg.Headers) > 0 {
			httpClient.Transport = headerRoundTripper{headers: srv.cfg.Headers}
		}
		return &mcp.SSEClientTransport{Endpoint: srv.cfg.URL, HTTPClient: httpClient}, nil
	default:
		httpClient := &http.Client{}
		if len(srv.cfg.Headers) > 0 {
			httpClient.Transport = headerRoundTripper{headers: srv.cfg.Headers}
		}
		// The standalone SSE stream carries server-initiated traffic
		// (tools/list_changed, out-of-call elicitation). Stateless
		// servers (ask's loopback bridge) reject the GET with a
		// spec-compliant 405, which the SDK handles gracefully.
		t := &mcp.StreamableClientTransport{
			Endpoint:   srv.cfg.URL,
			HTTPClient: httpClient,
		}
		if oauth != nil {
			t.OAuthHandler = oauth
		}
		return t, nil
	}
}

// mcpManager owns every MCP server attached to one agent session:
// connection lifecycle (lazy ping-and-rebuild before each call),
// tool-list refresh notifications, elicitation routing into the ask
// modal, and teardown.
type mcpManager struct {
	tabID    int
	imagesOK func() bool
	// onToolsChanged fires (from SDK goroutines) after a server's tool
	// list is re-read so the session can swap its tool set at the next
	// turn boundary.
	onToolsChanged func()

	mu    sync.Mutex
	conns []*mcpServerConn
}

func newMCPManager(tabID int, imagesOK func() bool, onToolsChanged func()) *mcpManager {
	if imagesOK == nil {
		imagesOK = func() bool { return false }
	}
	return &mcpManager{tabID: tabID, imagesOK: imagesOK, onToolsChanged: onToolsChanged}
}

// attachAll connects every server concurrently. Failures are logged
// and skipped — a dead remote must not block a coding session.
func (m *mcpManager) attachAll(ctx context.Context, servers []agentMCPServer) {
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv agentMCPServer) {
			defer wg.Done()
			if err := m.attach(ctx, srv); err != nil {
				debugLog("mcp: %s skipped: %v", srv.name, err)
			}
		}(srv)
	}
	wg.Wait()
}

func (m *mcpManager) attach(ctx context.Context, srv agentMCPServer) error {
	conn := &mcpServerConn{mgr: m, srv: srv}
	if srv.cfg.OAuth && srv.cfg.effectiveType() != mcpServerTypeStdio {
		oauth, err := newMCPOAuthHandler(srv.cfg.URL)
		if err != nil {
			return fmt.Errorf("oauth setup %s: %w", srv.name, err)
		}
		conn.oauth = oauth
	}
	connectCtx, cancel := context.WithTimeout(ctx, srv.connectTimeout())
	defer cancel()
	if err := conn.connect(connectCtx); err != nil {
		conn.close()
		return err
	}
	if err := conn.refreshTools(connectCtx); err != nil {
		conn.close()
		return fmt.Errorf("list tools on %s: %w", srv.name, err)
	}
	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()
	return nil
}

// tools snapshots the current fantasy tool set across all servers.
func (m *mcpManager) tools() []fantasy.AgentTool {
	m.mu.Lock()
	conns := append([]*mcpServerConn(nil), m.conns...)
	m.mu.Unlock()
	var out []fantasy.AgentTool
	for _, c := range conns {
		out = append(out, c.currentTools()...)
	}
	return out
}

func (m *mcpManager) close() {
	m.mu.Lock()
	conns := m.conns
	m.conns = nil
	m.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func (m *mcpManager) toolsChanged() {
	if m.onToolsChanged != nil {
		m.onToolsChanged()
	}
}

// mcpServerConn is one live server attachment.
type mcpServerConn struct {
	mgr   *mcpManager
	srv   agentMCPServer
	oauth *askMCPOAuthHandler

	mu      sync.Mutex
	session *mcp.ClientSession
	tools   []fantasy.AgentTool
}

// connect dials the server and installs the client-side handlers:
// elicitation → ask's question modal, tools/list_changed → re-list and
// notify the session.
func (c *mcpServerConn) connect(ctx context.Context) error {
	transport, err := mcpTransportFor(c.srv, c.oauth)
	if err != nil {
		return fmt.Errorf("transport %s: %w", c.srv.name, err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ask-agent", Version: "1.0.0"}, &mcp.ClientOptions{
		ElicitationHandler: c.handleElicitation,
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			go c.onToolListChanged()
		},
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.srv.name, err)
	}
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	return nil
}

// ensure returns a live session, pinging first and rebuilding the
// connection once when the ping fails (crush's lazy-renew scheme — no
// background watchdog, liveness is checked at the moment it matters).
func (c *mcpServerConn) ensure(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session != nil {
		pingCtx, cancel := context.WithTimeout(ctx, agentMCPPingTimeout)
		err := session.Ping(pingCtx, nil)
		cancel()
		if err == nil {
			return session, nil
		}
		debugLog("mcp: %s ping failed (%v); reconnecting", c.srv.name, err)
		_ = session.Close()
		c.mu.Lock()
		if c.session == session {
			c.session = nil
		}
		c.mu.Unlock()
	}
	connectCtx, cancel := context.WithTimeout(ctx, c.srv.connectTimeout())
	defer cancel()
	if err := c.connect(connectCtx); err != nil {
		return nil, err
	}
	if err := c.refreshTools(connectCtx); err != nil {
		debugLog("mcp: %s re-list after reconnect: %v", c.srv.name, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session, nil
}

// refreshTools re-reads the server's tool list and rebuilds the
// fantasy wrappers, honouring the skip filter and the per-server
// enable/disable lists.
func (c *mcpServerConn) refreshTools(ctx context.Context) error {
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == nil {
		return fmt.Errorf("%s: no session", c.srv.name)
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	tools := make([]fantasy.AgentTool, 0, len(listed.Tools))
	for _, t := range listed.Tools {
		if c.srv.skip[t.Name] || !mcpToolAllowed(c.srv.cfg, t.Name) {
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

// onToolListChanged handles a tools/list_changed notification: re-read
// the list and let the session pick the new set up at the next turn.
func (c *mcpServerConn) onToolListChanged() {
	ctx, cancel := context.WithTimeout(context.Background(), agentMCPConnectTimeout)
	defer cancel()
	if err := c.refreshTools(ctx); err != nil {
		debugLog("mcp: %s tool-list refresh: %v", c.srv.name, err)
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
		c.oauth.close()
	}
}

// handleElicitation maps the MCP elicitation request onto ask's
// question modal. Form mode builds one modal question per schema
// property (enum → options, boolean → yes/no, anything else → a
// free-text "Enter your own" row); an empty schema becomes a plain
// accept/decline confirmation. URL mode has no modal affordance and is
// declined. Headless (workflow) tabs decline so chains never stall.
func (c *mcpServerConn) handleElicitation(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	params := req.Params
	if params == nil {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	if params.URL != "" {
		debugLog("mcp: %s url-mode elicitation declined (%s)", c.srv.name, params.URL)
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	props, required := elicitationSchemaProperties(params.RequestedSchema)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	questions := make([]mcpQuestion, 0, len(names)+1)
	if len(names) == 0 {
		questions = append(questions, mcpQuestion{
			Kind:    "pick_one",
			Prompt:  params.Message,
			Options: []mcpOption{{Label: "Accept"}, {Label: "Decline"}},
		})
	}
	for _, name := range names {
		questions = append(questions, elicitationQuestion(c.srv.name, params.Message, name, props[name], required[name]))
	}

	reply := make(chan askReply, 1)
	if !agentSendToProgram(askToolRequestMsg{
		tabID:     c.mgr.tabID,
		questions: convertMCPQuestions(questions),
		reply:     reply,
	}) {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	select {
	case resp := <-reply:
		switch {
		case resp.headless:
			return &mcp.ElicitResult{Action: "decline"}, nil
		case resp.cancelled:
			return &mcp.ElicitResult{Action: "cancel"}, nil
		}
		answers := convertMCPAnswers(questions, resp.answers)
		if len(names) == 0 {
			if len(answers) > 0 && len(answers[0].Picks) > 0 && answers[0].Picks[0] == "Accept" {
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
			}
			return &mcp.ElicitResult{Action: "decline"}, nil
		}
		content := map[string]any{}
		for i, name := range names {
			if v, ok := elicitationAnswerValue(props[name], answers[i]); ok {
				content[name] = v
			}
		}
		return &mcp.ElicitResult{Action: "accept", Content: content}, nil
	case <-ctx.Done():
		return &mcp.ElicitResult{Action: "cancel"}, nil
	}
}

// elicitationSchemaProperties extracts the flat property map and the
// required-field set from the requested 2020-12 schema.
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

// elicitationQuestion builds one modal question for a schema property.
func elicitationQuestion(server, message, name string, prop map[string]any, req bool) mcpQuestion {
	prompt := fmt.Sprintf("[%s] %s — %s", server, message, name)
	if desc, ok := prop["description"].(string); ok && desc != "" {
		prompt += " (" + desc + ")"
	}
	if req {
		prompt += " (required)"
	}
	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		opts := make([]mcpOption, 0, len(enum))
		for _, e := range enum {
			opts = append(opts, mcpOption{Label: fmt.Sprintf("%v", e)})
		}
		return mcpQuestion{Kind: "pick_one", Prompt: prompt, Options: opts}
	}
	if t, _ := prop["type"].(string); t == "boolean" {
		return mcpQuestion{Kind: "pick_one", Prompt: prompt,
			Options: []mcpOption{{Label: "yes"}, {Label: "no"}}}
	}
	// Free-form: the modal renders a lone "Enter your own" row.
	return mcpQuestion{Kind: "pick_one", Prompt: prompt, AllowCustom: true}
}

// elicitationAnswerValue converts the user's pick back into the
// schema's value space.
func elicitationAnswerValue(prop map[string]any, ans mcpAnswer) (any, bool) {
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

// mcpAgentTool adapts one server-side MCP tool to fantasy's AgentTool.
// NewAgentTool's reflection schema can't express arbitrary remote
// schemas, so this implements the interface directly and passes the
// server's input schema through verbatim. Calls go through the conn's
// ensure() so a dropped server is transparently redialed.
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
		name:        fmt.Sprintf("mcp__%s__%s", conn.srv.name, t.Name),
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
		// One renew+retry: the ping can pass and the call still hit a
		// just-died transport.
		if session, rerr := m.conn.ensure(ctx); rerr == nil {
			res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: m.remoteName, Arguments: args})
		}
		if err != nil {
			return fantasy.NewTextErrorResponse(m.name + ": " + err.Error()), nil
		}
	}
	return m.convertResult(res), nil
}

// convertResult flattens the MCP content list. Image content becomes a
// real image response when the session's model has vision (the first
// image wins — fantasy tool responses carry one media payload);
// otherwise a text placeholder so the model knows something was there.
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
	return fantasy.NewTextResponse(truncateMiddle(body))
}

func (m *mcpAgentTool) ProviderOptions() fantasy.ProviderOptions        { return m.opts }
func (m *mcpAgentTool) SetProviderOptions(opts fantasy.ProviderOptions) { m.opts = opts }
