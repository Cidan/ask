package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/memory"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func openToolTestMemory(t *testing.T) string {
	t.Helper()
	_ = memory.Close()
	if err := memory.Open(memory.Options{DBPath: filepath.Join(t.TempDir(), "tool.db"), Embedder: memory.NewFakeEmbedder(512)}); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { memory.Close() })
	return filepath.Join(t.TempDir(), "proj")
}

func TestTool_MemoryIndexTool(t *testing.T) {
	cwd := openToolTestMemory(t)
	var approved map[string]any
	approvalHandler := func(ctx context.Context, name string, params map[string]any) string {
		approved = params
		return ""
	}
	tool := MemoryIndexTool(cwd, approvalHandler)
	if tool.Name() != "memory_index" {
		t.Fatalf("expected tool name memory_index, got %s", tool.Name())
	}

	resp, err := RunToolWithJSON(testAgentCtx(), tool, `{"kind":"feedback","title":"","description":"x"}`)
	if err != nil || !resp.IsError {
		t.Fatalf("empty title must error: %+v err=%v", resp, err)
	}
	resp, _ = RunToolWithJSON(testAgentCtx(), tool, `{"kind":"bogus","title":"t","description":"x"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "kind must be one of") {
		t.Fatalf("invalid kind must error: %+v", resp)
	}

	out, err := runTypedTool[MemoryIndexResult](t, tool, memoryIndexParams{Kind: "Feedback", Topic: "Style", Title: "short answers please", Body: "Bullets over prose.", Description: "store"})
	if err != nil || !out.Indexed || out.ID == 0 {
		t.Fatalf("index: %+v err=%v", out, err)
	}
	if approved["title"] != "short answers please" || approved["kind"] != "feedback" {
		t.Fatalf("approval must see the concept: %v", approved)
	}
	c, err := memory.Default().Get(context.Background(), out.ID)
	if err != nil || c.Scope != memory.ScopeFor(cwd) || c.Kind != memory.KindFeedback || c.Topic != "style" || c.Body != "Bullets over prose." {
		t.Fatalf("stored concept: %+v err=%v", c, err)
	}

	global, err := runTypedTool[MemoryIndexResult](t, tool, memoryIndexParams{Kind: "user", Scope: "global", Title: "writes Go daily", Description: "store"})
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := memory.Default().Get(context.Background(), global.ID); g.Scope != memory.ScopeGlobal {
		t.Fatalf("scope global must be honoured: %+v", g)
	}

	denied := MemoryIndexTool(cwd, func(context.Context, string, map[string]any) string { return "denied by user" })
	if _, err := runTypedTool[MemoryIndexResult](t, denied, memoryIndexParams{Kind: "project", Title: "nope", Description: "store"}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denial must surface as the error: %v", err)
	}
}

func TestTool_LoadMemoryTool(t *testing.T) {
	cwd := openToolTestMemory(t)
	ctx := context.Background()
	svc := memory.Default()
	id, _ := svc.Upsert(ctx, memory.Concept{Scope: memory.ScopeFor(cwd), Kind: memory.KindProject, Topic: "deploy", Title: "deploy pipeline requires staging validation", Body: "Staging first, always."})
	_, _ = svc.Upsert(ctx, memory.Concept{Scope: "/other", Kind: memory.KindProject, Title: "deploy pipeline requires staging validation"})

	tool := LoadMemoryTool(cwd)
	if tool.Name() != "load_memory" {
		t.Fatalf("name = %s", tool.Name())
	}
	info := ExtractToolInfo(tool)
	for _, p := range []string{"query", "id", "description"} {
		if _, ok := info.Parameters[p]; !ok {
			t.Errorf("missing parameter %s: %v", p, info.Parameters)
		}
	}
	decl := tool.(interface {
		Declaration() *genai.FunctionDeclaration
	}).Declaration()
	if decl == nil || decl.ParametersJsonSchema == nil || decl.Parameters != nil {
		t.Fatal("load_memory must declare a JSON schema for GenAI compatibility")
	}

	resp, _ := RunToolWithJSON(testAgentCtx(), tool, `{"query":"","description":"x"}`)
	if !resp.IsError {
		t.Error("empty query without id must error")
	}

	out, err := runTypedTool[LoadMemoryResult](t, tool, loadMemoryParams{Query: "deploy pipeline requires staging validation", Description: "x"})
	if err != nil || out.Count != 1 || !strings.Contains(out.Memories, "Staging first") || out.Topic != "" {
		t.Fatalf("query: %+v err=%v", out, err)
	}
	if strings.Contains(out.Memories, "/other") {
		t.Fatal("other projects must not leak")
	}
	byID, err := runTypedTool[LoadMemoryResult](t, tool, loadMemoryParams{ID: id, Description: "x"})
	if err != nil || byID.Count != 1 || byID.Topic != "deploy" || !strings.Contains(byID.Memories, "Staging first, always.") {
		t.Fatalf("by id: %+v err=%v", byID, err)
	}
	if _, err := runTypedTool[LoadMemoryResult](t, tool, loadMemoryParams{ID: 9999, Description: "x"}); err == nil {
		t.Fatal("unknown id must error")
	}
	none, err := runTypedTool[LoadMemoryResult](t, tool, loadMemoryParams{Query: "kitten photography", Description: "x"})
	if err != nil || none.Count != 0 || none.Memories != "" {
		t.Fatalf("no match: %+v err=%v", none, err)
	}
}

func TestTool_ReinforceDemoteForget(t *testing.T) {
	cwd := openToolTestMemory(t)
	ctx := context.Background()
	id, _ := memory.Default().Upsert(ctx, memory.Concept{Scope: memory.ScopeFor(cwd), Kind: memory.KindProject, Title: "a fact"})

	up, err := runTypedTool[MemoryAdjustResult](t, MemoryReinforceTool(), memoryIDParams{ID: id, Description: "x"})
	if err != nil || !up.Applied || up.Weight <= memory.WeightInitial {
		t.Fatalf("reinforce: %+v err=%v", up, err)
	}
	again, err := runTypedTool[MemoryAdjustResult](t, MemoryReinforceTool(), memoryIDParams{ID: id, Description: "x"})
	if err != nil || again.Applied || again.Note == "" {
		t.Fatalf("second reinforce inside the refractory window: %+v err=%v", again, err)
	}
	if _, err := runTypedTool[MemoryAdjustResult](t, MemoryDemoteTool(), memoryIDParams{ID: 0, Description: "x"}); err == nil {
		t.Fatal("id 0 must error")
	}
	if _, err := runTypedTool[MemoryAdjustResult](t, MemoryDemoteTool(), memoryIDParams{ID: 4242, Description: "x"}); err == nil {
		t.Fatal("unknown id must error")
	}

	var approvedName string
	forget := MemoryForgetTool(func(_ context.Context, name string, _ map[string]any) string {
		approvedName = name
		return ""
	})
	out, err := runTypedTool[MemoryForgetResult](t, forget, memoryIDParams{ID: id, Description: "x"})
	if err != nil || !out.Forgotten || approvedName != "memory_forget" {
		t.Fatalf("forget: %+v err=%v approved=%q", out, err, approvedName)
	}
	if _, err := memory.Default().Get(ctx, id); err == nil {
		t.Fatal("concept must be gone")
	}
	denied := MemoryForgetTool(func(context.Context, string, map[string]any) string { return "denied" })
	if _, err := runTypedTool[MemoryForgetResult](t, denied, memoryIDParams{ID: 1, Description: "x"}); err == nil {
		t.Fatal("denied forget must error")
	}

	names := map[string]bool{}
	for _, tl := range MemoryTools(cwd, nil) {
		names[tl.Name()] = true
	}
	for _, want := range []string{"memory_index", "memory_reinforce", "memory_demote", "memory_forget"} {
		if !names[want] {
			t.Errorf("MemoryTools missing %s", want)
		}
	}
}

// hookCtx is an agent.Context with a user message and invocation id.
type hookCtx struct {
	agent.Context
	user *genai.Content
	inv  string
}

func (h hookCtx) UserContent() *genai.Content { return h.user }
func (h hookCtx) InvocationID() string        { return h.inv }

func TestTool_PreloadMemoryHookInjectsIntoUserMessage(t *testing.T) {
	cwd := openToolTestMemory(t)
	ctx := context.Background()
	svc := memory.Default()
	_, _ = svc.Upsert(ctx, memory.Concept{Scope: memory.ScopeFor(cwd), Kind: memory.KindFeedback, Topic: "style", Title: "answers should be short"})
	_, _ = svc.Upsert(ctx, memory.Concept{Scope: memory.ScopeFor(cwd), Kind: memory.KindFeedback, Topic: "style", Title: "answers should be brief"})
	// Plenty of higher-weight concepts so the two above fall outside the
	// session-start block and are not excluded from per-turn recall.
	for i := 0; i < memory.DefaultTopK; i++ {
		id, _ := svc.Upsert(ctx, memory.Concept{Scope: memory.ScopeGlobal, Kind: memory.KindReference, Title: "unrelated dashboard " + string(rune('a'+i))})
		_, _ = svc.Reinforce(ctx, id)
	}

	var gotTopic string
	hook := PreloadMemoryTool(cwd, func() string { return "previous" }, func(topic string) { gotTopic = topic })
	if hook.Name() != "preload_memory" || hook.Description() == "" || hook.IsLongRunning() {
		t.Fatal("hook identity")
	}

	user := genai.NewContentFromText("answers should be short", genai.RoleUser)
	history := genai.NewContentFromText("earlier", genai.RoleUser)
	req := &model.LLMRequest{Contents: []*genai.Content{history, user}}
	actx := hookCtx{Context: testAgentCtx(), user: user, inv: "inv-1"}
	if err := hook.ProcessRequest(actx, req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	injected := req.Contents[1]
	if injected == user {
		t.Fatal("the session's content must not be mutated in place")
	}
	if len(injected.Parts) != 2 || !strings.Contains(injected.Parts[1].Text, "<memory>") || !strings.Contains(injected.Parts[1].Text, "answers should be short") {
		t.Fatalf("memory block not appended to the user message: %+v", injected.Parts)
	}
	if len(user.Parts) != 1 || len(req.Contents[0].Parts) != 1 {
		t.Fatal("only the turn's user message gains a part")
	}
	if req.Config != nil && req.Config.SystemInstruction != nil {
		t.Fatal("the block must not touch the system instruction")
	}
	if gotTopic != "style" {
		t.Fatalf("inferred topic = %q", gotTopic)
	}

	// Later requests in the same invocation reuse the block, even after
	// tool responses are in the history, and it lands on the same message.
	fnResp := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{genai.NewPartFromFunctionResponse("read", map[string]any{"content": "x"})}}
	req2 := &model.LLMRequest{Contents: []*genai.Content{history, user, genai.NewContentFromText("calling", genai.RoleModel), fnResp}}
	if err := hook.ProcessRequest(actx, req2); err != nil {
		t.Fatal(err)
	}
	if len(req2.Contents[1].Parts) != 2 || req2.Contents[1].Parts[1].Text != injected.Parts[1].Text {
		t.Fatal("same invocation must reuse the cached block on the user message")
	}
	if len(req2.Contents[3].Parts) != 1 {
		t.Fatal("function responses must never receive the block")
	}

	// A new invocation recomputes; an image-only prompt injects nothing.
	imageOnly := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{genai.NewPartFromBytes([]byte{1}, "image/png")}}
	req3 := &model.LLMRequest{Contents: []*genai.Content{imageOnly}}
	if err := hook.ProcessRequest(hookCtx{Context: testAgentCtx(), user: imageOnly, inv: "inv-2"}, req3); err != nil || len(req3.Contents[0].Parts) != 1 {
		t.Fatalf("image-only turn: err=%v parts=%d", err, len(req3.Contents[0].Parts))
	}
	// Unrelated prompt: no block at all.
	other := genai.NewContentFromText("kitten photography", genai.RoleUser)
	req4 := &model.LLMRequest{Contents: []*genai.Content{other}}
	_ = hook.ProcessRequest(hookCtx{Context: testAgentCtx(), user: other, inv: "inv-3"}, req4)
	if len(req4.Contents[0].Parts) != 1 || hook.Block() != "" {
		t.Fatal("no hits, no block")
	}

	_ = memory.Close()
	req5 := &model.LLMRequest{Contents: []*genai.Content{user}}
	if err := hook.ProcessRequest(actx, req5); err != nil || len(req5.Contents[0].Parts) != 1 {
		t.Fatal("closed memory must be a no-op")
	}
}

func TestTool_MemoryAwareTool(t *testing.T) {
	cwd := openToolTestMemory(t)
	targetFile := filepath.Join(cwd, "main.go")
	_, _ = memory.Default().Upsert(context.Background(), memory.Concept{Scope: memory.ScopeFor(cwd), Kind: memory.KindProject, Topic: "entrypoint", Title: targetFile})

	fileTools := []Tool{
		NewTypedTool("read", "read file", func(ctx agent.Context, p map[string]any) (map[string]any, error) {
			return map[string]any{"content": "package main\nfunc main() {}"}, nil
		}),
		NewTypedTool("glob", "glob pattern", func(ctx agent.Context, p map[string]any) (map[string]any, error) {
			return map[string]any{"content": "main.go"}, nil
		}),
	}

	wrapped := WrapFileToolsWithMemory(fileTools, cwd)
	if len(wrapped) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(wrapped))
	}
	resp, err := RunToolWithJSON(testAgentCtx(), wrapped[0], `{"file_path":"`+targetFile+`"}`)
	if err != nil {
		t.Fatalf("readTool.Run failed: %v", err)
	}
	if !strings.Contains(resp.Content, "Memory for "+targetFile) || !strings.Contains(resp.Content, "[project · entrypoint] "+targetFile) {
		t.Errorf("expected memory recall in tool output, got:\n%s", resp.Content)
	}
	c, _ := memory.Default().Top(context.Background(), cwd, 1)
	if len(c) != 1 || c[0].AccessCount != 0 {
		t.Errorf("file-touch recall must not bump weights: %+v", c)
	}
	resp, err = RunToolWithJSON(testAgentCtx(), wrapped[1], `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("globTool.Run failed: %v", err)
	}
	if strings.Contains(resp.Content, "Memory for") {
		t.Errorf("did not expect memory block in glob tool output: %s", resp.Content)
	}
}
