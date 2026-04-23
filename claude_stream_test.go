package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// runStream replays newline-delimited JSON through readClaudeStream (proc.cmd
// left nil so Wait is skipped). Returns every message emitted in order.
func runStream(t *testing.T, lines ...string) []tea.Msg {
	t.Helper()
	var body bytes.Buffer
	for _, ln := range lines {
		body.WriteString(ln)
		body.WriteByte('\n')
	}
	ch := make(chan tea.Msg, 64)
	proc := &providerProc{}
	readClaudeStream(&body, proc, ch)
	return drainCh(ch)
}

func TestReadClaudeStream_InitEmitsCwd(t *testing.T) {
	msgs := runStream(t, `{"type":"system","subtype":"init","cwd":"/tmp/foo"}`)
	var got *providerCwdMsg
	for _, m := range msgs {
		if c, ok := m.(providerCwdMsg); ok {
			got = &c
		}
	}
	if got == nil {
		t.Fatalf("no providerCwdMsg; got %#v", msgs)
	}
	if got.cwd != "/tmp/foo" {
		t.Errorf("cwd=%q want /tmp/foo", got.cwd)
	}
}

func TestReadClaudeStream_InitWithoutCwdEmitsNothing(t *testing.T) {
	msgs := runStream(t, `{"type":"system","subtype":"init"}`)
	for _, m := range msgs {
		if _, ok := m.(providerCwdMsg); ok {
			t.Errorf("should not emit providerCwdMsg when cwd missing")
		}
	}
}

func TestReadClaudeStream_TaskStartedAndNotification(t *testing.T) {
	msgs := runStream(t,
		`{"type":"system","subtype":"task_started","task_id":"t-1","task_type":"agent"}`,
		`{"type":"system","subtype":"task_notification","task_id":"t-1","status":"completed"}`,
	)
	var started, ended int
	for _, m := range msgs {
		if s, ok := m.(bgTaskStartedMsg); ok && s.taskID == "t-1" {
			started++
		}
		if e, ok := m.(bgTaskEndedMsg); ok && e.taskID == "t-1" {
			ended++
		}
	}
	if started != 1 || ended != 1 {
		t.Errorf("started=%d ended=%d want 1/1; msgs=%#v", started, ended, msgs)
	}
}

func TestReadClaudeStream_TaskNotificationIgnoresUnknownStatus(t *testing.T) {
	msgs := runStream(t, `{"type":"system","subtype":"task_notification","task_id":"t-9","status":"in_progress"}`)
	for _, m := range msgs {
		if _, ok := m.(bgTaskEndedMsg); ok {
			t.Errorf("in_progress status should not emit bgTaskEndedMsg; msgs=%#v", msgs)
		}
	}
}

func TestReadClaudeStream_AssistantThinkingStatus(t *testing.T) {
	ev := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "thinking"},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var saw bool
	for _, m := range msgs {
		if s, ok := m.(streamStatusMsg); ok && s.status == "thinking…" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected streamStatusMsg(thinking…), got %#v", msgs)
	}
}

func TestReadClaudeStream_AssistantTextMsg(t *testing.T) {
	ev := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "hi there"},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var saw *assistantTextMsg
	for _, m := range msgs {
		if a, ok := m.(assistantTextMsg); ok {
			saw = &a
		}
	}
	if saw == nil || saw.text != "hi there" {
		t.Fatalf("want assistantTextMsg(hi there); got %#v", msgs)
	}
}

