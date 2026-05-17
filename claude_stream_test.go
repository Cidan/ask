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
		`{"type":"system","subtype":"task_started","task_id":"t-1","tool_use_id":"toolu_1","task_type":"agent"}`,
		`{"type":"system","subtype":"task_notification","task_id":"t-1","status":"completed"}`,
	)
	var started, ended int
	for _, m := range msgs {
		if s, ok := m.(bgTaskStartedMsg); ok && s.taskID == "t-1" {
			started++
			if s.toolUseID != "toolu_1" {
				t.Errorf("bgTaskStartedMsg.toolUseID=%q want toolu_1", s.toolUseID)
			}
		}
		if e, ok := m.(bgTaskEndedMsg); ok && e.taskID == "t-1" {
			ended++
		}
	}
	if started != 1 || ended != 1 {
		t.Errorf("started=%d ended=%d want 1/1; msgs=%#v", started, ended, msgs)
	}
}

// task_started for an Agent worker without a tool_use_id (older CLIs)
// still emits a bgTaskStartedMsg with empty toolUseID — the
// SubagentStop reap path degrades to task_id-only matching, which is
// still correct when claude's CLI happens to use task_id as the
// agent_id.
func TestReadClaudeStream_TaskStartedAgentWithoutToolUseID(t *testing.T) {
	msgs := runStream(t, `{"type":"system","subtype":"task_started","task_id":"t-2","task_type":"agent"}`)
	var saw *bgTaskStartedMsg
	for _, m := range msgs {
		if s, ok := m.(bgTaskStartedMsg); ok {
			saw = &s
		}
	}
	if saw == nil {
		t.Fatalf("expected bgTaskStartedMsg, got %#v", msgs)
	}
	if saw.taskID != "t-2" || saw.toolUseID != "" {
		t.Errorf("bgTaskStartedMsg{taskID=%q toolUseID=%q} want {t-2 \"\"}", saw.taskID, saw.toolUseID)
	}
}

// task_started for a local_bash background worker (Bash with
// run_in_background=true) must NOT feed the chip — claude's CLI
// signals are unreliable for this task_type (task_notification
// occasionally drops, SubagentStop never fires) so tracking them
// causes the chip to drift upward and never recover. Bash
// run_in_background remains observable through normal tool output;
// it just doesn't get a chip slot.
func TestReadClaudeStream_TaskStartedLocalBashIsIgnored(t *testing.T) {
	msgs := runStream(t,
		`{"type":"system","subtype":"task_started","task_id":"bxgqt","tool_use_id":"toolu_1","task_type":"local_bash","description":"Worker 1"}`,
		`{"type":"system","subtype":"task_notification","task_id":"bxgqt","status":"completed"}`,
	)
	for _, m := range msgs {
		if _, ok := m.(bgTaskStartedMsg); ok {
			t.Errorf("local_bash task_started must not emit bgTaskStartedMsg; msgs=%#v", msgs)
		}
	}
}

// task_started without a task_type (very old CLIs or non-agent
// streams) is also ignored — the chip is now scoped to
// task_type=agent only. We'd rather under-count than drift upward.
func TestReadClaudeStream_TaskStartedWithoutTaskTypeIsIgnored(t *testing.T) {
	msgs := runStream(t, `{"type":"system","subtype":"task_started","task_id":"t-3"}`)
	for _, m := range msgs {
		if _, ok := m.(bgTaskStartedMsg); ok {
			t.Errorf("task_started without task_type must not emit bgTaskStartedMsg; msgs=%#v", msgs)
		}
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

func TestReadClaudeStream_ToolUseEmitsToolCallMsg(t *testing.T) {
	ev := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "Read",
					"input": map[string]any{"file_path": "/x.go"},
				},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var got *toolCallMsg
	for _, m := range msgs {
		if c, ok := m.(toolCallMsg); ok {
			got = &c
		}
	}
	if got == nil {
		t.Fatalf("no toolCallMsg: %#v", msgs)
	}
	if got.name != "Read" {
		t.Errorf("name=%q want Read", got.name)
	}
	if fp, _ := got.input["file_path"].(string); fp != "/x.go" {
		t.Errorf("input file_path=%q want /x.go; input=%+v", fp, got.input)
	}
}

func TestReadClaudeStream_TodoWriteDoesNotEmitToolCallMsg(t *testing.T) {
	// TodoWrite is routed through todoUpdatedMsg; rendering it as a
	// generic tool call would double-count it.
	ev := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "TodoWrite",
					"input": map[string]any{"todos": []any{}},
				},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	for _, m := range msgs {
		if _, ok := m.(toolCallMsg); ok {
			t.Fatalf("TodoWrite should not emit toolCallMsg; msgs=%#v", msgs)
		}
	}
}

