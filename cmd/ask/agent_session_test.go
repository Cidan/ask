package main

import (
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/engine"
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
	if file.Cwd != cwd || len(file.Messages) != 4 {
		t.Fatalf("loaded file wrong: cwd=%q msgs=%d", file.Cwd, len(file.Messages))
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

func TestAgentSessionStore_LoadHistoryModes(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()
	if err := st.save("ses-h", cwd, testTranscript()); err != nil {
		t.Fatal(err)
	}

	full, err := st.loadHistory("ses-h", HistoryOpts{ToolOutput: toolOutputFull})
	if err != nil {
		t.Fatalf("loadHistory: %v", err)
	}
	if len(full) != 4 {
		t.Errorf("full mode entries = %d want 4: %+v", len(full), full)
	}
	if full[0].kind != histUser || !strings.Contains(full[0].text, "fix the bug") {
		t.Errorf("first entry must be the user turn: %+v", full[0])
	}

	off, err := st.loadHistory("ses-h", HistoryOpts{ToolOutput: toolOutputOff})
	if err != nil {
		t.Fatal(err)
	}
	if len(off) != 2 {
		t.Errorf("off mode entries = %d want 2: %+v", len(off), off)
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
	if len(file.Messages) != 2 ||
		file.Messages[0].Role != engine.RoleUser ||
		file.Messages[1].Role != engine.RoleAssistant {
		t.Errorf("materialized transcript wrong: %+v", file.Messages)
	}
	if file.Messages[1].Text != "original answer" {
		t.Errorf("assistant text lost: %q", file.Messages[1].Text)
	}
}
