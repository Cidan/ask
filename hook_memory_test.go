package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Cidan/memmy"
)

// newTestBridge builds an mcpBridge with an empty cwd that tests can
// drive directly without spinning up a TCP listener. Hook handlers
// only consume the bridge's cwd field; the server / listener are
// unused in these tests.
func newTestBridge(tabID int, cwd string) *mcpBridge {
	b := &mcpBridge{tabID: tabID, cwd: atomic.Pointer[string]{}}
	if cwd != "" {
		b.setCwd(cwd)
	}
	return b
}

func TestBridgeSetCwd_RoundTrips(t *testing.T) {
	b := newTestBridge(1, "")
	if got := b.getCwd(); got != "" {
		t.Errorf("initial cwd should be empty, got %q", got)
	}
	b.setCwd("/tmp/proj")
	if got := b.getCwd(); got != "/tmp/proj" {
		t.Errorf("after setCwd: got %q want /tmp/proj", got)
	}
	b.setCwd("/tmp/other")
	if got := b.getCwd(); got != "/tmp/other" {
		t.Errorf("after second setCwd: got %q want /tmp/other", got)
	}
}

func TestFormatRecallContext_EmptyHits(t *testing.T) {
	if got := formatRecallContext(nil, "Project memory"); got != "" {
		t.Errorf("empty hits should produce empty context, got %q", got)
	}
}

func TestFormatRecallContext_NumbersHits(t *testing.T) {
	hits := []memmy.RecallHit{
		{NodeID: "1", Text: "first observation"},
		{NodeID: "2", Text: "second observation"},
	}
	got := formatRecallContext(hits, "Project memory")
	if !strings.HasPrefix(got, "## Project memory") {
		t.Errorf("expected heading, got %q", got)
	}
	if !strings.Contains(got, "1. first observation") {
		t.Errorf("expected numbered first hit, got %q", got)
	}
	if !strings.Contains(got, "2. second observation") {
		t.Errorf("expected numbered second hit, got %q", got)
	}
}

func TestFormatRecallContext_FallsBackToSourceText(t *testing.T) {
	// When a chunk-level hit's Text is empty, the renderer falls
	// back to the parent SourceText so we always emit something for
	// the user to see.
	hits := []memmy.RecallHit{{NodeID: "1", Text: "", SourceText: "from-source"}}
	got := formatRecallContext(hits, "Memory")
	if !strings.Contains(got, "from-source") {
		t.Errorf("expected source-text fallback, got %q", got)
	}
}

func TestPreToolUseFilePath_ExtractsFromInput(t *testing.T) {
	cases := []struct {
		name string
		ev   preToolUseHookInput
		want string
	}{
		{"read", preToolUseHookInput{ToolName: "Read", ToolInput: map[string]any{"file_path": "/a.go"}}, "/a.go"},
		{"edit", preToolUseHookInput{ToolName: "Edit", ToolInput: map[string]any{"file_path": "/b.go"}}, "/b.go"},
		{"write", preToolUseHookInput{ToolName: "Write", ToolInput: map[string]any{"file_path": "/c.go"}}, "/c.go"},
		{"multiedit", preToolUseHookInput{ToolName: "MultiEdit", ToolInput: map[string]any{"file_path": "/d.go"}}, "/d.go"},
		{"notebook", preToolUseHookInput{ToolName: "NotebookEdit", ToolInput: map[string]any{"notebook_path": "/n.ipynb"}}, "/n.ipynb"},
		{"unknown tool", preToolUseHookInput{ToolName: "Bash", ToolInput: map[string]any{"command": "ls"}}, ""},
		{"missing field", preToolUseHookInput{ToolName: "Read", ToolInput: map[string]any{}}, ""},
		{"nil input", preToolUseHookInput{ToolName: "Read", ToolInput: nil}, ""},
	}
	for _, c := range cases {
		if got := preToolUseFilePath(c.ev); got != c.want {
			t.Errorf("%s: preToolUseFilePath=%q want %q", c.name, got, c.want)
		}
	}
}

// callBridgeHook drives an http.Handler directly with a JSON body and
// returns the parsed hookContextResponse from its reply.
func callBridgeHook(t *testing.T, h http.HandlerFunc, body any) hookContextResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks/x", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hook returned %d, body=%s", rec.Code, rec.Body.String())
	}
	var got hookContextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, rec.Body.String())
	}
	return got
}

