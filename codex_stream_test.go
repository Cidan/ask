package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// parseCodexEvent unmarshals a single JSON frame string. Used throughout
// these tests so the notification payloads read like the real wire.
func parseCodexEvent(t *testing.T, raw string) map[string]any {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("parseCodexEvent %q: %v", raw, err)
	}
	return ev
}

func TestCodexEventToMsgs_TurnStartedEmitsThinking(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"turn/started","params":{"threadId":"t","turn":{"id":"r"}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d: %v", len(msgs), msgs)
	}
	st, ok := msgs[0].(streamStatusMsg)
	if !ok || st.status != "thinking…" || st.proc != proc {
		t.Fatalf("want streamStatusMsg{thinking…, proc}, got %T %+v", msgs[0], msgs[0])
	}
}

func TestCodexEventToMsgs_ItemStartedStatusByType(t *testing.T) {
	proc := &providerProc{}
	cases := []struct {
		name    string
		raw     string
		want    string
		wantNil bool
	}{
		{"reasoning", `{"method":"item/started","params":{"item":{"type":"reasoning","id":"r"}}}`, "reasoning…", false},
		{"agentMessage", `{"method":"item/started","params":{"item":{"type":"agentMessage","id":"m","text":""}}}`, "responding…", false},
		{"fileChange", `{"method":"item/started","params":{"item":{"type":"fileChange","id":"f"}}}`, "editing files…", false},
		{"planItem", `{"method":"item/started","params":{"item":{"type":"plan","id":"p"}}}`, "planning…", false},
		{"userMessageSilent", `{"method":"item/started","params":{"item":{"type":"userMessage","id":"u"}}}`, "", true},
		{"unknownItemSilent", `{"method":"item/started","params":{"item":{"type":"whatever","id":"x"}}}`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := parseCodexEvent(t, c.raw)
			msgs := codexEventToMsgs(ev, proc)
			if c.wantNil {
				if len(msgs) != 0 {
					t.Fatalf("want no msgs, got %v", msgs)
				}
				return
			}
			if len(msgs) != 1 {
				t.Fatalf("want 1 msg, got %d", len(msgs))
			}
			st, ok := msgs[0].(streamStatusMsg)
			if !ok || st.status != c.want {
				t.Fatalf("want streamStatusMsg{%q}, got %T %+v", c.want, msgs[0], msgs[0])
			}
		})
	}
}

func TestCodexEventToMsgs_CommandExecutionStatusTruncated(t *testing.T) {
	proc := &providerProc{}
	long := strings.Repeat("x", 200)
	raw := `{"method":"item/started","params":{"item":{"type":"commandExecution","id":"c","command":"` + long + `"}}}`
	ev := parseCodexEvent(t, raw)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	st := msgs[0].(streamStatusMsg)
	if !strings.HasPrefix(st.status, "shell: ") {
		t.Errorf("status should start with 'shell: ', got %q", st.status)
	}
	// truncate(..., 60) clips to 60 runes including the ellipsis; the prefix
	// 'shell: ' adds 7 bytes. Just guard against unbounded growth.
	if len(st.status) > 80 {
		t.Errorf("status should be truncated, got %d chars: %q", len(st.status), st.status)
	}
}

