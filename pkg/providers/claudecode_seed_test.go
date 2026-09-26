package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func modelCall(id, name string, args map[string]any) *genai.Content {
	return &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}}}
}

func toolResponse(id, name, content string) *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: id, Name: name, Response: map[string]any{"content": content},
	}}}}
}

func modelTextContent(text string) *genai.Content {
	return &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}}
}

// readSeed parses a seed file back into its lines.
func readSeed(t *testing.T, path string) []ccSeedLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open seed %q: %v", path, err)
	}
	defer f.Close()
	var lines []ccSeedLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var l ccSeedLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("seed line is not JSON: %v: %s", err, sc.Text())
		}
		lines = append(lines, l)
	}
	return lines
}

// dialRecorder swaps ccDial for one that hands out fake conns in order and
// records every launch.
type dialRecorder struct {
	conns    []*fakeConn
	launches []ClaudeCodeStartArgs
	seeds    [][]ccSeedLine
}

func recordDials(t *testing.T, conns ...*fakeConn) *dialRecorder {
	t.Helper()
	r := &dialRecorder{conns: conns}
	prev := ccDial
	ccDial = func(ctx context.Context, args ClaudeCodeStartArgs) (ccConn, error) {
		r.launches = append(r.launches, args)
		var seed []ccSeedLine
		if i := indexOf(args.Argv, "--resume"); i >= 0 {
			seed = readSeed(t, args.Argv[i+1])
		}
		r.seeds = append(r.seeds, seed)
		fc := r.conns[len(r.launches)-1]
		return fc, nil
	}
	t.Cleanup(func() { ccDial = prev })
	return r
}

func userFrames(fc *fakeConn) []string {
	var out []string
	for _, s := range fc.sent {
		if s["type"] == "user" {
			out = append(out, userFrameText(s))
		}
	}
	return out
}

func toolResultsSent(fc *fakeConn) int {
	n := 0
	for _, s := range fc.sent {
		resp, _ := s["response"].(map[string]any)
		inner, _ := resp["response"].(map[string]any)
		mcp, _ := inner["mcp_response"].(map[string]any)
		if _, ok := mcp["result"].(map[string]any); ok {
			n++
		}
	}
	return n
}

