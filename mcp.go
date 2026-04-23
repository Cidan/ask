package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const askProgressInterval = 20 * time.Second

type mcpOption struct {
	Label   string `json:"label" jsonschema:"short label for the option"`
	Diagram string `json:"diagram,omitempty" jsonschema:"required only for pick_diagram kind: monospace box-drawing art, max 40 cols x 12 rows"`
}

type mcpQuestion struct {
	Kind        string      `json:"kind" jsonschema:"one of pick_one, pick_many, pick_diagram"`
	Prompt      string      `json:"prompt" jsonschema:"the question shown to the user"`
	Options     []mcpOption `json:"options" jsonschema:"list of options for the user to choose from"`
	AllowCustom bool        `json:"allow_custom,omitempty" jsonschema:"append an Enter-your-own free-text option (pick_one and pick_many only)"`
}

type askInput struct {
	Questions []mcpQuestion `json:"questions" jsonschema:"one or more questions to ask the user together in a tabbed modal"`
}

type mcpAnswer struct {
	Picks  []string `json:"picks" jsonschema:"labels of options the user selected; empty if user only entered a custom answer"`
	Custom string   `json:"custom,omitempty" jsonschema:"free-form text if the user used Enter your own"`
	Note   string   `json:"note,omitempty" jsonschema:"additional note the user attached via n"`
}

type askOutput struct {
	Answers   []mcpAnswer `json:"answers" jsonschema:"answers in the same order as input questions"`
	Cancelled bool        `json:"cancelled,omitempty" jsonschema:"true if the user dismissed the dialog without submitting"`
}

type askReply struct {
	answers   []qAnswer
	cancelled bool
}

type askToolRequestMsg struct {
	tabID     int
	questions []question
	reply     chan askReply
}