func TestReadClaudeStream_ToolResultEmitsToolResultMsg(t *testing.T) {
	ev := map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":       "tool_result",
					"tool_use_id": "abc",
					"content":    "hello world",
				},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var got *toolResultMsg
	for _, m := range msgs {
		if r, ok := m.(toolResultMsg); ok {
			got = &r
		}
	}
	if got == nil {
		t.Fatalf("no toolResultMsg: %#v", msgs)
	}
	if got.output != "hello world" {
		t.Errorf("output=%q want 'hello world'", got.output)
	}
	if got.isError {
		t.Errorf("non-error result flagged as error")
	}
}

func TestReadClaudeStream_ToolResultWithStructuredPatchIsDiffOnly(t *testing.T) {
	// When a tool_result carries a structuredPatch (Edit/Write output),
	// the diff block owns the render; emitting toolResultMsg too would
	// surface a duplicate "The file has been updated" stub.
	ev := map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "tool_result", "content": "The file has been updated"},
			},
		},
		"tool_use_result": map[string]any{
			"filePath": "/z.txt",
			"structuredPatch": []any{
				map[string]any{"oldStart": 1, "oldLines": 1, "newStart": 1, "newLines": 1, "lines": []any{"-a", "+b"}},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var diffs, results int
	for _, m := range msgs {
		if _, ok := m.(toolDiffMsg); ok {
			diffs++
		}
		if _, ok := m.(toolResultMsg); ok {
			results++
		}
	}
	if diffs != 1 || results != 0 {
		t.Errorf("diffs=%d results=%d want 1/0; msgs=%#v", diffs, results, msgs)
	}
}

func TestReadClaudeStream_BackgroundFlagPropagates(t *testing.T) {
	// A tool_use with run_in_background=true marks the toolCallMsg as
	// background, and the matching tool_result inherits the flag (paired
	// by tool_use_id). update.go then decides whether to render based on
	// the user's tri-state mode — the stream layer just propagates.
	call := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"id":   "tool_bg",
					"name": "Bash",
					"input": map[string]any{
						"command":           "sleep 5",
						"run_in_background": true,
					},
				},
			},
		},
	}
	result := map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "tool_bg",
					"content":     "Command running in background with ID: tool_bg.",
				},
			},
		},
	}
	cb, _ := json.Marshal(call)
	rb, _ := json.Marshal(result)
	msgs := runStream(t, string(cb), string(rb))
	var gotCall *toolCallMsg
	var gotRes *toolResultMsg
	for _, m := range msgs {
		switch v := m.(type) {
		case toolCallMsg:
			gotCall = &v
		case toolResultMsg:
			gotRes = &v
		}
	}
	if gotCall == nil || !gotCall.background || gotCall.id != "tool_bg" {
		t.Fatalf("toolCallMsg missing background marker; got %+v", gotCall)
	}
	if gotRes == nil || !gotRes.background || gotRes.toolUseID != "tool_bg" {
		t.Fatalf("toolResultMsg missing background marker; got %+v", gotRes)
	}
}

func TestReadClaudeStream_NonBackgroundResultLeavesFlagFalse(t *testing.T) {
	// A tool_result whose id was never marked background must come
	// through with background=false so update.go renders it.
	ev := map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "tool_fg",
					"content":     "ok",
				},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var got *toolResultMsg
	for _, m := range msgs {
		if r, ok := m.(toolResultMsg); ok {
			got = &r
		}
	}
	if got == nil {
		t.Fatalf("no toolResultMsg; msgs=%#v", msgs)
	}
	if got.output != "ok" {
		t.Errorf("output=%q want ok", got.output)
	}
	if got.background {
		t.Errorf("foreground result should not be flagged background; got %+v", got)
	}
}

func TestReadClaudeStream_ToolResultListContent(t *testing.T) {
	// Some tools return content as an array of {type:"text",text:...}
	// blocks. We flatten them so the consumer gets one string.
	ev := map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type": "tool_result",
					"content": []any{
						map[string]any{"type": "text", "text": "first"},
						map[string]any{"type": "text", "text": "second"},
					},
					"is_error": true,
				},
			},
		},
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	var got *toolResultMsg
	for _, m := range msgs {
		if r, ok := m.(toolResultMsg); ok {
			got = &r
		}
	}
	if got == nil {
		t.Fatalf("no toolResultMsg: %#v", msgs)
	}
	if got.output != "first\nsecond" {
		t.Errorf("output=%q want 'first\\nsecond'", got.output)
	}
	if !got.isError {
		t.Error("is_error=true should propagate")
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

// stream_event frames carry no turn-boundary signal — that's
// derived solely from `result`. Driving a turnComplete from a
// message_delta would conflate per-API-message ends with the
// loop end and would fire (incorrectly) on subagent stream
// frames too. See the rationale comment in claude.go.
func TestReadClaudeStream_StreamEventDoesNotEmitTurnComplete(t *testing.T) {
	cases := []map[string]any{
		{"type": "stream_event", "event": map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}},
		{"type": "stream_event", "event": map[string]any{"type": "message_stop"}},
		// Subagent (Task) end_turn — same shape, plus parent_tool_use_id.
		{"type": "stream_event", "parent_tool_use_id": "toolu_sub", "event": map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}},
	}
	for _, ev := range cases {
		b, _ := json.Marshal(ev)
		msgs := runStream(t, string(b))
		for _, m := range msgs {
			if _, ok := m.(turnCompleteMsg); ok {
				t.Errorf("stream_event must not produce turnCompleteMsg; ev=%v msgs=%#v", ev, msgs)
			}
		}
	}
}

