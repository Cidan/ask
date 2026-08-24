package main

import (
	"testing"

	"github.com/Cidan/ask/pkg/engine"
)

// A ToolDiffEvent carries the unified diff text; the adapter must parse
// it into hunks so renderDiffBlock has content to draw. Regression: the
// adapter used to drop ev.Diff, leaving every edit/write rendered as a
// lone file path with no diff body.
func TestEngineEventToTeaMsg_ToolDiffParsesHunks(t *testing.T) {
	ev := engine.ToolDiffEvent{
		BaseEvent: engine.BaseEvent{TabID: 3},
		Path:      "/foo/bar.go",
		Diff:      "@@ -1,1 +1,1 @@\n-old line\n+new line\n",
	}
	msg, ok := EngineEventToTeaMsg(ev).(toolDiffMsg)
	if !ok {
		t.Fatalf("expected toolDiffMsg, got %T", EngineEventToTeaMsg(ev))
	}
	if msg.filePath != "/foo/bar.go" || msg.tabID != 3 {
		t.Errorf("path/tab lost: %+v", msg)
	}
	if len(msg.hunks) != 1 {
		t.Fatalf("expected 1 parsed hunk, got %d: %+v", len(msg.hunks), msg.hunks)
	}
	h := msg.hunks[0]
	if len(h.Lines) < 2 || h.Lines[0] != "-old line" || h.Lines[1] != "+new line" {
		t.Errorf("hunk lines wrong: %+v", h.Lines)
	}
}

func TestEngineEventToTeaMsg_ToolCallAndResult(t *testing.T) {
	call, ok := EngineEventToTeaMsg(engine.ToolCallEvent{
		BaseEvent: engine.BaseEvent{TabID: 1},
		ToolUseID: "tu-1",
		ToolName:  "read",
		Input:     map[string]any{"file_path": "/x.go"},
	}).(toolCallMsg)
	if !ok {
		t.Fatal("expected toolCallMsg")
	}
	if call.name != "read" || call.id != "tu-1" || call.input["file_path"] != "/x.go" {
		t.Errorf("tool call mapped wrong: %+v", call)
	}

	res, ok := EngineEventToTeaMsg(engine.ToolResultEvent{
		BaseEvent: engine.BaseEvent{TabID: 1},
		ToolName:  "read",
		Output:    "data",
		IsError:   true,
	}).(toolResultMsg)
	if !ok {
		t.Fatal("expected toolResultMsg")
	}
	if res.name != "read" || res.output != "data" || !res.isError {
		t.Errorf("tool result mapped wrong: %+v", res)
	}
}