type approvalIn struct {
	ToolName  string         `json:"tool_name"`
	Input     map[string]any `json:"input"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
}

// permissionRule mirrors the Claude Code permission-rule wire shape:
// toolName identifies the tool (e.g. "Edit", "Bash"); ruleContent narrows the
// rule to a specific target (file_path for file tools, command for Bash).
// An empty ruleContent means "every invocation of this tool".
type permissionRule struct {
	toolName    string
	ruleContent string
}

type approvalReply struct {
	allow    bool
	remember *permissionRule
}

type approvalRequestMsg struct {
	tabID     int
	toolName  string
	input     map[string]any
	toolUseID string
	reply     chan approvalReply
}

const approvalToolDescription = `Permission prompt callback wired up via claude's --permission-prompt-tool.
Do not call this tool directly. It is invoked by claude before running a tool so
the ask TUI can show an approval modal to the user; the user answers allow or
deny and the decision is returned as a stringified JSON body.`

// approvalInputSchema is emitted verbatim in tools/list. The low-level
// Server.AddTool path doesn't auto-generate a schema, so we supply one by hand.
var approvalInputSchema = json.RawMessage(`{"type":"object","properties":{"tool_name":{"type":"string"},"input":{"type":"object"},"tool_use_id":{"type":"string"}},"required":["tool_name","input"]}`)

const askToolDescription = `Ask the user one or more questions through a tabbed modal in the ask terminal UI.

Each question is one of three kinds:
  - "pick_one": user picks exactly one option
  - "pick_many": user picks zero or more options
  - "pick_diagram": user picks exactly one option; each option has an ASCII-art
    preview that is rendered in a side box as the user navigates the list

All submitted questions are displayed together as tabs; the user answers each
before submitting. Answers are returned in input order.

Diagram format (pick_diagram only; strict):
  - Monospace box-drawing characters only: ╭╮╰╯─│├┤┬┴┼
  - Fill blocks: ░ for content areas, ▓ for interactive or accent areas
  - No emoji, no tabs, no trailing whitespace
  - At most 40 columns wide and 12 rows tall; all diagrams in one question are
    padded to the same bounding box before rendering, so smaller is fine

Set allow_custom=true on pick_one or pick_many to append an Enter-your-own
option that accepts free-form multi-line text from the user.`

// teaProgramPtr is shared by every tab's mcpBridge. main.go stores the
// *tea.Program into it after tea.NewProgram so bridges can route tool
// requests (ask / approval) back to the owning tab through the app.
var teaProgramPtr atomic.Pointer[tea.Program]

func setTeaProgram(p *tea.Program) { teaProgramPtr.Store(p) }

type mcpBridge struct {
	tabID  int
	port   int
	ln     net.Listener
	server *mcp.Server

	// alwaysAllow is a per-tab session allowlist. When the user picks
	// "Always allow" in the approval modal, the corresponding permissionRule
	// lands here, and subsequent approval_prompt invocations matching the
	// same rule auto-allow without popping a modal. We also return the same
	// rule back to claude via updatedPermissions so its own session-scoped
	// permission engine caches it, but the in-memory copy is the belt-and-
	// suspenders path in case claude re-asks anyway.
	allowMu     sync.Mutex
	alwaysAllow map[permissionRule]struct{}
}

func newMCPBridge(tabID int) (*mcpBridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &mcpBridge{
		tabID:       tabID,
		ln:          ln,
		port:        ln.Addr().(*net.TCPAddr).Port,
		alwaysAllow: map[permissionRule]struct{}{},
	}
	b.server = mcp.NewServer(&mcp.Implementation{Name: "ask", Version: "0.1"}, nil)
	mcp.AddTool(b.server, &mcp.Tool{
		Name:        "ask_user_question",
		Description: askToolDescription,
	}, b.askTool)
	b.server.AddTool(&mcp.Tool{
		Name:        "approval_prompt",
		Description: approvalToolDescription,
		InputSchema: approvalInputSchema,
	}, b.approvalTool)
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return b.server },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true},
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/hooks/subagent-start", b.handleHookSubagentStart)
	mux.HandleFunc("/hooks/subagent-stop", b.handleHookSubagentStop)
	mux.Handle("/", handler)
	go func() {
		_ = http.Serve(b.ln, mux)
	}()
	return b, nil
}

// hookInput is the subset of the claude hook-event JSON we consume.
// The wire schema is defined in the Python SDK at
// src/claude_agent_sdk/types.py (SubagentStartHookInput /
// SubagentStopHookInput). We ignore the rest of the fields.
type hookInput struct {
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
}

func (b *mcpBridge) handleHookSubagentStart(w http.ResponseWriter, r *http.Request) {
	ev, ok := b.decodeHook(w, r)
	if !ok {
		return
	}
	debugLog("hook SubagentStart tab=%d agent_id=%s agent_type=%s",
		b.tabID, ev.AgentID, ev.AgentType)
	if p := teaProgramPtr.Load(); p != nil {
		p.Send(hookSubagentStartMsg{
			tabID:     b.tabID,
			agentID:   ev.AgentID,
			agentType: ev.AgentType,
		})
	}
	w.WriteHeader(http.StatusOK)
}

func (b *mcpBridge) handleHookSubagentStop(w http.ResponseWriter, r *http.Request) {
	ev, ok := b.decodeHook(w, r)
	if !ok {
		return
	}
	debugLog("hook SubagentStop tab=%d agent_id=%s agent_type=%s",
		b.tabID, ev.AgentID, ev.AgentType)
	if p := teaProgramPtr.Load(); p != nil {
		p.Send(hookSubagentStopMsg{
			tabID:     b.tabID,
			agentID:   ev.AgentID,
			agentType: ev.AgentType,
		})
	}
	w.WriteHeader(http.StatusOK)
}

func (b *mcpBridge) decodeHook(w http.ResponseWriter, r *http.Request) (hookInput, bool) {
	var ev hookInput
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return hookInput{}, false
	}
	return ev, true
}

func (b *mcpBridge) stop() {
	if b == nil || b.ln == nil {
		return
	}
	_ = b.ln.Close()
}

func (b *mcpBridge) askTool(ctx context.Context, req *mcp.CallToolRequest, in askInput) (*mcp.CallToolResult, askOutput, error) {
	if len(in.Questions) == 0 {
		return nil, askOutput{}, errors.New("at least one question is required")
	}
	p := teaProgramPtr.Load()
	if p == nil {
		return nil, askOutput{}, errors.New("ask UI not ready")
	}
	reply := make(chan askReply, 1)
	p.Send(askToolRequestMsg{
		tabID:     b.tabID,
		questions: convertMCPQuestions(in.Questions),
		reply:     reply,
	})
	if token := req.Params.GetProgressToken(); token != nil && req.Session != nil {
		done := make(chan struct{})
		defer close(done)
		go b.pingProgress(ctx, req.Session, token, done)
	}
	select {
	case resp := <-reply:
		if resp.cancelled {
			out := askOutput{Cancelled: true}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "user cancelled the dialog"}},
				IsError: true,
			}, out, nil
		}
		out := askOutput{Answers: convertMCPAnswers(in.Questions, resp.answers)}
		body, err := json.Marshal(out)
		if err != nil {
			return nil, askOutput{}, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
		}, out, nil
	case <-ctx.Done():
		return nil, askOutput{}, ctx.Err()
	}
}

// approvalTool is registered via the low-level Server.AddTool. That path skips
// auto-populating CallToolResult.StructuredContent and auto-running our output
// through jsonschema defaults/validation, so the wire response is exactly the
// stringified-JSON text block claude's --permission-prompt-tool expects.
//
// Wire shape of the decision body is documented by the Claude Agent SDK: see
// claude-agent-sdk-python src/claude_agent_sdk/_internal/query.py around the
// "if isinstance(response, PermissionResultAllow)" branch, and the
// PermissionUpdate serializer in src/claude_agent_sdk/types.py. An allow can
// attach updatedPermissions=[{type:"addRules", destination, behavior, rules}]
// with destination "session" | "userSettings" | "projectSettings" |
// "localSettings"; "session" keeps the new rule in-memory for the current run.
func (b *mcpBridge) approvalTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in approvalIn
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return nil, fmt.Errorf("approval_prompt: %w", err)
		}
	}
	rule := permissionRuleFor(in.ToolName, in.Input)
	if b.ruleAlwaysAllowed(rule) {
		body := buildApprovalBody(true, in.Input, nil)
		debugLog("approval_prompt cache-hit tool=%s rule=%q body=%s", in.ToolName, rule.ruleContent, string(body))
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
		}, nil
	}
	p := teaProgramPtr.Load()
	if p == nil {
		return nil, errors.New("ask UI not ready")
	}
	reply := make(chan approvalReply, 1)
	p.Send(approvalRequestMsg{
		tabID:     b.tabID,
		toolName:  in.ToolName,
		input:     in.Input,
		toolUseID: in.ToolUseID,
		reply:     reply,
	})
	if token := req.Params.GetProgressToken(); token != nil && req.Session != nil {
		done := make(chan struct{})
		defer close(done)
		go b.pingProgress(ctx, req.Session, token, done)
	}
	select {
	case resp := <-reply:
		if resp.allow && resp.remember != nil {
			b.rememberAlwaysAllow(*resp.remember)
		}
		body := buildApprovalBody(resp.allow, in.Input, resp.remember)
		debugLog("approval_prompt reply tool=%s allow=%v remember=%v body=%s",
			in.ToolName, resp.allow, resp.remember != nil, string(body))
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// permissionRuleFor maps a tool invocation to its narrowest rule scope. For
// file-touching tools we key on file_path, for Bash on the command, otherwise
// we fall back to a bare rule (empty ruleContent = any invocation).
func permissionRuleFor(toolName string, input map[string]any) permissionRule {
	r := permissionRule{toolName: toolName}
	switch toolName {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "Read":
		if p, _ := input["file_path"].(string); p != "" {
			r.ruleContent = p
		}
	case "Bash":
		if c, _ := input["command"].(string); c != "" {
			r.ruleContent = c
		}
	}
	return r
}

func (b *mcpBridge) ruleAlwaysAllowed(rule permissionRule) bool {
	if rule.toolName == "" {
		return false
	}
	b.allowMu.Lock()
	defer b.allowMu.Unlock()
	_, ok := b.alwaysAllow[rule]
	return ok
}

func (b *mcpBridge) rememberAlwaysAllow(rule permissionRule) {
	if rule.toolName == "" {
		return
	}
	b.allowMu.Lock()
	defer b.allowMu.Unlock()
	b.alwaysAllow[rule] = struct{}{}
}

// buildApprovalBody serialises the decision in the shape claude's
// --permission-prompt-tool consumes. When remember is non-nil we also emit a
// session-scoped addRules PermissionUpdate so claude's own permission engine
// stops asking for this rule within the current run.
func buildApprovalBody(allow bool, input map[string]any, remember *permissionRule) []byte {
	if !allow {
		body, _ := json.Marshal(map[string]any{"behavior": "deny", "message": "user denied the approval"})
		return body
	}
	updated := input
	if updated == nil {
		updated = map[string]any{}
	}
	decision := map[string]any{"behavior": "allow", "updatedInput": updated}
	if remember != nil && remember.toolName != "" {
		var ruleContent any
		if remember.ruleContent != "" {
			ruleContent = remember.ruleContent
		}
		decision["updatedPermissions"] = []map[string]any{{
			"type":        "addRules",
			"destination": "session",
			"behavior":    "allow",
			"rules": []map[string]any{{
				"toolName":    remember.toolName,
				"ruleContent": ruleContent,
			}},
		}}
	}
	body, _ := json.Marshal(decision)
	return body
}

func (b *mcpBridge) pingProgress(ctx context.Context, sess *mcp.ServerSession, token any, done <-chan struct{}) {
	ticker := time.NewTicker(askProgressInterval)
	defer ticker.Stop()
	var n float64
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			n++
			_ = sess.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      n,
				Message:       "waiting for user",
			})
		}
	}
}

func convertMCPQuestions(qs []mcpQuestion) []question {
	out := make([]question, len(qs))
	for i, q := range qs {
		var kind qKind
		switch q.Kind {
		case "pick_many":
			kind = qPickMany
		case "pick_diagram":
			kind = qPickDiagram
		default:
			kind = qPickOne
		}
		labels := make([]string, 0, len(q.Options)+1)
		diagrams := make([]string, 0, len(q.Options)+1)
		for _, o := range q.Options {
			labels = append(labels, o.Label)
			diagrams = append(diagrams, o.Diagram)
		}
		if q.AllowCustom && kind != qPickDiagram {
			labels = append(labels, "Enter your own")
			diagrams = append(diagrams, "")
		}
		out[i] = question{
			kind:     kind,
			prompt:   q.Prompt,
			options:  labels,
			diagrams: diagrams,
		}
	}
	return out
}

func convertMCPAnswers(qs []mcpQuestion, answers []qAnswer) []mcpAnswer {
	out := make([]mcpAnswer, len(qs))
	for i, q := range qs {
		ans := answers[i]
		customIdx := -1
		if q.AllowCustom && q.Kind != "pick_diagram" {
			customIdx = len(q.Options)
		}
		var picks []string
		for idx := 0; idx < len(q.Options); idx++ {
			if ans.picks[idx] {
				picks = append(picks, q.Options[idx].Label)
			}
		}
		if picks == nil {
			picks = []string{}
		}
		custom := ""
		if customIdx >= 0 && ans.picks[customIdx] {
			custom = ans.custom
		}
		out[i] = mcpAnswer{
			Picks:  picks,
			Custom: custom,
			Note:   ans.note,
		}
	}
	return out
}
