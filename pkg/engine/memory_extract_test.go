package engine

import (
	"context"
	"iter"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func openTestMemory(t *testing.T) {
	t.Helper()
	_ = memory.Close()
	if err := memory.Open(memory.Options{
		DBPath:   filepath.Join(t.TempDir(), "memory.db"),
		Embedder: memory.NewFakeEmbedder(512),
	}); err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() {
		SetMemoryExtractor(nil)
		_ = memory.Close()
	})
}

// scriptExtractionModel routes ModelBuilder to a model that replies with
// reply and records every request it saw.
func scriptExtractionModel(t *testing.T, reply string) (*[]*model.LLMRequest, *sync.Mutex) {
	t.Helper()
	prev := ModelBuilder
	t.Cleanup(func() { ModelBuilder = prev })
	var mu sync.Mutex
	var seen []*model.LLMRequest
	ModelBuilder = func(_ context.Context, p providers.Provider, _ config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{name: modelID, generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			mu.Lock()
			seen = append(seen, req)
			mu.Unlock()
			resp := textResponse(reply)
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 120, CandidatesTokenCount: 30}
			return mockLLMSequence(resp)
		}}, nil
	}
	return &seen, &mu
}

func TestMemoryExtractInstructionStaysTiny(t *testing.T) {
	if n := len(memoryExtractInstruction); n > 1200 {
		t.Fatalf("extraction instruction is %d bytes; it runs after every turn and must stay tiny", n)
	}
	long := strings.Repeat("word ", 3000)
	content := MemoryExtractContent(MemoryTurn{Prompt: long, Response: long, Topic: "memory", Files: []string{"a.go"}},
		[]memory.Concept{{ID: 7, Title: "seven"}}, []string{"memory", "deploy"})
	if n := len([]rune(content)); n > memoryExtractPromptCap+memoryExtractResponseCap+200 {
		t.Fatalf("content is %d runes; prompt and response must be clipped", n)
	}
	for _, want := range []string{"Topics: current: memory, memory, deploy", "Existing:\n#7 seven", "Files: a.go", "User:\n", "Assistant:\n…"} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q:\n%s", want, content)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(content), "word") || !strings.Contains(content, "…\nAssistant:\n…") {
		t.Errorf("prompt keeps its head, response keeps its tail:\n%s", clipHead(content, 300))
	}
	if MemoryExtractContent(MemoryTurn{Prompt: "p", Response: "r"}, nil, nil) != "User:\np\nAssistant:\nr" {
		t.Error("bare turn must render only the two sections")
	}
}

func TestParseMemoryExtraction(t *testing.T) {
	got, err := ParseMemoryExtraction("Sure! ```json\n{\"topic\":\"Memory Design\",\"concepts\":[{\"op\":\"new\",\"kind\":\"feedback\",\"scope\":\"global\",\"title\":\"t\",\"body\":\"b\"}]}\n```")
	if err != nil || got.Topic != "Memory Design" || len(got.Concepts) != 1 || got.Concepts[0].Scope != "global" {
		t.Fatalf("parse: %+v err=%v", got, err)
	}
	if _, err := ParseMemoryExtraction("nothing to see"); err == nil {
		t.Fatal("no object must error")
	}
	if _, err := ParseMemoryExtraction("{not json}"); err == nil {
		t.Fatal("bad json must error")
	}
}