func TestReadClaudeStream_ResultEmitsDoneAndComplete(t *testing.T) {
	ev := map[string]any{
		"type":        "result",
		"subtype":     "success",
		"result":      "done",
		"session_id":  "sess-1",
		"stop_reason": "end_turn",
		"is_error":    false,
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
	if done.res.Subtype != "success" || done.res.StopReason != "end_turn" {
		t.Errorf("subtype/stop_reason not plumbed: %+v", done.res)
	}
	if !seenComplete {
		t.Errorf("result should also produce a turnCompleteMsg: %#v", msgs)
	}
}

// Error subtypes don't carry the `result` text body — the parser
// must synthesize a readable line so the UI doesn't render
// "error: " with no detail. Covers the four documented error
// subtypes plus the refusal-stop_reason path.
func TestReadClaudeStream_ResultErrorSubtypeSynthesizesMessage(t *testing.T) {
	cases := []struct {
		name string
		ev   map[string]any
		want string
	}{
		{
			name: "max turns",
			ev:   map[string]any{"type": "result", "subtype": "error_max_turns", "session_id": "s", "is_error": true},
			want: "hit max-turns limit",
		},
		{
			name: "max budget",
			ev:   map[string]any{"type": "result", "subtype": "error_max_budget_usd", "session_id": "s", "is_error": true},
			want: "hit max-budget limit",
		},
		{
			name: "execution error with HTTP status",
			ev:   map[string]any{"type": "result", "subtype": "error_during_execution", "session_id": "s", "is_error": true, "api_error_status": float64(529)},
			want: "execution error — HTTP 529",
		},
		{
			name: "refusal stop_reason on success subtype",
			ev:   map[string]any{"type": "result", "subtype": "success", "session_id": "s", "is_error": true, "stop_reason": "refusal"},
			want: "model declined the request",
		},
		{
			name: "errors list joined",
			ev:   map[string]any{"type": "result", "subtype": "error_during_execution", "session_id": "s", "is_error": true, "errors": []any{"upstream 5xx", "retry exhausted"}},
			want: "execution error — upstream 5xx; retry exhausted",
		},
		{
			name: "unknown subtype passthrough",
			ev:   map[string]any{"type": "result", "subtype": "error_brand_new", "session_id": "s", "is_error": true},
			want: "error_brand_new",
		},
	}
	for _, c := range cases {
		b, _ := json.Marshal(c.ev)
		msgs := runStream(t, string(b))
		var done *providerDoneMsg
		for _, m := range msgs {
			if d, ok := m.(providerDoneMsg); ok {
				done = &d
			}
		}
		if done == nil {
			t.Fatalf("%s: no providerDoneMsg: %#v", c.name, msgs)
		}
		if !done.res.IsError {
			t.Errorf("%s: IsError=false on error subtype: %+v", c.name, done.res)
		}
		if done.res.Result != c.want {
			t.Errorf("%s: Result=%q want %q", c.name, done.res.Result, c.want)
		}
	}
}

// An IsError=true result that DID carry a `result` body must keep
// that body verbatim rather than overwriting it with synthesized
// text — the API-supplied message is more specific.
func TestReadClaudeStream_ResultErrorWithBodyKeepsBody(t *testing.T) {
	ev := map[string]any{
		"type":       "result",
		"subtype":    "error_during_execution",
		"session_id": "s",
		"is_error":   true,
		"result":     "downstream auth failed at step 3",
	}
	b, _ := json.Marshal(ev)
	msgs := runStream(t, string(b))
	for _, m := range msgs {
		if d, ok := m.(providerDoneMsg); ok {
			if d.res.Result != "downstream auth failed at step 3" {
				t.Errorf("Result body overwritten: %q", d.res.Result)
			}
		}
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

func TestHumanResultSubtype(t *testing.T) {
	cases := map[string]string{
		"error_max_turns":                     "hit max-turns limit",
		"error_max_budget_usd":                "hit max-budget limit",
		"error_during_execution":              "execution error",
		"error_max_structured_output_retries": "structured-output validation failed",
		"error_some_future_value":             "error_some_future_value",
	}
	for in, want := range cases {
		if got := humanResultSubtype(in); got != want {
			t.Errorf("humanResultSubtype(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHumanStopReason(t *testing.T) {
	cases := map[string]string{
		"refusal":       "model declined the request",
		"max_tokens":    "model hit the output-token limit",
		"pause_turn":    "model paused mid-turn",
		"stop_sequence": "model hit a stop sequence",
		"end_turn":      "end_turn", // passthrough — normal completion never reaches the error path
	}
	for in, want := range cases {
		if got := humanStopReason(in); got != want {
			t.Errorf("humanStopReason(%q)=%q want %q", in, got, want)
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
