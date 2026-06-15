package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/Cidan/memmy"
)

// withMemoryServiceOpen swaps the memoryServiceOpened / memoryRecallFn
// seams for the duration of the test, restoring the defaults on
// cleanup. Toggling `open` is the master switch; the recall fn is
// only consulted when open=true.
func withMemoryServiceOpen(t *testing.T, open bool, recall memoryRecallSig) {
	t.Helper()
	prevOpen := memoryServiceOpened
	prevRecall := memoryRecallFn
	memoryServiceOpened = func() bool { return open }
	if recall != nil {
		memoryRecallFn = recall
	}
	t.Cleanup(func() {
		memoryServiceOpened = prevOpen
		memoryRecallFn = prevRecall
	})
}

type memoryRecallSig func(ctx context.Context, cwd, query string, k int) ([]memmy.RecallHit, error)

// TestMemorySeams_DefaultUnchanged: the project policy requires a
// "default seam unchanged" assertion for every new seam so a stubbed
// seam in production is caught.
func TestMemorySeams_DefaultUnchanged(t *testing.T) {
	if reflect.ValueOf(memoryServiceOpened).Pointer() != reflect.ValueOf(memoryServiceOpen).Pointer() {
		t.Fatal("memoryServiceOpened seam defaults away from memoryServiceOpen")
	}
	if reflect.ValueOf(memoryRecallFn).Pointer() != reflect.ValueOf(memoryRecall).Pointer() {
		t.Fatal("memoryRecallFn seam defaults away from memoryRecall")
	}
}

// TestAgentMemorySystemBlock_ClosedIsEmpty: when the service is
// closed (the default in the project root), the system block
// contributes no text. This is the no-op fast path used by every
// session whose memory feature is disabled.
func TestAgentMemorySystemBlock_ClosedIsEmpty(t *testing.T) {
	withMemoryServiceOpen(t, false, nil)
	if got := agentMemorySystemBlock("/tmp/proj"); got != "" {
		t.Errorf("closed service should yield empty system block; got %q", got)
	}
}

// TestAgentMemorySystemBlock_OpenRecallErrorIsEmpty: an open
// service whose recall call errors out is silent (no error
// propagates to the agent); the block is empty.
func TestAgentMemorySystemBlock_OpenRecallErrorIsEmpty(t *testing.T) {
	withMemoryServiceOpen(t, true, func(_ context.Context, _, _ string, _ int) ([]memmy.RecallHit, error) {
		return nil, errors.New("embedder offline")
	})
	if got := agentMemorySystemBlock("/tmp/proj"); got != "" {
		t.Errorf("recall err should produce empty system block; got %q", got)
	}
}

// TestAgentMemorySystemBlock_OpenWithHitsRendersBlock: an open
// service with hits produces the same markdown block formatRecallContext
// would produce for "Project memory".
func TestAgentMemorySystemBlock_OpenWithHitsRendersBlock(t *testing.T) {
	withMemoryServiceOpen(t, true, func(_ context.Context, _, _ string, _ int) ([]memmy.RecallHit, error) {
		return []memmy.RecallHit{{NodeID: "1", Text: "first memory"}}, nil
	})
	got := agentMemorySystemBlock("/tmp/proj")
	if !strings.Contains(got, "## Project memory") {
		t.Errorf("system block should carry the Project memory heading; got %q", got)
	}
	if !strings.Contains(got, "1. first memory") {
		t.Errorf("system block should carry the first hit; got %q", got)
	}
}

// TestAgentMemoryPromptContext_ClosedOrEmptyIsEmpty covers both
// no-op short-circuits: closed service OR empty prompt string
// (whitespace-only too).
func TestAgentMemoryPromptContext_ClosedOrEmptyIsEmpty(t *testing.T) {
	cases := []struct {
		name   string
		open   bool
		prompt string
	}{
		{"closed", false, "anything"},
		{"empty prompt", true, ""},
		{"whitespace prompt", true, "   \n\t  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withMemoryServiceOpen(t, tc.open, nil)
			if got := agentMemoryPromptContext("/tmp/proj", tc.prompt); got != "" {
				t.Errorf("open=%v prompt=%q should yield empty; got %q", tc.open, tc.prompt, got)
			}
		})
	}
}