func TestMemoryExtractModel(t *testing.T) {
	prev := providers.ModelMetaLookup
	t.Cleanup(func() { providers.ModelMetaLookup = prev })
	providers.ModelMetaLookup = func(providerID, modelID string) (providers.ModelMeta, bool) {
		if providerID == providers.VertexProviderID && modelID == "gemini-3-pro-preview" {
			return providers.ModelMeta{Pricing: &providers.ModelPricing{InputPer1M: 0.1, OutputPer1M: 0.1}}, true
		}
		return providers.ModelMeta{}, false
	}

	p, m := MemoryExtractModel(config.Config{}, providers.VertexProviderID)
	if p != providers.VertexProviderID || m != "gemini-3-pro-preview" {
		t.Fatalf("session provider + cheapest: %s/%s", p, m)
	}
	p, m = MemoryExtractModel(config.Config{Memory: config.MemoryConfig{Provider: providers.OpenRouterProviderID, Model: "openai/gpt-4o-mini"}}, providers.VertexProviderID)
	if p != providers.OpenRouterProviderID || m != "openai/gpt-4o-mini" {
		t.Fatalf("configured memory block wins: %s/%s", p, m)
	}
	p, m = MemoryExtractModel(config.Config{Provider: providers.VertexProviderID, Memory: config.MemoryConfig{Model: "gemini-3.7-flash"}}, "")
	if p != providers.VertexProviderID || m != "gemini-3.7-flash" {
		t.Fatalf("model-only memory block applies to the session provider: %s/%s", p, m)
	}
	if p, m = MemoryExtractModel(config.Config{}, "nosuch"); p != "nosuch" || m != "" {
		t.Fatalf("unknown provider: %s/%s", p, m)
	}
}

func TestMemoryExtractorFilesConceptsAndTopic(t *testing.T) {
	openTestMemory(t)
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "proj")
	svc := memory.Default()
	existing, err := svc.Upsert(ctx, memory.Concept{Scope: memory.ScopeFor(cwd), Kind: memory.KindFeedback, Topic: "style", Title: "short answers"})
	if err != nil {
		t.Fatal(err)
	}
	reply := `{"topic":"Style","concepts":[
		{"op":"update","id":` + itoa(existing) + `,"kind":"feedback","scope":"project","title":"short answers please","body":"Bullets, one idea per line."},
		{"op":"new","kind":"user","scope":"global","title":"Prefers Go","body":"The user writes Go daily."},
		{"op":"update","id":424242,"kind":"project","scope":"project","title":"unknown id becomes new","body":"body"},
		{"op":"new","kind":"project","scope":"project","title":"","body":""}
	]}`
	seen, mu := scriptExtractionModel(t, reply)

	ex := NewMemoryExtractor(MemoryExtractorOptions{LoadConfig: func() (config.Config, error) {
		return config.Config{Provider: providers.VertexProviderID}, nil
	}})
	t.Cleanup(ex.Close)

	var usageMu sync.Mutex
	var usageProvider, usageModel, gotTopic string
	var in, out int
	ok := ex.EnqueueTurn(MemoryTurn{
		Cwd:      cwd,
		Prompt:   "short answers please",
		Response: "short answers",
		Topic:    "previous",
		Files:    []string{"a.go"},
		Provider: providers.VertexProviderID,
		OnUsage: func(p, m string, i, o int) {
			usageMu.Lock()
			defer usageMu.Unlock()
			usageProvider, usageModel, in, out = p, m, i, o
		},
		OnTopic: func(topic string) {
			usageMu.Lock()
			defer usageMu.Unlock()
			gotTopic = topic
		},
	})
	if !ok {
		t.Fatal("EnqueueTurn refused a valid turn")
	}
	drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ex.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	mu.Lock()
	if len(*seen) != 1 {
		t.Fatalf("expected one extraction call, got %d", len(*seen))
	}
	req := (*seen)[0]
	mu.Unlock()
	if req.Config == nil || req.Config.SystemInstruction == nil || req.Config.SystemInstruction.Parts[0].Text != memoryExtractInstruction {
		t.Fatal("extraction call must carry the fixed instruction")
	}
	content := req.Contents[0].Parts[0].Text
	for _, want := range []string{"Topics: current: previous, style", "#" + itoa(existing) + " short answers", "Files: a.go", "User:\nshort answers please"} {
		if !strings.Contains(content, want) {
			t.Errorf("request content missing %q:\n%s", want, content)
		}
	}

	usageMu.Lock()
	defer usageMu.Unlock()
	if usageProvider != providers.VertexProviderID || usageModel == "" || in != 120 || out != 30 {
		t.Errorf("OnUsage = %s/%s %d/%d", usageProvider, usageModel, in, out)
	}
	if gotTopic != "style" {
		t.Errorf("OnTopic = %q", gotTopic)
	}

	updated, err := svc.Get(ctx, existing)
	if err != nil || updated.Title != "short answers please" || updated.Body != "Bullets, one idea per line." || updated.Topic != "style" {
		t.Fatalf("existing concept not updated in place: %+v err=%v", updated, err)
	}
	top, _ := svc.Top(ctx, cwd, 10)
	titles := map[string]memory.Concept{}
	for _, c := range top {
		titles[c.Title] = c
	}
	if len(top) != 3 {
		t.Fatalf("expected 3 concepts (update, new global, unknown-id-as-new), got %d: %+v", len(top), top)
	}
	if g := titles["Prefers Go"]; g.Scope != memory.ScopeGlobal || g.Kind != memory.KindUser {
		t.Errorf("global concept: %+v", g)
	}
	if n := titles["unknown id becomes new"]; n.ID == 424242 || n.Scope != memory.ScopeFor(cwd) {
		t.Errorf("unknown update id must insert under the project scope: %+v", n)
	}
	if names := svc.TopicNames(ctx, cwd, 10); len(names) == 0 || names[0] != "style" {
		t.Errorf("topic must be touched: %v", names)
	}
}