func TestCodexEventToMsgs_McpToolCallStatus(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"item/started","params":{"item":{"type":"mcpToolCall","id":"m","server":"gh","tool":"pr_read","arguments":{}}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	st := msgs[0].(streamStatusMsg)
	if st.status != "mcp: gh/pr_read" {
		t.Errorf("status=%q want 'mcp: gh/pr_read'", st.status)
	}
}

func TestCodexEventToMsgs_ItemCompletedAgentMessageEmitsText(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"item/completed","params":{"threadId":"t","turnId":"r","item":{"type":"agentMessage","id":"m","text":"Hello"}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	got, ok := msgs[0].(assistantTextMsg)
	if !ok || got.text != "Hello" || got.proc != proc {
		t.Fatalf("want assistantTextMsg{Hello}, got %T %+v", msgs[0], msgs[0])
	}
}

func TestCodexEventToMsgs_ItemCompletedEmptyAgentMessageSilent(t *testing.T) {
	// A zero-length final agentMessage happens occasionally — don't render
	// an empty response bubble for it.
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"m","text":""}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 0 {
		t.Fatalf("empty agent text should be suppressed, got %v", msgs)
	}
}

func TestCodexEventToMsgs_ItemCompletedReasoningSilent(t *testing.T) {
	// Reasoning items arrive with empty content/summary arrays — they
	// shouldn't fall through and produce status updates.
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"item/completed","params":{"item":{"type":"reasoning","id":"r"}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 0 {
		t.Fatalf("reasoning completion should be silent, got %v", msgs)
	}
}

func TestCodexEventToMsgs_CommandExecutionCompletedEmitsCallAndResult(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"item/completed","params":{"item":{
		"type":"commandExecution","id":"c","command":"ls -la","cwd":"/tmp",
		"output":"total 0\n","exitCode":0,"status":"completed"
	}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs (call+result), got %d: %v", len(msgs), msgs)
	}
	call, ok := msgs[0].(toolCallMsg)
	if !ok {
		t.Fatalf("msg[0] %T want toolCallMsg", msgs[0])
	}
	if call.name != "shell" {
		t.Errorf("name=%q want shell", call.name)
	}
	if cmd, _ := call.input["command"].(string); cmd != "ls -la" {
		t.Errorf("input command=%q want 'ls -la'", cmd)
	}
	res, ok := msgs[1].(toolResultMsg)
	if !ok {
		t.Fatalf("msg[1] %T want toolResultMsg", msgs[1])
	}
	if !strings.Contains(res.output, "total 0") {
		t.Errorf("output=%q want containing 'total 0'", res.output)
	}
	if res.isError {
		t.Errorf("exitCode=0 should not flag isError")
	}
}

func TestCodexEventToMsgs_CommandExecutionFailedMarksError(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"item/completed","params":{"item":{
		"type":"commandExecution","id":"c","command":"false","output":"","exitCode":1,"status":"failed"
	}}}`)
	msgs := codexEventToMsgs(ev, proc)
	// Expect a call + result where isError=true.
	var sawErrorResult bool
	for _, m := range msgs {
		if r, ok := m.(toolResultMsg); ok && r.isError {
			sawErrorResult = true
		}
	}
	if !sawErrorResult {
		t.Fatalf("non-zero exit or failed status should flag result isError; msgs=%v", msgs)
	}
}

func TestCodexEventToMsgs_McpToolCallCompletedEmitsCallAndResult(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"item/completed","params":{"item":{
		"type":"mcpToolCall","id":"m","server":"gh","tool":"pr_read",
		"arguments":{"number":42},"result":"title: Fix the thing","status":"completed"
	}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs, got %d: %v", len(msgs), msgs)
	}
	call := msgs[0].(toolCallMsg)
	if call.name != "mcp: gh/pr_read" {
		t.Errorf("call.name=%q want 'mcp: gh/pr_read'", call.name)
	}
	if n, _ := call.input["number"].(float64); n != 42 {
		t.Errorf("arguments.number=%v want 42; input=%+v", n, call.input)
	}
	res := msgs[1].(toolResultMsg)
	if !strings.Contains(res.output, "Fix the thing") {
		t.Errorf("output missing result body: %q", res.output)
	}
}

func TestCodexEventToMsgs_TurnCompletedEmitsDoneAndComplete(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"turn/completed","params":{"threadId":"tid-abc","turn":{"id":"r","status":"completed"}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs (done+complete), got %d: %v", len(msgs), msgs)
	}
	done, ok := msgs[0].(providerDoneMsg)
	if !ok {
		t.Fatalf("msg[0] %T want providerDoneMsg", msgs[0])
	}
	if done.res.SessionID != "tid-abc" {
		t.Errorf("SessionID=%q want tid-abc", done.res.SessionID)
	}
	if done.res.IsError {
		t.Errorf("completed turn should not be IsError")
	}
	if _, ok := msgs[1].(turnCompleteMsg); !ok {
		t.Errorf("msg[1] %T want turnCompleteMsg", msgs[1])
	}
}

func TestCodexEventToMsgs_TurnCompletedFailedMarksError(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"turn/completed","params":{"threadId":"t","turn":{"id":"r","status":"failed","error":{"message":"context length exceeded"}}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs, got %d", len(msgs))
	}
	done := msgs[0].(providerDoneMsg)
	if !done.res.IsError {
		t.Errorf("failed turn must set IsError")
	}
	if done.res.Result != "context length exceeded" {
		t.Errorf("want turn.error.message in Result, got %q", done.res.Result)
	}
}

func TestCodexEventToMsgs_ErrorNotificationMarksError(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"error","params":{"threadId":"t","turnId":"r","willRetry":false,"error":{"message":"boom"}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs, got %d", len(msgs))
	}
	done := msgs[0].(providerDoneMsg)
	if !done.res.IsError || done.res.Result != "boom" {
		t.Errorf("error notif should propagate message & IsError; got %+v", done.res)
	}
	if _, ok := msgs[1].(turnCompleteMsg); !ok {
		t.Errorf("msg[1] want turnCompleteMsg, got %T", msgs[1])
	}
}

func TestCodexEventToMsgs_ErrorWithWillRetryKeepsTurnAlive(t *testing.T) {
	// When codex signals it's retrying, ending the turn would flip the UI
	// to idle underneath the retry. Emit a status line and keep going.
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"error","params":{"threadId":"t","turnId":"r","willRetry":true,"error":{"message":"rate limited, retrying"}}}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 1 {
		t.Fatalf("want 1 status msg, got %d: %v", len(msgs), msgs)
	}
	st, ok := msgs[0].(streamStatusMsg)
	if !ok {
		t.Fatalf("want streamStatusMsg, got %T", msgs[0])
	}
	if !strings.HasPrefix(st.status, "retrying:") {
		t.Errorf("status should start with 'retrying:', got %q", st.status)
	}
}

func TestCodexEventToMsgs_ResponsesAndEmptyFramesIgnored(t *testing.T) {
	proc := &providerProc{}
	// JSON-RPC responses (no method) must not emit events — they're caught
	// by the handshake, not the stream.
	for _, raw := range []string{
		`{"id":3,"result":{"turn":{"id":"r"}}}`,
		`{}`,
		`{"method":"thread/started","params":{"thread":{"id":"t"}}}`,   // ignored in MVP
		`{"method":"thread/status/changed","params":{"threadId":"t"}}`, // ignored
		`{"method":"account/rateLimits/updated","params":{}}`,          // ignored
	} {
		ev := parseCodexEvent(t, raw)
		if msgs := codexEventToMsgs(ev, proc); len(msgs) != 0 {
			t.Errorf("frame %q should produce no msgs, got %v", raw, msgs)
		}
	}
}

// TestReadCodexStream_DispatchesAndEmitsExited runs the reader goroutine
// against a hand-crafted JSON stream and confirms (a) the dispatch order,
// (b) providerExitedMsg fires after the scanner drains.
func TestReadCodexStream_DispatchesAndEmitsExited(t *testing.T) {
	proc := &providerProc{} // nil cmd so Wait is skipped
	stream := strings.Join([]string{
		`{"method":"turn/started","params":{"threadId":"t","turn":{"id":"r"}}}`,
		`{"method":"item/started","params":{"item":{"type":"agentMessage","id":"m"}}}`,
		`{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"m","text":"hi"}}}`,
		`{"method":"turn/completed","params":{"threadId":"t","turn":{"id":"r","status":"completed"}}}`,
	}, "\n") + "\n"

	sc := bufio.NewScanner(bytes.NewReader([]byte(stream)))
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	ch := make(chan tea.Msg, 32)

	done := make(chan struct{})
	go func() {
		readCodexStream(sc, proc, ch)
		close(done)
	}()

	var got []tea.Msg
	for msg := range ch {
		got = append(got, msg)
	}
	<-done

	// Expected: thinking, responding, hi, done, turn-complete, exited.
	if len(got) != 6 {
		t.Fatalf("want 6 msgs, got %d: %v", len(got), got)
	}
	wantStatuses := []string{"thinking…", "responding…"}
	for i, w := range wantStatuses {
		st, ok := got[i].(streamStatusMsg)
		if !ok || st.status != w {
			t.Errorf("msg[%d] want streamStatusMsg{%q}, got %T %+v", i, w, got[i], got[i])
		}
	}
	if tm, ok := got[2].(assistantTextMsg); !ok || tm.text != "hi" {
		t.Errorf("msg[2] want assistantTextMsg{hi}, got %T %+v", got[2], got[2])
	}
	if _, ok := got[3].(providerDoneMsg); !ok {
		t.Errorf("msg[3] want providerDoneMsg, got %T", got[3])
	}
	if _, ok := got[4].(turnCompleteMsg); !ok {
		t.Errorf("msg[4] want turnCompleteMsg, got %T", got[4])
	}
	if _, ok := got[5].(providerExitedMsg); !ok {
		t.Errorf("msg[5] want providerExitedMsg, got %T", got[5])
	}
}

// TestReadCodexStream_SkipsMalformedLines proves the scanner keeps going
// past invalid JSON. Codex shouldn't emit garbage, but a stderr-accident
// or partial write shouldn't wedge the reader.
func TestReadCodexStream_SkipsMalformedLines(t *testing.T) {
	proc := &providerProc{}
	stream := "not json\n" +
		`{"method":"turn/started","params":{"threadId":"t","turn":{"id":"r"}}}` + "\n"

	sc := bufio.NewScanner(bytes.NewReader([]byte(stream)))
	ch := make(chan tea.Msg, 8)
	go readCodexStream(sc, proc, ch)

	var statuses []string
	for msg := range ch {
		if st, ok := msg.(streamStatusMsg); ok {
			statuses = append(statuses, st.status)
		}
	}
	if len(statuses) != 1 || statuses[0] != "thinking…" {
		t.Fatalf("want single 'thinking…' status, got %v", statuses)
	}
}
