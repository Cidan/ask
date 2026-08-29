package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// ccBatchWindow is how long the adapter waits after the first tools/call of a
// step for sibling calls before yielding the batch. Read-only tools arrive
// together (parallel); a serial tool arrives alone and this window just
// elapses. Small enough to be invisible, large enough to coalesce a burst.
var ccBatchWindow = 30 * time.Millisecond

// claudeCodeModel is the ADK model.LLM backed by one forked `claude -p`. It
// runs in lock-step with ADK's function-call loop: each GenerateContent feeds
// the contents ADK added since the last call to the child, then reads the
// child until a batch of tool calls (yielded as FunctionCalls) or the turn's
// result (yielded as final text). The child holds its own conversation
// history, so only new content is ever sent.
type claudeCodeModel struct {
	binary  string
	modelID string
	cwd     string

	nativeWebSearch bool             // pass --tools WebSearch to the child
	sink            ObservedToolSink // renders native tool activity; may be nil

	mu         sync.Mutex
	conn       ccConn
	procCancel context.CancelFunc // cancels the child's own lifetime context
	started    bool
	tools      []ccTool // served on the child's tools/list
	cursor     int      // how far into req.Contents has been sent to the child
	// pending maps a tool_use id to the control-request id ask must answer
	// with that tool's result.
	pending map[string]ccPending
	// nativeCalls maps a native (non-MCP) tool_use id to its tool name, so a
	// later tool_result frame can be matched and surfaced. Only touched from
	// readStep (one GenerateContent at a time), so it needs no lock.
	nativeCalls map[string]string
	sysSent     string // system prompt captured at spawn
	// sysPromptPath is the temp file passed to the child as
	// --system-prompt-file; removed on Close (and on a failed spawn).
	sysPromptPath string
}

type ccPending struct {
	requestID string
	innerID   json.RawMessage
}

func newClaudeCodeModel(binary, modelID, cwd string, nativeWebSearch bool, sink ObservedToolSink) *claudeCodeModel {
	return &claudeCodeModel{
		binary:          binary,
		modelID:         modelID,
		cwd:             cwd,
		nativeWebSearch: nativeWebSearch,
		sink:            sink,
		pending:         map[string]ccPending{},
		nativeCalls:     map[string]string{},
	}
}

func (m *claudeCodeModel) Name() string { return m.modelID }

// Close terminates the child. Idempotent.
func (m *claudeCodeModel) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sysPromptPath != "" {
		_ = os.Remove(m.sysPromptPath)
		m.sysPromptPath = ""
	}
	if m.conn == nil {
		return nil
	}
	// Best-effort interrupt so any in-flight turn unwinds before the kill.
	_ = m.conn.send(ccControlEnvelope{Type: "control_request", RequestID: "close", Request: map[string]any{"subtype": "interrupt"}})
	err := m.conn.close()
	if m.procCancel != nil {
		m.procCancel()
		m.procCancel = nil
	}
	m.conn = nil
	m.started = false
	return err
}

func (m *claudeCodeModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if err := m.ensure(req); err != nil {
			yield(nil, err)
			return
		}
		if err := m.drainContents(req); err != nil {
			yield(nil, err)
			return
		}
		m.readStep(ctx, stream, yield)
	}
}

// ensure spawns the child on the first call and sends the initialize control
// request. The child's MCP initialize/tools_list and the system/init frame are
// served lazily by readStep as they arrive, so ensure never blocks on them.
func (m *claudeCodeModel) ensure(req *model.LLMRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = ccToolsFromRequest(reqTools(req))
	if m.started && m.conn != nil {
		return nil
	}
	sys := reqSystemPrompt(req)
	sysPath, err := writeClaudeSystemPromptFile(sys)
	if err != nil {
		return err
	}
	argv := ccArgv(m.modelID, reqEffort(req), sysPath, m.nativeWebSearch)
	// The child lives for the model's lifetime, not one turn: dial with a
	// context derived from Background and cancelled only by Close. Binding it
	// to the per-turn ctx (which the TUI cancels at turn end) would kill the
	// child between turns and the next write would hit a broken pipe.
	procCtx, cancel := context.WithCancel(context.Background())
	conn, err := ccDial(procCtx, ccDialArgs{
		Binary: m.binary,
		Argv:   argv,
		Dir:    m.cwd,
		Env:    currentEnvMinusClaude(),
	})
	if err != nil {
		cancel()
		_ = os.Remove(sysPath)
		return err
	}
	m.conn = conn
	m.procCancel = cancel
	m.started = true
	m.sysSent = sys
	m.sysPromptPath = sysPath
	m.cursor = 0
	m.pending = map[string]ccPending{}
	if err := conn.send(ccControlEnvelope{
		Type:      "control_request",
		RequestID: "init",
		Request:   map[string]any{"subtype": "initialize", "hooks": nil},
	}); err != nil {
		return err
	}
	return nil
}