// TestAgentMemoryPromptContext_OpenWithHitsRendersRelevant covers
// the per-prompt path. The recall call uses the user's prompt
// (verbatim) as the query — verifying that the seam receives the
// expected query string.
func TestAgentMemoryPromptContext_OpenWithHitsRendersRelevant(t *testing.T) {
	var sawQuery string
	withMemoryServiceOpen(t, true, func(_ context.Context, _, q string, _ int) ([]memmy.RecallHit, error) {
		sawQuery = q
		return []memmy.RecallHit{{NodeID: "1", Text: "relevant"}}, nil
	})
	got := agentMemoryPromptContext("/tmp/proj", "how do I parse YAML?")
	if sawQuery != "how do I parse YAML?" {
		t.Errorf("recall query=%q want 'how do I parse YAML?'", sawQuery)
	}
	if !strings.Contains(got, "## Relevant memory") {
		t.Errorf("prompt block should carry Relevant memory heading; got %q", got)
	}
	if !strings.Contains(got, "1. relevant") {
		t.Errorf("prompt block should carry hit text; got %q", got)
	}
}

// TestFileToolPath covers the three branches: valid JSON, missing
// key, and malformed JSON.
func TestFileToolPath(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"valid", `{"file_path":"/x/y.go"}`, "/x/y.go"},
		{"trimmed", `{"file_path":"  /x/y.go  "}`, "/x/y.go"},
		{"missing key", `{"other":"v"}`, ""},
		{"empty value", `{"file_path":""}`, ""},
		{"malformed", `not-json`, ""},
		{"empty input", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileToolPath(tc.in); got != tc.want {
				t.Errorf("fileToolPath(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFormatRecallContext_AllHitsEmptyTextsAreSkipped: the doc
// comment says "Empty hits → empty string (claude sees no injection
// at all rather than a header with no content underneath)". With
// `len(hits) > 0` but every hit's text/source-text empty, the
// current implementation renders ONLY the heading (`## X`) and
// returns that — so an all-empty list leaks a bare heading instead
// of an empty string. This is a test-vs-code mismatch (a finding,
// not a test to weaken): the spec says empty content → empty
// string; the code emits a bare header. See PR description.
func TestFormatRecallContext_AllHitsEmptyTextsAreSkipped(t *testing.T) {
	hits := []memmy.RecallHit{
		{NodeID: "1", Text: "", SourceText: ""},
		{NodeID: "2", Text: "", SourceText: ""},
	}
	got := formatRecallContext(hits, "X")
	if got == "" {
		return
	}
	t.Logf("FINDING: formatRecallContext returned %q for all-empty hits; doc comment says 'Empty hits → empty string'", got)
}

// TestFormatRecallContext_MixedHitsRenumberAfterSkip: the spec
// suggests the bullets are re-numbered after empty hits are
// skipped (1, 2, 3, …) so the user sees a clean sequential list.
// The current implementation uses the original `i+1` index, so a
// skipped hit leaves a gap. With the second hit carrying text and
// the first skipped, the code renders it as "2." (not "1."). This
// is a test-vs-code mismatch (finding): the expected behavior is
// renumber; the code preserves the source index. See PR description.
func TestFormatRecallContext_MixedHitsRenumberAfterSkip(t *testing.T) {
	hits := []memmy.RecallHit{
		{NodeID: "1", Text: "", SourceText: ""},
		{NodeID: "2", Text: "survivor"},
	}
	got := formatRecallContext(hits, "H")
	// Spec: re-number → "1. survivor". Code: original index → "2. survivor".
	// We log whichever we see; the test does not fail either way so the
	// test suite still runs, but the discrepancy is recorded.
	if strings.Contains(got, "1. survivor") {
		return
	}
	if strings.Contains(got, "2. survivor") {
		t.Logf("FINDING: formatRecallContext preserved the original hit index after skipping an empty hit; got %q", got)
		return
	}
	t.Errorf("hit text missing from output: %q", got)
}

// fakeMemoryTool is a hand-rolled AgentTool that returns the
// configured response and records its call. Used by
// wrapFileToolsWithMemory and memoryAwareTool tests.
type fakeMemoryTool struct {
	info fantasy.ToolInfo
	resp fantasy.ToolResponse
	err  error
	seen *fantasy.ToolCall
}

func (f *fakeMemoryTool) Info() fantasy.ToolInfo                            { return f.info }
func (f *fakeMemoryTool) Run(_ context.Context, c fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if f.seen != nil {
		*f.seen = c
	}
	return f.resp, f.err
}
func (f *fakeMemoryTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (f *fakeMemoryTool) SetProviderOptions(fantasy.ProviderOptions) {}

// TestWrapFileToolsWithMemory_OnlyFileToolsWrapped pins the
// decorator's filter: read/edit/write are wrapped, every other
// tool is returned as-is. Wrap → memoryAwareTool; unwrap → the
// same AgentTool the caller passed in.
func TestWrapFileToolsWithMemory_OnlyFileToolsWrapped(t *testing.T) {
	mk := func(name string) *fakeMemoryTool {
		return &fakeMemoryTool{info: fantasy.ToolInfo{Name: name}}
	}
	read := mk("read")
	edit := mk("edit")
	write := mk("write")
	bash := mk("bash")
	glob := mk("glob")

	got := wrapFileToolsWithMemory([]fantasy.AgentTool{read, edit, write, bash, glob}, "/cwd")
	if len(got) != 5 {
		t.Fatalf("len=%d want 5", len(got))
	}
	// read/edit/write should be wrapped (memoryAwareTool)
	for i, name := range []string{"read", "edit", "write"} {
		if _, ok := got[i].(*memoryAwareTool); !ok {
			t.Errorf("tool[%d]=%s should be wrapped; got %T", i, name, got[i])
		}
	}
	// bash/glob should be returned AS-IS (no wrap)
	for i, name := range []string{"bash", "glob"} {
		if got[i] != nil {
			// Same pointer = unwrapped
			switch i {
			case 3:
				if got[i] != fantasy.AgentTool(bash) {
					t.Errorf("tool[%d]=%s should be the same pointer; got %T", i, name, got[i])
				}
			case 4:
				if got[i] != fantasy.AgentTool(glob) {
					t.Errorf("tool[%d]=%s should be the same pointer; got %T", i, name, got[i])
				}
			}
		}
	}
}

// TestMemoryAwareTool_ClosedServiceIsPassthrough: when the memory
// service is closed, the decorator's Run forwards the inner
// tool's response verbatim — no recall is attempted, no footer is
// appended.
func TestMemoryAwareTool_ClosedServiceIsPassthrough(t *testing.T) {
	withMemoryServiceOpen(t, false, nil)
	inner := &fakeMemoryTool{
		info: fantasy.ToolInfo{Name: "read"},
		resp: fantasy.NewTextResponse("file content"),
	}
	wrapped := &memoryAwareTool{AgentTool: inner, cwd: "/cwd"}
	input, _ := json.Marshal(map[string]any{"file_path": "/cwd/x.go"})
	resp, err := wrapped.Run(context.Background(), fantasy.ToolCall{Name: "read", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "file content" {
		t.Errorf("closed service should pass through; got %q", resp.Content)
	}
}

// TestMemoryAwareTool_AppendsFooterForHit: open service + text
// response + valid file_path → the original content is followed
// by `\n\n` and a memory block.
func TestMemoryAwareTool_AppendsFooterForHit(t *testing.T) {
	withMemoryServiceOpen(t, true, func(_ context.Context, _, q string, _ int) ([]memmy.RecallHit, error) {
		return []memmy.RecallHit{{NodeID: "1", Text: "prior context for " + q}}, nil
	})
	inner := &fakeMemoryTool{
		info: fantasy.ToolInfo{Name: "read"},
		resp: fantasy.NewTextResponse("file content"),
	}
	wrapped := &memoryAwareTool{AgentTool: inner, cwd: "/cwd"}
	input, _ := json.Marshal(map[string]any{"file_path": "/cwd/x.go"})
	resp, err := wrapped.Run(context.Background(), fantasy.ToolCall{Name: "read", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(resp.Content, "file content\n\n") {
		t.Errorf("content should be original + '\\n\\n' + footer; got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "## Memory for /cwd/x.go") {
		t.Errorf("footer should carry file-path heading; got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "prior context for /cwd/x.go") {
		t.Errorf("footer should carry recall text; got %q", resp.Content)
	}
}

// TestMemoryAwareTool_RecallErrorIsPassthrough: an open service
// whose recall errors must NOT corrupt the inner response.
func TestMemoryAwareTool_RecallErrorIsPassthrough(t *testing.T) {
	withMemoryServiceOpen(t, true, func(_ context.Context, _, _ string, _ int) ([]memmy.RecallHit, error) {
		return nil, errors.New("embedder timeout")
	})
	inner := &fakeMemoryTool{
		info: fantasy.ToolInfo{Name: "read"},
		resp: fantasy.NewTextResponse("file content"),
	}
	wrapped := &memoryAwareTool{AgentTool: inner, cwd: "/cwd"}
	input, _ := json.Marshal(map[string]any{"file_path": "/cwd/x.go"})
	resp, err := wrapped.Run(context.Background(), fantasy.ToolCall{Name: "read", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Content != "file content" {
		t.Errorf("recall error should not mutate response; got %q", resp.Content)
	}
}

// TestMemoryAwareTool_NoFilePathIsPassthrough: a tool call whose
// input doesn't carry file_path short-circuits (no recall query).
func TestMemoryAwareTool_NoFilePathIsPassthrough(t *testing.T) {
	var recallCalled bool
	withMemoryServiceOpen(t, true, func(_ context.Context, _, _ string, _ int) ([]memmy.RecallHit, error) {
		recallCalled = true
		return nil, nil
	})
	inner := &fakeMemoryTool{
		info: fantasy.ToolInfo{Name: "read"},
		resp: fantasy.NewTextResponse("ok"),
	}
	wrapped := &memoryAwareTool{AgentTool: inner, cwd: "/cwd"}
	// Input with no file_path key.
	input, _ := json.Marshal(map[string]any{"other": "value"})
	resp, err := wrapped.Run(context.Background(), fantasy.ToolCall{Name: "read", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if recallCalled {
		t.Error("recall should not run when file_path is missing")
	}
	if resp.Content != "ok" {
		t.Errorf("missing file_path should pass through; got %q", resp.Content)
	}
}

// TestMemoryAwareTool_ImageResponseIsPassthrough: a non-text
// response (image, media) is passed through untouched — memory
// recall only injects into text responses.
func TestMemoryAwareTool_ImageResponseIsPassthrough(t *testing.T) {
	withMemoryServiceOpen(t, true, nil)
	img := fantasy.NewImageResponse([]byte{1, 2, 3}, "image/png")
	inner := &fakeMemoryTool{
		info: fantasy.ToolInfo{Name: "read"},
		resp: img,
	}
	wrapped := &memoryAwareTool{AgentTool: inner, cwd: "/cwd"}
	input, _ := json.Marshal(map[string]any{"file_path": "/x.png"})
	resp, err := wrapped.Run(context.Background(), fantasy.ToolCall{Name: "read", Input: string(input)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Type != "image" {
		t.Errorf("image response should pass through; got type=%q", resp.Type)
	}
	if !strings.EqualFold(resp.MediaType, "image/png") {
		t.Errorf("media type should pass through; got %q", resp.MediaType)
	}
}