func TestReadClaudeStream_TodoWriteEmitsTodoUpdatedMsg(t *testing.T) {
	ev := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "TodoWrite",
					"input": map[string]any{
						"todos": []any{
							map[string]any{"content": "do thing", "activeForm": "doing thing", "status": "pending"},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var got *todoUpdatedMsg
	for _, m := range msgs {
		if tu, ok := m.(todoUpdatedMsg); ok {
			got = &tu
		}
	}
	if got == nil {
		t.Fatalf("no todoUpdatedMsg: %#v", msgs)
	}
	if len(got.todos) != 1 || got.todos[0].Content != "do thing" || got.todos[0].Status != "pending" {
		t.Errorf("todo content wrong: %+v", got.todos)
	}
}

func TestReadClaudeStream_StructuredPatchEmitsDiff(t *testing.T) {
	ev := map[string]any{
		"type": "user",
		"tool_use_result": map[string]any{
			"filePath": "/x/y.txt",
			"structuredPatch": []any{
				map[string]any{
					"oldStart": 1, "oldLines": 2, "newStart": 1, "newLines": 3,
					"lines": []any{"-old", "+new1", "+new2", " ctx"},
				},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var got *toolDiffMsg
	for _, m := range msgs {
		if d, ok := m.(toolDiffMsg); ok {
			got = &d
		}
	}
	if got == nil {
		t.Fatalf("no toolDiffMsg: %#v", msgs)
	}
	if got.filePath != "/x/y.txt" {
		t.Errorf("filePath=%q want /x/y.txt", got.filePath)
	}
	if len(got.hunks) != 1 || got.hunks[0].oldStart != 1 || got.hunks[0].newLines != 3 || len(got.hunks[0].lines) != 4 {
		t.Errorf("hunk parse wrong: %+v", got.hunks)
	}
}

func TestReadClaudeStream_StreamEventEndTurnEmitsTurnComplete(t *testing.T) {
	ev := map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var n int
	for _, m := range msgs {
		if _, ok := m.(turnCompleteMsg); ok {
			n++
		}
	}
	if n == 0 {
		t.Fatalf("want at least one turnCompleteMsg: %#v", msgs)
	}
}

func TestReadClaudeStream_ResultEmitsDoneAndComplete(t *testing.T) {
	ev := map[string]any{
		"type":       "result",
		"result":     "done",
		"session_id": "sess-1",
		"is_error":   false,
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var done *providerDoneMsg
	var seenComplete bool
	for _, m := range msgs {
		if d, ok := m.(providerDoneMsg); ok {
			done = &d
		}
		if _, ok := m.(turnCompleteMsg); ok {
			seenComplete = true
		}
	}
	if done == nil {
		t.Fatalf("no providerDoneMsg: %#v", msgs)
	}
	if done.res.SessionID != "sess-1" || done.res.Result != "done" || done.res.IsError {
		t.Errorf("result fields wrong: %+v", done.res)
	}
	if !seenComplete {
		t.Errorf("result should also produce a turnCompleteMsg: %#v", msgs)
	}
}

func TestReadClaudeStream_AlwaysEmitsExitedLast(t *testing.T) {
	msgs := runStream(t, `{"type":"system","subtype":"init","cwd":"/"}`)
	if len(msgs) == 0 {
		t.Fatal("no messages emitted")
	}
	_, ok := msgs[len(msgs)-1].(providerExitedMsg)
	if !ok {
		t.Errorf("final message must be providerExitedMsg, got %T", msgs[len(msgs)-1])
	}
}

func TestReadClaudeStream_SkipsInvalidJSON(t *testing.T) {
	msgs := runStream(t,
		`not-json`,
		`{"type":"system","subtype":"init","cwd":"/x"}`,
	)
	// Should still process the valid event and emit the final exited msg.
	if len(msgs) < 2 {
		t.Errorf("invalid JSON should be skipped, valid event still processed; msgs=%#v", msgs)
	}
}

func TestParseStructuredPatch_EmptyOrNil(t *testing.T) {
	if _, _, ok := parseStructuredPatch(nil); ok {
		t.Error("nil should return ok=false")
	}
	if _, _, ok := parseStructuredPatch(map[string]any{}); ok {
		t.Error("empty map should return ok=false")
	}
	if _, _, ok := parseStructuredPatch(map[string]any{"structuredPatch": []any{}}); ok {
		t.Error("empty structuredPatch should return ok=false")
	}
}

func TestStreamEventEndTurn(t *testing.T) {
	cases := []struct {
		name string
		ev   map[string]any
		want bool
	}{
		{"no event", map[string]any{}, false},
		{"non-message-delta", map[string]any{"event": map[string]any{"type": "message_start"}}, false},
		{"delta not end_turn", map[string]any{"event": map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "max_tokens"}}}, false},
		{"end_turn", map[string]any{"event": map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}}, true},
	}
	for _, c := range cases {
		if got := streamEventEndTurn(c.ev); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestAssistantText_ConcatenatesParts(t *testing.T) {
	ev := map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "one"},
				map[string]any{"type": "tool_use", "name": "X"},
				map[string]any{"type": "text", "text": "two"},
			},
		},
	}
	got := assistantText(ev)
	if got != "one\n\ntwo" {
		t.Errorf("assistantText=%q want 'one\\n\\ntwo'", got)
	}
}

func TestAssistantText_EmptyReturnsEmpty(t *testing.T) {
	if assistantText(map[string]any{}) != "" {
		t.Error("missing message returns empty")
	}
	ev := map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_use"}}}}
	if assistantText(ev) != "" {
		t.Error("no text blocks returns empty")
	}
}

func TestAssistantStatus_ToolUseRoutesThroughFormatter(t *testing.T) {
	ev := map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "/a/b/c.go"}},
			},
		},
	}
	got := assistantStatus(ev)
	if got != "Read: c.go" {
		t.Errorf("assistantStatus tool_use=%q want 'Read: c.go'", got)
	}
}