func TestMemoryExtractorDropsOldestWhenFull(t *testing.T) {
	openTestMemory(t)
	prev := ModelBuilder
	t.Cleanup(func() { ModelBuilder = prev })
	release := make(chan struct{})
	var mu sync.Mutex
	var prompts []string
	ModelBuilder = func(_ context.Context, p providers.Provider, _ config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{name: modelID, generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			select {
			case <-release:
			case <-ctx.Done():
				return mockLLMSequence()
			}
			mu.Lock()
			prompts = append(prompts, req.Contents[0].Parts[0].Text)
			mu.Unlock()
			return mockLLMSequence(textResponse(`{"topic":"","concepts":[]}`))
		}}, nil
	}
	ex := NewMemoryExtractor(MemoryExtractorOptions{QueueSize: 1, LoadConfig: func() (config.Config, error) {
		return config.Config{Provider: providers.VertexProviderID}, nil
	}})
	t.Cleanup(ex.Close)

	for _, p := range []string{"first", "second", "third"} {
		if !ex.EnqueueTurn(MemoryTurn{Cwd: t.TempDir(), Prompt: p, Response: "r", Provider: providers.VertexProviderID}) {
			t.Fatalf("enqueue %s refused", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ex.Dropped() != 1 {
		t.Fatalf("dropped = %d, want the one middle job", ex.Dropped())
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ex.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(prompts, "|")
	if len(prompts) != 2 || !strings.Contains(joined, "first") || !strings.Contains(joined, "third") || strings.Contains(joined, "second") {
		t.Fatalf("processed prompts = %v, want first and third", prompts)
	}
	if ex.EnqueueTurn(MemoryTurn{Prompt: "", Response: "r"}) || ex.EnqueueTurn(MemoryTurn{Prompt: "p", Response: " "}) {
		t.Fatal("turns without both sides must be refused")
	}
}

func TestMemoryExtractorCloseCancelsInFlight(t *testing.T) {
	openTestMemory(t)
	prev := ModelBuilder
	t.Cleanup(func() { ModelBuilder = prev })
	started := make(chan struct{}, 1)
	ModelBuilder = func(_ context.Context, p providers.Provider, _ config.Config, modelID string) (model.LLM, error) {
		return &mockLLM{name: modelID, generateFunc: func(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
			started <- struct{}{}
			<-ctx.Done()
			return mockLLMSequence()
		}}, nil
	}
	ex := NewMemoryExtractor(MemoryExtractorOptions{LoadConfig: func() (config.Config, error) {
		return config.Config{Provider: providers.VertexProviderID}, nil
	}})
	ex.EnqueueTurn(MemoryTurn{Cwd: t.TempDir(), Prompt: "p", Response: "r", Provider: providers.VertexProviderID})
	ex.EnqueueTurn(MemoryTurn{Cwd: t.TempDir(), Prompt: "queued", Response: "r", Provider: providers.VertexProviderID})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job never started")
	}
	done := make(chan struct{})
	go func() {
		ex.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close must cancel the in-flight job and return")
	}
	if ex.EnqueueTurn(MemoryTurn{Prompt: "p", Response: "r"}) {
		t.Fatal("closed extractor must refuse turns")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ex.Drain(ctx); err != nil {
		t.Fatalf("queued jobs must be released on Close: %v", err)
	}
	ex.Close()
}

func TestEnqueueMemoryTurnWithoutMemory(t *testing.T) {
	_ = memory.Close()
	SetMemoryExtractor(nil)
	if EnsureMemoryExtractor() != nil {
		t.Fatal("no extractor without an open memory service")
	}
	if EnqueueMemoryTurn(MemoryTurn{Prompt: "p", Response: "r"}) {
		t.Fatal("enqueue must be a no-op without memory")
	}
	CloseMemoryExtractor()
}

func TestIngestWorkflowMemoryUsesLastExchange(t *testing.T) {
	openTestMemory(t)
	seen, mu := scriptExtractionModel(t, `{"topic":"","concepts":[]}`)
	ex := NewMemoryExtractor(MemoryExtractorOptions{LoadConfig: func() (config.Config, error) {
		return config.Config{Provider: providers.VertexProviderID}, nil
	}})
	SetMemoryExtractor(ex)
	t.Cleanup(ex.Close)

	ctx := context.Background()
	sessSvc := session.InMemoryService()
	created, err := sessSvc.Create(ctx, &session.CreateRequest{AppName: "ask", UserID: "user", SessionID: "wf-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []struct {
		role genai.Role
		text string
	}{{genai.RoleUser, "ship the login fix"}, {genai.RoleModel, "step one done"}, {genai.RoleModel, "opened PR 12"}} {
		e := session.NewEvent(ctx, "inv")
		e.Content = genai.NewContentFromText(ev.text, ev.role)
		_ = sessSvc.AppendEvent(ctx, created.Session, e)
	}
	cwd := t.TempDir()
	IngestWorkflowMemory(ctx, sessSvc, "wf-1", cwd)
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ex.Drain(dctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*seen) != 1 {
		t.Fatalf("expected one extraction, got %d", len(*seen))
	}
	content := (*seen)[0].Contents[0].Parts[0].Text
	if !strings.Contains(content, "User:\nship the login fix") || !strings.Contains(content, "step one done\nopened PR 12") {
		t.Fatalf("workflow turn content:\n%s", content)
	}
	IngestWorkflowMemory(ctx, sessSvc, "missing", cwd)
	IngestWorkflowMemory(ctx, nil, "wf-1", cwd)
}

func TestAppendTouchedFile(t *testing.T) {
	var files []string
	files = AppendTouchedFile(files, "read", map[string]any{"file_path": "a.go"})
	files = AppendTouchedFile(files, "edit", map[string]any{"file_path": "a.go"})
	files = AppendTouchedFile(files, "bash", map[string]any{"file_path": "b.go"})
	files = AppendTouchedFile(files, "write", map[string]any{"file_path": " "})
	files = AppendTouchedFile(files, "write", map[string]any{"file_path": "c.go"})
	if strings.Join(files, ",") != "a.go,c.go" {
		t.Fatalf("files = %v", files)
	}
	for i := 0; i < memoryExtractMaxFiles+5; i++ {
		files = AppendTouchedFile(files, "read", map[string]any{"file_path": "f" + itoa(int64(i))})
	}
	if len(files) != memoryExtractMaxFiles {
		t.Fatalf("files must cap at %d, got %d", memoryExtractMaxFiles, len(files))
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
