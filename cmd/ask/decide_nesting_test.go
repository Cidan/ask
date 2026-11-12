package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/engine"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Live: a call decide runs arrives as a tool event carrying its parent, and
// lands in the chat nested under the decide.
func TestDecideInnerCallsNestLive(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.toolOutputMode = toolOutputFull
	call, _ := EngineEventToTeaMsg(engine.ToolCallEvent{BaseEvent: engine.BaseEvent{TabID: m.id}, ToolUseID: "c1/1", ToolName: "grep", Input: map[string]any{"pattern": "x"}, Parent: "c1"}).(toolCallMsg)
	result, _ := EngineEventToTeaMsg(engine.ToolResultEvent{BaseEvent: engine.BaseEvent{TabID: m.id}, ToolUseID: "c1/1", ToolName: "grep", Output: "a.go", Parent: "c1"}).(toolResultMsg)
	if !call.nested || !result.nested {
		t.Fatalf("a parent marks the call nested: %+v %+v", call, result)
	}
	if own, _ := EngineEventToTeaMsg(engine.ToolCallEvent{ToolName: "read"}).(toolCallMsg); own.nested {
		t.Error("a model's own call is not nested")
	}

	m, _ = runUpdate(t, m, toolCallMsg{name: "decide", input: map[string]any{"description": "triage"}, tabID: m.id})
	m, _ = runUpdate(t, m, call)
	m, _ = runUpdate(t, m, result)
	if n := len(m.transcript); n != 3 || m.transcript[0].nested || !m.transcript[1].nested || !m.transcript[2].nested {
		t.Fatalf("transcript: %+v", m.transcript)
	}
	var nestedCall *historyEntry
	for i := range m.history {
		if m.history[i].kind == histToolCall && m.history[i].toolName == "grep" {
			nestedCall = &m.history[i]
		}
	}
	if nestedCall == nil || !nestedCall.toolNested || nestedCall.toolInflight {
		t.Fatalf("the nested call is projected nested and settled by its result: %+v", nestedCall)
	}
}

// /resume: the inner calls never became events, so they come back from the
// decision record under the decide call that made them.
func TestDecideInnerCallsNestOnReplay(t *testing.T) {
	rec := engine.DecisionRecord{ID: "d1", CallID: "fc-9", Calls: []engine.DecisionCall{
		{Tool: "read", Args: map[string]any{"file_path": "a.go"}, Result: map[string]any{"content": "package a"}},
		{Tool: "write", Args: map[string]any{"file_path": "x"}, Error: "write cannot run inside decide"},
	}}
	raw, _ := json.Marshal(map[string]any{"ask:decision:d1": rec})
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	byCall := decisionsByCall(state)
	if byCall["fc-9"].ID != "d1" {
		t.Fatalf("records are found by the decide call that made them: %+v", byCall)
	}

	now := time.Now()
	events := []*session.Event{
		{Author: "ask_coder", Timestamp: now, LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: "fc-9", Name: "decide", Args: map[string]any{"description": "triage"}}},
		}}}},
		{Author: "user", Timestamp: now, LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{ID: "fc-9", Name: "decide", Response: map[string]any{"answers": "a: yes"}}},
		}}}},
	}
	items, err := loadTranscriptFromEvents(events, nil, byCall)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		kind := map[transcriptKind]string{trToolCall: "call", trToolResult: "result"}[it.kind]
		label := kind + ":" + it.toolName
		if it.nested {
			label = "  " + label
		}
		if it.isError || it.errored {
			label += "!"
		}
		got = append(got, label)
	}
	want := "call:decide|  call:read|  result:read|  call:write!|  result:write!|result:decide"
	if strings.Join(got, "|") != want {
		t.Fatalf("replay order:\n%s\nwant:\n%s", strings.Join(got, "|"), want)
	}
	if items[2].output != "package a" {
		t.Errorf("a replayed inner result shows its output: %q", items[2].output)
	}

	plain, _ := loadTranscriptFromEvents(events, nil, nil)
	if len(plain) != 2 {
		t.Errorf("without records the decide replays alone: %d items", len(plain))
	}
}