func TestFormatToolStatus_Cases(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{"Bash description", "Bash", map[string]any{"description": "running tests"}, "Bash: running tests"},
		{"Bash command truncates", "Bash", map[string]any{"command": strings.Repeat("x", 100)}, "Bash: " + strings.Repeat("x", 59) + "…"},
		{"Read with file_path basename", "Read", map[string]any{"file_path": "/a/b/c.txt"}, "Read: c.txt"},
		{"Edit with file_path basename", "Edit", map[string]any{"file_path": "/a/b/c.txt"}, "Edit: c.txt"},
		{"Write with file_path basename", "Write", map[string]any{"file_path": "/a/b/c.txt"}, "Write: c.txt"},
		{"Glob pattern", "Glob", map[string]any{"pattern": "*.go"}, "Glob: *.go"},
		{"Grep pattern", "Grep", map[string]any{"pattern": "TODO"}, "Grep: TODO"},
		{"WebFetch url", "WebFetch", map[string]any{"url": "https://example.com"}, "WebFetch: https://example.com"},
		{"WebSearch query", "WebSearch", map[string]any{"query": "ice cream"}, "WebSearch: ice cream"},
		{"WebSearch falls back to url when query empty", "WebSearch", map[string]any{"url": "https://x.test"}, "WebSearch: https://x.test"},
		{"Task subagent_type", "Task", map[string]any{"subagent_type": "architect"}, "Task: architect"},
		{"TaskOutput literal", "TaskOutput", map[string]any{}, "waiting for background task…"},
		{"Unknown tool", "FooTool", map[string]any{}, "FooTool"},
		{"Bash empty returns bare name", "Bash", map[string]any{}, "Bash"},
	}
	for _, c := range cases {
		if got := formatToolStatus(c.tool, c.input); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestEnrichSlashCommands_PreservesOrder(t *testing.T) {
	got := enrichSlashCommands([]string{"/a", "/b", "/c"})
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0].Name != "/a" || got[1].Name != "/b" || got[2].Name != "/c" {
		t.Errorf("order lost: %+v", got)
	}
}

func TestMapKeys_ReturnsAllKeys(t *testing.T) {
	m := map[string]any{"a": 1, "b": 2}
	ks := mapKeys(m)
	has := map[string]bool{}
	for _, k := range ks {
		has[k] = true
	}
	if len(ks) != 2 || !has["a"] || !has["b"] {
		t.Errorf("mapKeys=%v want [a b]", ks)
	}
}

func TestJsonInt(t *testing.T) {
	if jsonInt(float64(42)) != 42 {
		t.Errorf("jsonInt(42.0) must return 42")
	}
	if jsonInt(nil) != 0 {
		t.Errorf("jsonInt(nil) must return 0")
	}
	if jsonInt("42") != 0 {
		t.Errorf("jsonInt of non-float must return 0 (JSON numbers are float64 after Unmarshal)")
	}
}