// A resumed session starts its child from the whole history as real tool_use
// and tool_result blocks, and sends only the newest message over stdin.
func TestClaudeCodeModel_ResumeSeedsStructuredHistory(t *testing.T) {
	fc := newFakeConn(8)
	rec := recordDials(t, fc)
	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	fc.push(resultFrame("ok"))

	collect(t, m, &model.LLMRequest{
		Config: &genai.GenerateContentConfig{},
		Contents: []*genai.Content{
			userContent("first question"),
			{Role: "model", Parts: []*genai.Part{{Thought: true, Text: "pondering"}, {FunctionCall: &genai.FunctionCall{ID: "T1", Name: "read", Args: map[string]any{"path": "a.go"}}}}},
			toolResponse("T1", "read", "package main"),
			modelTextContent("it is a main package"),
			userContent("second question"),
		},
	})

	launch := rec.launches[0]
	i := indexOf(launch.Argv, "--resume")
	if i < 0 {
		t.Fatalf("a history must seed the child; argv %v", launch.Argv)
	}
	seedPath := launch.Argv[i+1]
	if seedPath != m.seedPath {
		t.Fatalf("argv seed %q != tracked seed %q", seedPath, m.seedPath)
	}
	lines := rec.seeds[0]
	if len(lines) != 4 {
		t.Fatalf("seed has %d lines, want 4 (user, assistant, user, assistant): %+v", len(lines), lines)
	}
	want := []string{"user", "assistant", "user", "assistant"}
	for i, l := range lines {
		if l.Type != want[i] || l.Message.Role != want[i] {
			t.Fatalf("line %d is %s/%s, want %s", i, l.Type, l.Message.Role, want[i])
		}
		if l.SessionID == "" || l.SessionID != lines[0].SessionID {
			t.Fatalf("line %d session id %q does not match the file's", i, l.SessionID)
		}
		if l.Timestamp == "" || l.Cwd != "/repo" {
			t.Fatalf("line %d missing timestamp or cwd: %+v", i, l)
		}
		if i == 0 {
			if l.ParentUUID != nil {
				t.Fatal("the first line must have no parent")
			}
		} else if l.ParentUUID == nil || *l.ParentUUID != lines[i-1].UUID {
			t.Fatalf("line %d does not chain to line %d", i, i-1)
		}
	}
	use := lines[1].Message.Content
	if len(use) != 1 || use[0].Type != "tool_use" || use[0].Name != "mcp__ask__read" {
		t.Fatalf("assistant line should be one tool_use of mcp__ask__read (thought dropped): %+v", use)
	}
	if in, _ := use[0].Input.(map[string]any); in["path"] != "a.go" {
		t.Fatalf("tool_use input = %v", use[0].Input)
	}
	res := lines[2].Message.Content
	if len(res) != 1 || res[0].Type != "tool_result" || res[0].ToolUseID != use[0].ID || res[0].Content != "package main" {
		t.Fatalf("tool_result does not answer the tool_use: %+v vs %+v", res, use)
	}

	if frames := userFrames(fc); len(frames) != 1 || frames[0] != "second question" {
		t.Fatalf("stdin user frames = %q, want only the newest message", frames)
	}
	if !containsEnv(launch.Env, "DISABLE_COMPACT=1") {
		t.Fatal("the child must run with its own compaction switched off")
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(seedPath); !os.IsNotExist(err) {
		t.Fatalf("seed file must be removed on Close; stat err = %v", err)
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// After RebaseHistory mid-turn, the next call kills the child and starts a
// new one from the rewritten history. The pending tool result goes into the
// seed, never over the bridge (the new child has no call to answer), and the
// nudge restarts the turn.
func TestClaudeCodeModel_RebaseMidTurnSeedsToolResultAndNudges(t *testing.T) {
	prev := ccBatchWindow
	ccBatchWindow = 5 * time.Millisecond
	defer func() { ccBatchWindow = prev }()

	first, second := newFakeConn(16), newFakeConn(16)
	rec := recordDials(t, first, second)
	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })

	first.push(mcpRequest("r1", 1, "tools/call", map[string]any{
		"name": "read", "arguments": map[string]any{"path": "a.go"},
		"_meta": map[string]any{"claudecode/toolUseId": "T1"},
	}))
	cfg := &genai.GenerateContentConfig{Tools: readTool()}
	out := collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{userContent("read a.go")}})
	call := lastFunctionCall(out)
	if call == nil {
		t.Fatal("no tool call yielded")
	}

	m.RebaseHistory()
	notice := userContent("[elided]")
	second.push(resultFrame("done"))
	out = collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{
		userContent("read a.go"),
		notice,
		{Role: "model", Parts: []*genai.Part{{FunctionCall: call}}},
		toolResponse(call.ID, "read", "package main"),
	}})
	if lastText(out) != "done" {
		t.Fatalf("rebuilt child's reply = %q", lastText(out))
	}

	if !first.closed {
		t.Fatal("the old child must be torn down on rebase")
	}
	if len(rec.launches) != 2 {
		t.Fatalf("launches = %d, want 2", len(rec.launches))
	}
	if toolResultsSent(second) != 0 || toolResultsSent(first) != 0 {
		t.Fatal("the tool result must ride the seed, not the MCP bridge")
	}
	seed := rec.seeds[1]
	if len(seed) != 3 {
		t.Fatalf("seed has %d lines, want 3 (merged user request+notice, tool_use, tool_result): %+v", len(seed), seed)
	}
	if c := seed[0].Message.Content; len(c) != 2 || c[0].Text != "read a.go" || c[1].Text != "[elided]" {
		t.Fatalf("first seed message should carry the request then the notice: %+v", c)
	}
	last := seed[2].Message.Content
	if seed[2].Type != "user" || len(last) != 1 || last[0].Type != "tool_result" || last[0].Content != "package main" {
		t.Fatalf("seed must end on the tool result: %+v", seed[2])
	}
	if frames := userFrames(second); len(frames) != 1 || frames[0] != ccSeedNudge {
		t.Fatalf("rebuilt child's stdin = %q, want only the nudge", frames)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rebase {
		t.Fatal("rebase flag must clear once the child is rebuilt")
	}
}

