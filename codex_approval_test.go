package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Close() error { return nil }

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// codexApprovalHarness returns a proc/state pair wired to a locked
// buffer stdin so tests can inspect the JSON-RPC responses approval
// handlers write back without racing the responder goroutine.
func codexApprovalHarness() (*providerProc, *codexState, *lockedBuffer) {
	buf := &lockedBuffer{}
	state := &codexState{
		stdin:  buf,
		tabID:  7,
		nextID: codexTurnStartBaseID,
		done:   make(chan struct{}),
	}
	proc := &providerProc{stdin: buf, payload: state}
	return proc, state, buf
}

func parseCodexFrames(t *testing.T, buf *lockedBuffer) []map[string]any {
	t.Helper()
	if buf.Len() == 0 {
		return nil
	}
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad frame %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestCodexServerRequest_DetectsIDPlusMethod(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"notification", `{"method":"thread/started"}`, false},
		{"response", `{"id":2,"result":{}}`, false},
		{"request", `{"id":42,"method":"execCommandApproval","params":{}}`, true},
		{"empty", `{}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var ev map[string]any
			_ = json.Unmarshal([]byte(c.json), &ev)
			_, _, ok := codexServerRequest(ev)
			if ok != c.want {
				t.Errorf("codexServerRequest(%s) ok=%v want %v", c.json, ok, c.want)
			}
		})
	}
}

func TestCodexApproval_ExecCommandEmitsApprovalRequestMsg(t *testing.T) {
	proc, state, _ := codexApprovalHarness()
	ev := map[string]any{
		"jsonrpc": "2.0",
		"id":      float64(17),
		"method":  "execCommandApproval",
		"params": map[string]any{
			"callId":         "c1",
			"conversationId": "tid",
			"cwd":            "/tmp",
			"command":        []any{"rm", "-rf", "/"},
			"parsedCmd":      []any{},
			"reason":         "delete",
		},
	}
	msgs, handled := handleCodexServerRequest(proc, ev)
	if !handled {
		t.Fatal("expected handled=true for execCommandApproval")
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d: %v", len(msgs), msgs)
	}
	req, ok := msgs[0].(approvalRequestMsg)
	if !ok {
		t.Fatalf("msg[0] %T want approvalRequestMsg", msgs[0])
	}
	if req.tabID != state.tabID {
		t.Errorf("tabID=%d want %d", req.tabID, state.tabID)
	}
	if req.toolName != "Bash" {
		t.Errorf("toolName=%q want Bash", req.toolName)
	}
	if got, _ := req.input["command"].(string); got != "rm -rf /" {
		t.Errorf("input.command=%q want rm -rf /", got)
	}
	if req.reply == nil {
		t.Fatal("reply channel must be set so the responder can unblock")
	}
}

func TestCodexApproval_AllowReplyWritesApproved(t *testing.T) {
	proc, _, buf := codexApprovalHarness()
	ev := map[string]any{
		"jsonrpc": "2.0", "id": float64(1), "method": "execCommandApproval",
		"params": map[string]any{
			"callId": "c", "conversationId": "tid", "cwd": "/tmp",
			"command": []any{"ls"}, "parsedCmd": []any{},
		},
	}
	msgs, _ := handleCodexServerRequest(proc, ev)
	req := msgs[0].(approvalRequestMsg)

	req.reply <- approvalReply{allow: true}

	// Give the responder goroutine a moment to write; collect by waiting
	// for non-empty output with a bounded retry.
	waitForFrame(t, buf)
	frames := parseCodexFrames(t, buf)
	if len(frames) != 1 {
		t.Fatalf("want 1 response frame, got %d", len(frames))
	}
	idf, _ := frames[0]["id"].(float64)
	if idf != 1 {
		t.Errorf("response id=%v want 1", frames[0]["id"])
	}
	result, _ := frames[0]["result"].(map[string]any)
	if result["decision"] != "approved" {
		t.Errorf("decision=%v want approved", result["decision"])
	}
}

func TestCodexApproval_DenyReplyWritesDenied(t *testing.T) {
	proc, _, buf := codexApprovalHarness()
	ev := map[string]any{
		"jsonrpc": "2.0", "id": float64(2), "method": "applyPatchApproval",
		"params": map[string]any{
			"callId": "c", "conversationId": "tid",
			"fileChanges": map[string]any{
				"/a.txt": map[string]any{"type": "add", "content": "hi"},
			},
		},
	}
	msgs, _ := handleCodexServerRequest(proc, ev)
	msgs[0].(approvalRequestMsg).reply <- approvalReply{allow: false}

	waitForFrame(t, buf)
	frames := parseCodexFrames(t, buf)
	result, _ := frames[0]["result"].(map[string]any)
	if result["decision"] != "denied" {
		t.Errorf("decision=%v want denied", result["decision"])
	}
}

func TestCodexApproval_AlwaysReplyWritesApprovedForSession(t *testing.T) {
	proc, _, buf := codexApprovalHarness()
	ev := map[string]any{
		"jsonrpc": "2.0", "id": float64(3), "method": "execCommandApproval",
		"params": map[string]any{
			"callId": "c", "conversationId": "tid", "cwd": "/tmp",
			"command": []any{"ls"}, "parsedCmd": []any{},
		},
	}
	msgs, _ := handleCodexServerRequest(proc, ev)
	rule := permissionRule{ruleContent: "Bash(ls)"}
	msgs[0].(approvalRequestMsg).reply <- approvalReply{allow: true, remember: &rule}

	waitForFrame(t, buf)
	frames := parseCodexFrames(t, buf)
	result, _ := frames[0]["result"].(map[string]any)
	if result["decision"] != "approved_for_session" {
		t.Errorf("decision=%v want approved_for_session", result["decision"])
	}
}

func TestCodexApproval_SkipPermsShortCircuits(t *testing.T) {
	proc, state, buf := codexApprovalHarness()
	state.skipPerms = true
	ev := map[string]any{
		"jsonrpc": "2.0", "id": float64(9), "method": "execCommandApproval",
		"params": map[string]any{
			"callId": "c", "conversationId": "tid", "cwd": "/tmp",
			"command": []any{"ls"}, "parsedCmd": []any{},
		},
	}
	msgs, handled := handleCodexServerRequest(proc, ev)
	if !handled {
		t.Fatal("handled must be true even when short-circuiting")
	}
	if len(msgs) != 0 {
		t.Errorf("skipPerms must NOT emit a modal msg, got %v", msgs)
	}
	frames := parseCodexFrames(t, buf)
	if len(frames) != 1 {
		t.Fatalf("want 1 auto-approve frame, got %d", len(frames))
	}
	result, _ := frames[0]["result"].(map[string]any)
	if result["decision"] != "approved" {
		t.Errorf("skipPerms should auto-approve, got %v", result)
	}
}

func TestCodexApproval_UnknownMethodRepliesWithError(t *testing.T) {
	proc, _, buf := codexApprovalHarness()
	ev := map[string]any{
		"jsonrpc": "2.0", "id": float64(42), "method": "somethingNew",
		"params": map[string]any{},
	}
	msgs, handled := handleCodexServerRequest(proc, ev)
	if handled {
		t.Errorf("handled=true for unknown method leaks into UI; want false")
	}
	if len(msgs) != 0 {
		t.Errorf("unknown method must not produce UI msgs, got %v", msgs)
	}
	frames := parseCodexFrames(t, buf)
	if len(frames) != 1 {
		t.Fatalf("want 1 error frame, got %d", len(frames))
	}
	errObj, _ := frames[0]["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("frame should carry an error, got %v", frames[0])
	}
	if code, _ := errObj["code"].(float64); int(code) != -32601 {
		t.Errorf("error.code=%v want -32601 (Method Not Found)", errObj["code"])
	}
}

func TestCodexApproval_DoneChannelReleasesPendingResponder(t *testing.T) {
	proc, state, buf := codexApprovalHarness()
	ev := map[string]any{
		"jsonrpc": "2.0", "id": float64(5), "method": "execCommandApproval",
		"params": map[string]any{
			"callId": "c", "conversationId": "tid", "cwd": "/tmp",
			"command": []any{"ls"}, "parsedCmd": []any{},
		},
	}
	msgs, _ := handleCodexServerRequest(proc, ev)
	_ = msgs[0].(approvalRequestMsg)

	// Simulate the subprocess exiting: closing state.done must release
	// the responder goroutine without writing anything.
	close(state.done)
	// Give it a beat.
	for i := 0; i < 50; i++ {
		if buf.Len() == 0 {
			// keep polling briefly
			continue
		}
		break
	}
	if buf.Len() != 0 {
		t.Errorf("responder should write nothing after state.done fires, got %q", buf.String())
	}
}

func TestReadCodexStream_RoutesServerRequestsToApprovalHandler(t *testing.T) {
	// End-to-end: a stream that contains an execCommandApproval
	// request should produce an approvalRequestMsg on the channel
	// rather than a stray notification.
	bc := &bufferCloser{Buffer: &bytes.Buffer{}}
	state := &codexState{
		stdin: bc,
		tabID: 1,
		done:  make(chan struct{}),
	}
	proc := &providerProc{payload: state}

	stream := strings.Join([]string{
		`{"method":"turn/started","params":{"threadId":"t","turn":{"id":"r"}}}`,
		`{"jsonrpc":"2.0","id":77,"method":"execCommandApproval","params":{"callId":"c","conversationId":"t","cwd":"/tmp","command":["ls"],"parsedCmd":[]}}`,
	}, "\n") + "\n"

	sc := bufio.NewScanner(bytes.NewReader([]byte(stream)))
	sc.Buffer(make([]byte, 1<<16), 1<<18)
	ch := make(chan tea.Msg, 8)
	doneReader := make(chan struct{})
	go func() {
		readCodexStream(sc, proc, ch)
		close(doneReader)
	}()

	var sawApproval bool
	for msg := range ch {
		if _, ok := msg.(approvalRequestMsg); ok {
			sawApproval = true
			// Let the responder goroutine exit cleanly.
			msg.(approvalRequestMsg).reply <- approvalReply{allow: false}
		}
	}
	<-doneReader
	if !sawApproval {
		t.Error("expected an approvalRequestMsg to flow through readCodexStream")
	}
}

// waitForFrame spins until buf receives a response line. The short
// sleep yields the scheduler so the approval responder goroutine gets
// a chance to run and flush its write; without it the test's tight
// loop can starve the responder on single-core containers.
func waitForFrame(t *testing.T, buf *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(buf.Bytes(), []byte("\n")) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for codex response frame; got %q", buf.String())
}
