package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/engine"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func testTranscript() []engine.Message {
	return []engine.Message{
		engine.NewUserMessage("fix the bug in parser.go"),
		engine.NewAssistantMessage("",
			[]engine.ThoughtPart{{Text: "thinking about it"}},
			[]engine.ToolCallPart{{Name: "read", Args: map[string]any{"file_path": "parser.go"}}},
		),
		engine.NewToolResultMessage(
			engine.ToolResultPart{Name: "read", Content: "     1\tpackage parser"},
		),
		engine.NewAssistantMessage("Fixed the off-by-one.", nil, nil),
	}
}

func TestAgentSessionStore_SaveLoadRoundTrip(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()

	if err := st.save("ses-1", cwd, testTranscript()); err != nil {
		t.Fatalf("save: %v", err)
	}
	file, err := st.load("ses-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if file.Cwd != cwd || len(file.Messages()) != 4 {
		t.Fatalf("loaded file wrong: cwd=%q msgs=%d", file.Cwd, len(file.Messages()))
	}

	// CreatedAt survives re-saves.
	created := file.CreatedAt
	if err := st.save("ses-1", cwd, file.Messages()[:2]); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	file2, _ := st.load("ses-1")
	if !file2.CreatedAt.Equal(created) {
		t.Error("CreatedAt must be preserved across saves")
	}
	if len(file2.Messages()) != 2 {
		t.Error("re-save must replace messages")
	}

	if _, err := st.load("nope"); err == nil {
		t.Error("loading unknown id must error")
	}
}

func TestAgentSessionStore_ListNewestFirst(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()
	if err := st.save("older", cwd, []engine.Message{engine.NewUserMessage("first task")}); err != nil {
		t.Fatal(err)
	}
	if err := st.save("newer", cwd, []engine.Message{engine.NewUserMessage("second task")}); err != nil {
		t.Fatal(err)
	}
	entries, err := st.list(cwd)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].preview != "second task" {
		t.Errorf("newest first expected, got %q then %q", entries[0].preview, entries[1].preview)
	}
	if entries[0].cwd != cwd {
		t.Errorf("entry cwd %q want %q", entries[0].cwd, cwd)
	}

	// Other cwd → empty, not error.
	other, err := st.list(t.TempDir())
	if err != nil || len(other) != 0 {
		t.Errorf("foreign cwd should list nothing: %v %v", other, err)
	}
}