func TestHandleHookSessionStart_ReturnsEnvelopeWhenServiceClosed(t *testing.T) {
	// Memory closed → hits are empty → additionalContext is empty,
	// but the JSON envelope must still be well-formed so claude can
	// parse it without error.
	isolateHome(t)
	resetMemoryService(t)

	b := newTestBridge(1, "/tmp/proj")
	got := callBridgeHook(t, b.handleHookSessionStart, sessionStartHookInput{Source: "startup"})
	if got.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName=%q want SessionStart", got.HookSpecificOutput.HookEventName)
	}
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Errorf("expected empty context with closed service, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestHandleHookSessionStart_PopulatesContextWhenHits(t *testing.T) {
	// Open the fake-embedder service, seed a write, then expect
	// SessionStart to surface it.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	cwd := "/tmp/proj"
	if err := memoryWrite(context.Background(), cwd, "the canary observation about session-id resolution"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	b := newTestBridge(1, cwd)
	got := callBridgeHook(t, b.handleHookSessionStart, sessionStartHookInput{Source: "startup"})
	if got.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName=%q", got.HookSpecificOutput.HookEventName)
	}
	if got.HookSpecificOutput.AdditionalContext == "" {
		t.Errorf("expected non-empty context with seeded memory")
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "session-id resolution") {
		t.Errorf("expected canary text in context, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestHandleHookUserPromptSubmit_RecallsAgainstPrompt(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	cwd := "/tmp/proj"
	if err := memoryWrite(context.Background(), cwd, "earlier note about embedders"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	b := newTestBridge(1, cwd)
	got := callBridgeHook(t, b.handleHookUserPromptSubmit, userPromptSubmitHookInput{Prompt: "earlier note about embedders"})
	if got.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName=%q", got.HookSpecificOutput.HookEventName)
	}
	if got.HookSpecificOutput.AdditionalContext == "" {
		t.Errorf("expected non-empty context for matching prompt")
	}
}

func TestHandleHookUserPromptSubmit_EmptyPromptIsNoop(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	b := newTestBridge(1, "/tmp/proj")
	got := callBridgeHook(t, b.handleHookUserPromptSubmit, userPromptSubmitHookInput{Prompt: "  "})
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Errorf("empty prompt should produce empty context, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestHandleHookPreToolUse_ExtractsFilePathAndQueries(t *testing.T) {
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	cwd := "/tmp/proj"
	if err := memoryWrite(context.Background(), cwd, "previous edit on /tmp/proj/auth.go to fix login"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	b := newTestBridge(1, cwd)
	got := callBridgeHook(t, b.handleHookPreToolUse, preToolUseHookInput{
		ToolName:  "Edit",
		ToolInput: map[string]any{"file_path": "/tmp/proj/auth.go"},
	})
	if got.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName=%q", got.HookSpecificOutput.HookEventName)
	}
	// The fake embedder is deterministic but semantically blind;
	// recall returns hits regardless of similarity quality, so we
	// only assert the envelope shape and that something came back.
	if got.HookSpecificOutput.AdditionalContext == "" {
		t.Errorf("expected non-empty context for file with seeded memory")
	}
}

func TestHandleHookPreToolUse_NoFilePathReturnsEmpty(t *testing.T) {
	// PreToolUse fires for many tools including ones that don't take
	// a file (Bash, Glob, etc.); for those we return an empty
	// envelope rather than treating the tool name itself as a query.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	b := newTestBridge(1, "/tmp/proj")
	got := callBridgeHook(t, b.handleHookPreToolUse, preToolUseHookInput{
		ToolName:  "Bash",
		ToolInput: map[string]any{"command": "ls"},
	})
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Errorf("non-file tool should produce empty context, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestHandleHookPreToolUse_EmptyCwdReturnsEmpty(t *testing.T) {
	// Bridge without a synced cwd → no tenant → memoryRecall returns
	// nil hits → handler emits an empty envelope rather than
	// wandering across whatever default tenant memmy has cached.
	isolateHome(t)
	resetMemoryService(t)
	openFakeMemoryService(t)

	b := newTestBridge(1, "")
	got := callBridgeHook(t, b.handleHookPreToolUse, preToolUseHookInput{
		ToolName:  "Read",
		ToolInput: map[string]any{"file_path": "/x.go"},
	})
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Errorf("empty cwd should produce empty context, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}