// A running child is sent only what follows what it already has, even when
// request hooks change the history around it: the recall hook's part on the
// latest user message, a one-off image content, and a reply ADK drops because
// it was empty.
func TestClaudeCodeModel_DrainSendsOnlyNewContent(t *testing.T) {
	prev := ccBatchWindow
	ccBatchWindow = 5 * time.Millisecond
	defer func() { ccBatchWindow = prev }()

	fc := newFakeConn(32)
	recordDials(t, fc)
	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })
	cfg := &genai.GenerateContentConfig{Tools: readTool()}

	withMemory := func(text string) *genai.Content {
		c := userContent(text)
		c.Parts = append(c.Parts, &genai.Part{Text: "<memory>\nrecalled\n</memory>"})
		return c
	}
	img := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "rendered"}, {InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1, 2, 3}}}}}

	// Turn 1, step 1: the child asks for a tool.
	fc.push(mcpRequest("r1", 1, "tools/call", map[string]any{"name": "read", "arguments": map[string]any{"path": "a"}, "_meta": map[string]any{"claudecode/toolUseId": "T1"}}))
	out := collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{withMemory("q1")}})
	c1 := lastFunctionCall(out)

	// Step 2: the result plus a one-off image the hook appended.
	fc.push(mcpRequest("r2", 2, "tools/call", map[string]any{"name": "read", "arguments": map[string]any{"path": "b"}, "_meta": map[string]any{"claudecode/toolUseId": "T2"}}))
	out = collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{
		withMemory("q1"), {Role: "model", Parts: []*genai.Part{{FunctionCall: c1}}}, toolResponse("T1", "read", "A"), img,
	}})
	c2 := lastFunctionCall(out)

	// Step 3: the image is gone from the rebuilt request.
	fc.push(ccFrame{Type: "result", Subtype: "success"})
	collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{
		withMemory("q1"), {Role: "model", Parts: []*genai.Part{{FunctionCall: c1}}}, toolResponse("T1", "read", "A"),
		{Role: "model", Parts: []*genai.Part{{FunctionCall: c2}}}, toolResponse("T2", "read", "B"),
	}})

	// Turn 2: the empty reply above never reached the history.
	fc.push(resultFrame("ok"))
	collect(t, m, &model.LLMRequest{Config: cfg, Contents: []*genai.Content{
		userContent("q1"), {Role: "model", Parts: []*genai.Part{{FunctionCall: c1}}}, toolResponse("T1", "read", "A"),
		{Role: "model", Parts: []*genai.Part{{FunctionCall: c2}}}, toolResponse("T2", "read", "B"),
		withMemory("q2"),
	}})

	if got := userFrames(fc); strings.Join(got, "|") != "q1|rendered|q2" {
		t.Fatalf("user frames = %q, want q1, rendered, q2 exactly once each", got)
	}
	if n := toolResultsSent(fc); n != 2 {
		t.Fatalf("tool results sent = %d, want 2", n)
	}
}

// A turn Claude ends with no text leaves no model content in the next
// request; the previous user message must not be sent again.
func TestClaudeCodeModel_EmptyReplyDoesNotResendMessage(t *testing.T) {
	fc := newFakeConn(8)
	recordDials(t, fc)
	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })

	fc.push(ccFrame{Type: "result", Subtype: "success"})
	collect(t, m, &model.LLMRequest{Config: &genai.GenerateContentConfig{}, Contents: []*genai.Content{userContent("q1")}})
	fc.push(resultFrame("ok"))
	collect(t, m, &model.LLMRequest{Config: &genai.GenerateContentConfig{}, Contents: []*genai.Content{userContent("q1"), userContent("q2")}})

	if got := userFrames(fc); strings.Join(got, "|") != "q1|q2" {
		t.Fatalf("user frames = %q, want q1 then q2", got)
	}
}

// A request that shares nothing with the running child — a workflow loop's
// next iteration, whose step only sees its own turn — gets a child rebuilt
// from it rather than appended to the old one.
func TestClaudeCodeModel_UnrelatedRequestRebuildsChild(t *testing.T) {
	first, second := newFakeConn(8), newFakeConn(8)
	rec := recordDials(t, first, second)
	m := newClaudeCodeModel("claude", "opus", "/repo", false, nil)
	t.Cleanup(func() { _ = m.Close() })

	first.push(resultFrame("one"))
	collect(t, m, &model.LLMRequest{Config: &genai.GenerateContentConfig{}, Contents: []*genai.Content{userContent("iteration 1")}})
	second.push(resultFrame("two"))
	out := collect(t, m, &model.LLMRequest{Config: &genai.GenerateContentConfig{}, Contents: []*genai.Content{userContent("iteration 2")}})

	if lastText(out) != "two" || len(rec.launches) != 2 || !first.closed {
		t.Fatalf("want a second child for an unrelated request: launches %d, first closed %v, reply %q", len(rec.launches), first.closed, lastText(out))
	}
	if got := userFrames(second); len(got) != 1 || got[0] != "iteration 2" {
		t.Fatalf("second child's stdin = %q", got)
	}
}