// drainContents sends everything ADK added to req.Contents since the last
// call: function responses answer pending tool calls; a fresh user turn is
// written as a user frame (interrupting any abandoned pending calls first).
// On the very first turn a resumed/materialized history (more than one entry)
// is flattened into a single context message so the child has the prior
// conversation without native resume.
func (m *claudeCodeModel) drainContents(req *model.LLMRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	contents := req.Contents
	if m.cursor == 0 && len(contents) > 1 {
		if err := m.writeUserText(ccHistoryPreamble(contents[:len(contents)-1])); err != nil {
			return err
		}
		if err := m.writeUserContent(contents[len(contents)-1]); err != nil {
			return err
		}
		m.cursor = len(contents)
		return nil
	}

	for _, c := range contents[m.cursor:] {
		switch {
		case isModelContent(c):
			// Claude produced this; it already holds it. Skip.
		case hasFunctionResponses(c):
			for _, p := range c.Parts {
				if p != nil && p.FunctionResponse != nil {
					m.answerCall(p.FunctionResponse)
				}
			}
		default:
			if len(m.pending) > 0 {
				m.interruptLocked() // ADK abandoned the turn; unblock the child
			}
			if err := m.writeUserContent(c); err != nil {
				return err
			}
		}
	}
	m.cursor = len(contents)
	return nil
}

// readStep reads child frames until a tool-call batch or the turn result,
// yielding partial text/thinking as it streams. Caller holds no lock; readStep
// takes it only for short mutations.
func (m *claudeCodeModel) readStep(ctx context.Context, stream bool, yield func(*model.LLMResponse, error) bool) {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()
	if conn == nil {
		yield(nil, errChildExited)
		return
	}

	var text, thought strings.Builder
	var calls []*genai.FunctionCall
	var usage *ccUsage
	var batch <-chan time.Time

	final := func() *model.LLMResponse {
		return m.buildResponse(text.String(), thought.String(), calls, usage, true)
	}

	for {
		select {
		case <-ctx.Done():
			m.interrupt()
			yield(nil, ctx.Err())
			return
		case <-batch:
			yield(final(), nil)
			return
		case fr, ok := <-conn.frames():
			if !ok {
				if len(calls) > 0 || text.Len() > 0 {
					yield(final(), nil)
					return
				}
				yield(nil, m.exitError())
				return
			}
			switch fr.Type {
			case "stream_event":
				// Partial deltas are live-display only, and only when ADK asked
				// for streaming (RunConfig StreamingModeSSE). In non-streaming
				// mode ADK/consumers accumulate every text event, so yielding
				// partials there would double the text against the final. The
				// completed text is accumulated from the assistant frames.
				if !stream {
					continue
				}
				if d, t := ccDelta(fr.Event); d != "" {
					if t == "thinking" {
						if !yield(m.buildResponse("", d, nil, nil, false), nil) {
							return
						}
					} else if !yield(m.buildResponse(d, "", nil, nil, false), nil) {
						return
					}
				}
			case "assistant":
				at, ath, u := ccAssistantContent(fr.Message)
				text.WriteString(at)
				thought.WriteString(ath)
				if u != nil {
					usage = u
				}
				m.observeToolUses(fr.Message)
			case "user":
				// The child echoes results of tools it ran natively (its
				// WebSearch fallback) as tool_result blocks on user frames.
				// MCP results ride the bridge, not this path, so only calls we
				// surfaced are matched.
				m.observeToolResults(fr.Message)
			case "control_request":
				call := m.handleControl(conn, fr)
				if call != nil {
					calls = append(calls, call)
					if batch == nil {
						batch = time.After(ccBatchWindow)
					}
				}
			case "result":
				if fr.Usage != nil {
					usage = fr.Usage
				}
				// result.result is the turn's authoritative final text; the
				// streamed assistant blocks are its live preview.
				finalText := fr.Result
				if finalText == "" {
					finalText = text.String()
				}
				yield(m.buildResponse(finalText, thought.String(), calls, usage, true), nil)
				return
			}
		}
	}
}