func TestAgentSessionStore_LoadTranscriptAndProject(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()
	if err := st.save("ses-h", cwd, testTranscript()); err != nil {
		t.Fatal(err)
	}

	// The loaded transcript is faithful and mode-independent: user turn,
	// tool call, tool result, assistant answer.
	items, err := st.loadTranscript("ses-h")
	if err != nil {
		t.Fatalf("loadTranscript: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("transcript items = %d want 4: %+v", len(items), items)
	}
	if items[0].kind != trUser || !strings.Contains(items[0].text, "fix the bug") {
		t.Errorf("first item must be the user turn: %+v", items[0])
	}
	if items[1].kind != trToolCall || items[1].toolName != "read" {
		t.Errorf("second item must be the read tool call: %+v", items[1])
	}
	if items[2].kind != trToolResult {
		t.Errorf("third item must be the tool result: %+v", items[2])
	}
	if items[3].kind != trAssistant || items[3].text != "Fixed the off-by-one." {
		t.Errorf("fourth item must be the assistant answer: %+v", items[3])
	}

	// Projection applies the view modes over the same transcript.
	full := projectTranscript(items, false, toolOutputFull, true)
	if len(full) != 4 {
		t.Errorf("full projection = %d want 4: %+v", len(full), full)
	}
	off := projectTranscript(items, false, toolOutputOff, true)
	if len(off) != 2 {
		t.Fatalf("off projection = %d want 2 (tools hidden): %+v", len(off), off)
	}
	if off[0].kind != histUser || off[1].kind != histResponse {
		t.Errorf("off projection should be [user, assistant]: %+v", off)
	}
}

func TestAgentSessionStore_LoadHistoryQuietAndInvokeTool(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()

	events := []*session.Event{
		{
			Author: "user",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText("Search for foo", genai.RoleUser),
			},
			Timestamp: time.Now(),
		},
		{
			Author: "ask_coder",
			LLMResponse: adkmodel.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{
						genai.NewPartFromFunctionCall("invoke_tool", map[string]any{
							"tool_name": "linear_list_issues",
							"params": map[string]any{
								"query": "foo",
							},
						}),
						genai.NewPartFromText("Searching linear issues..."),
					},
				},
			},
			Timestamp: time.Now(),
		},
		{
			Author: "user",
			LLMResponse: adkmodel.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleUser,
					Parts: []*genai.Part{
						genai.NewPartFromFunctionResponse("invoke_tool", map[string]any{
							"result":   "Found 1 issue",
							"is_error": false,
						}),
					},
				},
			},
			Timestamp: time.Now(),
		},
		{
			Author: "ask_coder",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText("Here is the issue: FOO-1", genai.RoleModel),
			},
			Timestamp: time.Now(),
		},
	}

	if err := st.saveEvents("ses-quiet", cwd, events); err != nil {
		t.Fatalf("saveEvents failed: %v", err)
	}

	// The faithful transcript unwraps invoke_tool and keeps every block.
	items, err := st.loadTranscript("ses-quiet")
	if err != nil {
		t.Fatalf("loadTranscript failed: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("expected 5 transcript items (user, toolCall, assistant, toolRes, assistant), got %d: %+v", len(items), items)
	}
	if items[1].kind != trToolCall || items[1].toolName != "linear_list_issues" {
		t.Errorf("expected unwrapped linear_list_issues tool call, got %+v", items[1])
	}

	// Full projection shows all five.
	full := projectTranscript(items, false, toolOutputFull, true)
	if len(full) != 5 {
		t.Fatalf("expected 5 full-mode entries, got %d", len(full))
	}
	if !strings.Contains(full[1].text, "linear_list_issues") {
		t.Errorf("expected unwrapped linear_list_issues in tool call block, got %q", full[1].text)
	}

	// Quiet projection keeps BOTH assistant blocks — the interim
	// "Searching…" AND the final answer — and only hides the tool
	// call/result. This is the fix for the interim-block-drop bug that
	// the old quiet replay had (it collapsed the interim block).
	quiet := projectTranscript(items, true, toolOutputFull, true)
	if len(quiet) != 3 {
		t.Fatalf("expected 3 quiet-mode entries (user + two assistant blocks), got %d: %+v", len(quiet), quiet)
	}
	if quiet[0].kind != histUser || quiet[0].text != "Search for foo" {
		t.Errorf("unexpected first entry in quiet mode: %+v", quiet[0])
	}
	if quiet[1].kind != histResponse || quiet[1].text != "Searching linear issues..." {
		t.Errorf("interim assistant block must survive quiet projection: %+v", quiet[1])
	}
	if quiet[2].kind != histResponse || quiet[2].text != "Here is the issue: FOO-1" {
		t.Errorf("unexpected final entry in quiet mode: %+v", quiet[2])
	}
}

func TestAgentSessionStore_EventsAuthorPreserved(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()

	events := []*session.Event{
		{
			Author: "user",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText("hello", genai.RoleUser),
			},
			Timestamp: time.Now(),
		},
		{
			Author: "ask_coder",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText("world", genai.RoleModel),
			},
			Timestamp: time.Now(),
		},
	}

	if err := st.saveEvents("ses-author", cwd, events); err != nil {
		t.Fatalf("saveEvents: %v", err)
	}

	file, err := st.load("ses-author")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(file.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(file.Events))
	}
	if file.Events[0].Author != "user" {
		t.Errorf("expected event[0].Author to be 'user', got %q", file.Events[0].Author)
	}
	if file.Events[1].Author != "ask_coder" {
		t.Errorf("expected event[1].Author to be 'ask_coder', got %q", file.Events[1].Author)
	}
}

func TestAgentSessionStore_Materialize(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	workspace := t.TempDir()
	id, cwd, err := st.materialize(workspace, []NeutralTurn{
		{Role: "user", Text: "original question"},
		{Role: "assistant", Text: "original answer"},
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if cwd != workspace || id == "" {
		t.Fatalf("materialize returned id=%q cwd=%q", id, cwd)
	}
	file, err := st.load(id)
	if err != nil {
		t.Fatalf("load materialized: %v", err)
	}
	matMsgs := file.Messages()
	if len(matMsgs) != 2 ||
		matMsgs[0].Role != engine.RoleUser ||
		matMsgs[1].Role != engine.RoleAssistant {
		t.Errorf("materialized transcript wrong: %+v", matMsgs)
	}
	if matMsgs[1].Text != "original answer" {
		t.Errorf("assistant text lost: %q", matMsgs[1].Text)
	}
}