func TestCCSeedMessages_PairsCallsAndRepairsAbandonedOnes(t *testing.T) {
	msgs := ccSeedMessages([]*genai.Content{
		userContent("go"),
		// Parallel calls from a provider that sets no ids.
		{Role: "model", Parts: []*genai.Part{
			{Text: "two reads"},
			{FunctionCall: &genai.FunctionCall{Name: "read", Args: map[string]any{"path": "a"}}},
			{FunctionCall: &genai.FunctionCall{Name: "grep", Args: map[string]any{"q": "x"}}},
		}},
		{Role: "user", Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{Name: "grep", Response: map[string]any{"output": "G"}}},
			{FunctionResponse: &genai.FunctionResponse{Name: "read", Response: map[string]any{"error": "denied"}}},
		}},
		// A call the turn abandoned, then a new user message.
		modelCall("", "bash", nil),
		userContent("never mind"),
		// A response with no call in the history (its call was elided).
		toolResponse("gone", "read", "orphan"),
	})

	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5: %+v", len(msgs), msgs)
	}
	calls := msgs[1].Content
	if calls[0].Type != "text" || calls[1].Type != "tool_use" || calls[2].Type != "tool_use" || calls[1].ID == calls[2].ID {
		t.Fatalf("assistant blocks = %+v", calls)
	}
	results := msgs[2].Content
	byID := map[string]ccSeedBlock{}
	for _, r := range results {
		byID[r.ToolUseID] = r
	}
	if r := byID[calls[1].ID]; r.Content != "denied" || !r.IsError {
		t.Fatalf("read's result = %+v, want the error", r)
	}
	if r := byID[calls[2].ID]; r.Content != "G" || r.IsError {
		t.Fatalf("grep's result = %+v", r)
	}
	bash := msgs[3].Content[0]
	if bash.Type != "tool_use" || bash.Name != "mcp__ask__bash" {
		t.Fatalf("abandoned call = %+v", bash)
	}
	if in, ok := bash.Input.(map[string]any); !ok || len(in) != 0 {
		t.Fatalf("a call with no args must still send an input object, got %#v", bash.Input)
	}
	tail := msgs[4].Content
	if len(tail) != 2 || tail[0].Type != "tool_result" || tail[0].ToolUseID != bash.ID || !tail[0].IsError || tail[1].Text != "never mind" {
		t.Fatalf("abandoned call must be answered as interrupted before the next message: %+v", tail)
	}
}

func TestCCSplitSeed(t *testing.T) {
	q := userContent("q")
	if seed, in := ccSplitSeed([]*genai.Content{q}); len(seed) != 0 || in != q {
		t.Fatalf("one message: seed %d, input %v", len(seed), in)
	}
	hist := []*genai.Content{q, modelCall("T", "read", nil), toolResponse("T", "read", "x")}
	if seed, in := ccSplitSeed(hist); len(seed) != 3 || in != nil {
		t.Fatalf("mid-turn: seed %d, input %v", len(seed), in)
	}
	next := userContent("next")
	if seed, in := ccSplitSeed(append(hist, next)); len(seed) != 3 || in != next {
		t.Fatalf("new turn: seed %d, input %v", len(seed), in)
	}
}

func TestCCContentKey_IgnoresAppendedParts(t *testing.T) {
	plain := userContent("q")
	withMemory := userContent("q")
	withMemory.Parts = append(withMemory.Parts, &genai.Part{Text: "<memory>x</memory>"})
	if ccContentKey(plain) != ccContentKey(withMemory) {
		t.Fatal("a hook-appended part changed the content's identity")
	}
	if ccContentKey(plain) == ccContentKey(userContent("other")) {
		t.Fatal("different messages share a key")
	}
	if ccContentKey(plain) == ccContentKey(modelTextContent("q")) {
		t.Fatal("role must be part of the key")
	}

	repeated := []*genai.Content{userContent("go"), modelTextContent("ok"), userContent("go")}
	mark := ccMarkAt(repeated, 2)
	if i, ok := ccLocateMark(repeated, mark); !ok || i != 2 {
		t.Fatalf("repeated message located at %d (%v), want 2", i, ok)
	}
	if _, ok := ccLocateMark(repeated[:2], mark); ok {
		t.Fatal("a mark past the end must not resolve")
	}
}