// handleControl answers an MCP bridge request and, for a tools/call, returns
// the FunctionCall to yield (the child is now blocked awaiting our reply, which
// the next GenerateContent sends from ADK's FunctionResponse).
func (m *claudeCodeModel) handleControl(conn ccConn, fr ccFrame) *genai.FunctionCall {
	var req ccControlRequest
	if err := json.Unmarshal(fr.Request, &req); err != nil {
		return nil
	}
	switch req.Subtype {
	case "can_use_tool":
		// ask is the permission gate inside its tools; always allow here.
		_ = conn.send(ccControlEnvelope{Type: "control_response", Response: map[string]any{
			"subtype": "success", "request_id": fr.RequestID,
			"response": map[string]any{"behavior": "allow", "updatedInput": req.Input},
		}})
		return nil
	case "mcp_message":
		return m.handleMCP(conn, fr.RequestID, req.Message)
	default:
		_ = conn.send(ccControlEnvelope{Type: "control_response", Response: map[string]any{
			"subtype": "success", "request_id": fr.RequestID, "response": map[string]any{},
		}})
		return nil
	}
}

func (m *claudeCodeModel) handleMCP(conn ccConn, requestID string, raw json.RawMessage) *genai.FunctionCall {
	var msg ccJSONRPC
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	reply := func(result any) {
		_ = conn.send(ccControlEnvelope{Type: "control_response", Response: map[string]any{
			"subtype": "success", "request_id": requestID,
			"response": map[string]any{"mcp_response": map[string]any{
				"jsonrpc": "2.0", "id": msg.ID, "result": result,
			}},
		}})
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		reply(ccMCPInitializeResult(p.ProtocolVersion))
	case "tools/list":
		m.mu.Lock()
		tools := m.tools
		m.mu.Unlock()
		reply(map[string]any{"tools": tools})
	case "tools/call":
		var p ccToolsCallParams
		_ = json.Unmarshal(msg.Params, &p)
		id := p.Meta.ToolUseID
		if id == "" {
			id = fmt.Sprintf("cc_%s", requestID)
		}
		m.mu.Lock()
		m.pending[id] = ccPending{requestID: requestID, innerID: msg.ID}
		m.mu.Unlock()
		args := p.Arguments
		if args == nil {
			args = map[string]any{}
		}
		return &genai.FunctionCall{ID: id, Name: p.Name, Args: args}
	default:
		if len(msg.ID) == 0 {
			// notification (no id): ack the carrying control request.
			_ = conn.send(ccControlEnvelope{Type: "control_response", Response: map[string]any{
				"subtype": "success", "request_id": requestID,
				"response": map[string]any{"mcp_response": map[string]any{}},
			}})
		} else {
			_ = conn.send(ccControlEnvelope{Type: "control_response", Response: map[string]any{
				"subtype": "error", "request_id": requestID, "error": "unknown method " + msg.Method,
			}})
		}
	}
	return nil
}

// answerCall delivers a tool result back to the child as the reply to its
// pending tools/call. Caller holds m.mu.
func (m *claudeCodeModel) answerCall(fr *genai.FunctionResponse) {
	p, ok := m.pending[fr.ID]
	if !ok || m.conn == nil {
		return
	}
	delete(m.pending, fr.ID)
	text, isErr := ccToolResultText(fr.Response)
	if text == "" {
		text = "(no output)"
	}
	_ = m.conn.send(ccControlEnvelope{Type: "control_response", Response: map[string]any{
		"subtype": "success", "request_id": p.requestID,
		"response": map[string]any{"mcp_response": map[string]any{
			"jsonrpc": "2.0", "id": p.innerID,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			},
		}},
	}})
}

func (m *claudeCodeModel) interrupt() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interruptLocked()
}

