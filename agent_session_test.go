package main

import (
	"strings"
	"testing"

	"charm.land/fantasy"
)

func testTranscript() []fantasy.Message {
	return []fantasy.Message{
		fantasy.NewUserMessage("fix the bug in parser.go"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ReasoningPart{Text: "thinking about it"},
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "read", Input: `{"file_path":"parser.go"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: "     1\tpackage parser"}},
		}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "Fixed the off-by-one."},
		}},
	}
}

func TestAgentSessionStore_SaveLoadRoundTrip(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "deepseek"}
	cwd := t.TempDir()

	if err := st.save("ses-1", cwd, testTranscript()); err != nil {
		t.Fatalf("save: %v", err)
	}
	file, err := st.load("ses-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if file.Cwd != cwd || len(file.Messages) != 4 {
		t.Fatalf("loaded file wrong: cwd=%q msgs=%d", file.Cwd, len(file.Messages))
	}
	// Typed parts must survive: reasoning, tool call, tool result.
	var sawReasoning, sawCall, sawResult bool
	for _, m := range file.Messages {
		for _, p := range m.Content {
			if _, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](p); ok {
				sawReasoning = true
			}
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok && tc.Input != "" {
				sawCall = true
			}
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && toolResultText(tr.Output) != "" {
				sawResult = true
			}
		}
	}
	if !sawReasoning || !sawCall || !sawResult {
		t.Errorf("typed parts lost in round-trip: reasoning=%v call=%v result=%v", sawReasoning, sawCall, sawResult)
	}

	// CreatedAt survives re-saves.
	created := file.CreatedAt
	if err := st.save("ses-1", cwd, file.Messages[:2]); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	file2, _ := st.load("ses-1")
	if !file2.CreatedAt.Equal(created) {
		t.Error("CreatedAt must be preserved across saves")
	}
	if len(file2.Messages) != 2 {
		t.Error("re-save must replace messages")
	}

	if _, err := st.load("nope"); err == nil {
		t.Error("loading unknown id must error")
	}
}

func TestAgentSessionStore_ListNewestFirst(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "deepseek"}
	cwd := t.TempDir()
	if err := st.save("older", cwd, []fantasy.Message{fantasy.NewUserMessage("first task")}); err != nil {
		t.Fatal(err)
	}
	if err := st.save("newer", cwd, []fantasy.Message{fantasy.NewUserMessage("second task")}); err != nil {
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

func TestAgentSessionStore_LoadHistoryModes(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "deepseek"}
	cwd := t.TempDir()
	if err := st.save("ses-h", cwd, testTranscript()); err != nil {
		t.Fatal(err)
	}

	full, err := st.loadHistory("ses-h", HistoryOpts{ToolOutput: toolOutputFull})
	if err != nil {
		t.Fatalf("loadHistory: %v", err)
	}
	if len(full) != 4 { // user, tool call block, tool result block, response
		t.Errorf("full mode entries = %d want 4: %+v", len(full), full)
	}
	if full[0].kind != histUser || !strings.Contains(full[0].text, "fix the bug") {
		t.Errorf("first entry must be the user turn: %+v", full[0])
	}
	if full[len(full)-1].kind != histResponse {
		t.Errorf("last entry must be the assistant response: %+v", full[len(full)-1])
	}

	off, err := st.loadHistory("ses-h", HistoryOpts{ToolOutput: toolOutputOff})
	if err != nil {
		t.Fatal(err)
	}
	if len(off) != 2 { // tool blocks suppressed
		t.Errorf("off mode entries = %d want 2: %+v", len(off), off)
	}
}

func TestAgentSessionStore_Materialize(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "deepseek"}
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
	if len(file.Messages) != 2 ||
		file.Messages[0].Role != fantasy.MessageRoleUser ||
		file.Messages[1].Role != fantasy.MessageRoleAssistant {
		t.Errorf("materialized transcript wrong: %+v", file.Messages)
	}
	if messageText(file.Messages[1]) != "original answer" {
		t.Errorf("assistant text lost: %q", messageText(file.Messages[1]))
	}
}