// interruptLocked sends an interrupt and fails every pending call so the child
// unwinds the turn. Caller holds m.mu.
func (m *claudeCodeModel) interruptLocked() {
	if m.conn == nil {
		return
	}
	_ = m.conn.send(ccControlEnvelope{Type: "control_request", RequestID: "int", Request: map[string]any{"subtype": "interrupt"}})
	for id, p := range m.pending {
		_ = m.conn.send(ccControlEnvelope{Type: "control_response", Response: map[string]any{
			"subtype": "success", "request_id": p.requestID,
			"response": map[string]any{"mcp_response": map[string]any{
				"jsonrpc": "2.0", "id": p.innerID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "interrupted"}},
					"isError": true,
				},
			}},
		}})
		delete(m.pending, id)
	}
}

// observeToolUses forwards native tool calls in an assistant frame to the sink
// and records their ids so the matching result can be surfaced. No-op without a
// sink.
func (m *claudeCodeModel) observeToolUses(raw json.RawMessage) {
	if m.sink == nil {
		return
	}
	for _, tu := range ccAssistantToolUses(raw) {
		m.nativeCalls[tu.ID] = tu.Name
		m.sink.ObservedToolCall(tu.ID, tu.Name, tu.Input)
	}
}

// observeToolResults forwards the results of native calls (matched by id) to
// the sink. Results for ids we did not surface (MCP tools, handled by the
// bridge) are ignored. No-op without a sink.
func (m *claudeCodeModel) observeToolResults(raw json.RawMessage) {
	if m.sink == nil {
		return
	}
	for _, tr := range ccUserToolResults(raw) {
		name, ok := m.nativeCalls[tr.ToolUseID]
		if !ok {
			continue
		}
		delete(m.nativeCalls, tr.ToolUseID)
		m.sink.ObservedToolResult(tr.ToolUseID, name, tr.Text, tr.IsError)
	}
}

func (m *claudeCodeModel) exitError() error {
	tail := ""
	if m.conn != nil {
		tail = m.conn.stderrTail()
	}
	if tail != "" {
		return fmt.Errorf("%w: %s", errChildExited, tail)
	}
	return errChildExited
}

// ---- writers (caller holds m.mu) ----

func (m *claudeCodeModel) writeUserContent(c *genai.Content) error {
	if c == nil {
		return nil
	}
	var blocks []ccBlock
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.Text != "":
			blocks = append(blocks, ccBlock{Type: "text", Text: p.Text})
		case p.InlineData != nil && len(p.InlineData.Data) > 0:
			blocks = append(blocks, ccBlock{Type: "image", Source: &ccImageSource{
				Type: "base64", MediaType: p.InlineData.MIMEType,
				Data: base64.StdEncoding.EncodeToString(p.InlineData.Data),
			}})
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return m.conn.send(ccUserFrame{Type: "user", Message: ccUserMessage{Role: "user", Content: blocks}})
}

func (m *claudeCodeModel) writeUserText(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return m.conn.send(ccUserFrame{Type: "user", Message: ccUserMessage{
		Role: "user", Content: []ccBlock{{Type: "text", Text: text}},
	}})
}

// buildResponse assembles an LLMResponse. A final response always carries
// content (text and/or function calls) so ADK does not drop it.
func (m *claudeCodeModel) buildResponse(text, thought string, calls []*genai.FunctionCall, usage *ccUsage, final bool) *model.LLMResponse {
	var parts []*genai.Part
	if thought != "" {
		parts = append(parts, &genai.Part{Thought: true, Text: thought})
	}
	if text != "" {
		parts = append(parts, &genai.Part{Text: text})
	}
	for _, c := range calls {
		parts = append(parts, &genai.Part{FunctionCall: c})
	}
	resp := &model.LLMResponse{
		Content:      &genai.Content{Role: "model", Parts: parts},
		Partial:      !final,
		TurnComplete: final,
	}
	if usage != nil {
		total := usage.InputTokens + usage.OutputTokens
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        int32(usage.InputTokens),
			CandidatesTokenCount:    int32(usage.OutputTokens),
			TotalTokenCount:         int32(total),
			CachedContentTokenCount: int32(usage.CacheReadInputTokens),
			ThoughtsTokenCount:      int32(usage.OutputTokensDetails.ThinkingTokens),
		}
	}
	return resp
}

var _ model.LLM = (*claudeCodeModel)(nil)
