package memory

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type fakeEmbedder struct {
	dim int
}

func newFakeEmbedder(dim int) *fakeEmbedder {
	return &fakeEmbedder{dim: dim}
}

func (f *fakeEmbedder) Embed(text string) ([]float32, error) {
	vec := make([]float32, f.dim)
	var h uint32 = 2166136261
	for i := 0; i < len(text); i++ {
		h = (h ^ uint32(text[i])) * 16777619
	}
	for i := 0; i < f.dim; i++ {
		vec[i] = float32((h+uint32(i*31))%1000) / 1000.0
	}
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		s := float32(1.0 / math.Sqrt(float64(norm)))
		for i := range vec {
			vec[i] *= s
		}
	}
	return vec, nil
}

func (f *fakeEmbedder) EmbdSize() int {
	return f.dim
}

func (f *fakeEmbedder) Close() {}

func TestMemoryService_Lifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_memory.db")

	embedder := newFakeEmbedder(64)
	svc, err := NewService(Options{
		DBPath:   dbPath,
		Embedder: embedder,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	if !svc.IsOpen() {
		t.Fatal("expected service to be open")
	}

	ctx := context.Background()
	cwd := filepath.Join(tmpDir, "myproj")

	err = svc.Index(ctx, cwd, "Initial architectural design decision")
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	hits, err := svc.Recall(ctx, cwd, "Initial architectural", 5)
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].Text != "Initial architectural design decision" {
		t.Errorf("unexpected hit text: %s", hits[0].Text)
	}

	// Close service
	if err := svc.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if svc.IsOpen() {
		t.Fatal("expected service to be closed")
	}
}

func TestMemoryService_ProjectIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "isolation.db")

	embedder := newFakeEmbedder(64)
	svc, err := NewService(Options{
		DBPath:   dbPath,
		Embedder: embedder,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	projA := filepath.Join(tmpDir, "project-a")
	projB := filepath.Join(tmpDir, "project-b")

	if err := svc.Index(ctx, projA, "Project A secret documentation"); err != nil {
		t.Fatalf("Index projA: %v", err)
	}
	if err := svc.Index(ctx, projB, "Project B secret documentation"); err != nil {
		t.Fatalf("Index projB: %v", err)
	}

	// Query from projA should only return projA records
	hitsA, err := svc.Recall(ctx, projA, "Project A secret documentation", 5)
	if err != nil {
		t.Fatalf("Recall projA: %v", err)
	}
	if len(hitsA) != 1 || hitsA[0].Text != "Project A secret documentation" {
		t.Fatalf("expected only projA hit, got: %+v", hitsA)
	}

	// Query from projB should only return projB records
	hitsB, err := svc.Recall(ctx, projB, "Project B secret documentation", 5)
	if err != nil {
		t.Fatalf("Recall projB: %v", err)
	}
	if len(hitsB) != 1 || hitsB[0].Text != "Project B secret documentation" {
		t.Fatalf("expected only projB hit, got: %+v", hitsB)
	}
}

func TestMemoryService_Sweep(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "sweep.db")

	embedder := newFakeEmbedder(64)
	svc, err := NewService(Options{
		DBPath:   dbPath,
		Embedder: embedder,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	cwd := filepath.Join(tmpDir, "testproj")

	if err := svc.Index(ctx, cwd, "Recent memory entry"); err != nil {
		t.Fatalf("Index recent: %v", err)
	}

	// Artificially age a record
	_, err = svc.db.Exec(`
		UPDATE project_memory SET last_recalled_at = datetime('now', '-40 days')
	`)
	if err != nil {
		t.Fatalf("aging record failed: %v", err)
	}

	// Run sweep
	if err := svc.Sweep(ctx); err != nil {
		t.Fatalf("Sweep failed: %v", err)
	}

	// Recall should find nothing
	hits, err := svc.Recall(ctx, cwd, "Recent memory entry", 5)
	if err != nil {
		t.Fatalf("Recall after sweep: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits after sweep, got %d", len(hits))
	}
}

func TestMemoryService_ConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "concurrent.db")

	embedder := newFakeEmbedder(32)
	svc, err := NewService(Options{
		DBPath:   dbPath,
		Embedder: embedder,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	cwd := filepath.Join(tmpDir, "proj")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			_ = svc.Index(ctx, cwd, "Concurrent indexed text entry")
		}(i)
		go func(idx int) {
			defer wg.Done()
			_, _ = svc.Recall(ctx, cwd, "Concurrent indexed", 3)
		}(i)
	}
	wg.Wait()
}

func TestFormatRecallContext(t *testing.T) {
	hits := []RecallHit{
		{ID: 1, Text: "First rule"},
		{ID: 2, Text: "Second note"},
	}

	formatted := FormatRecallContext(hits, "Project Memory")
	expected := "## Project Memory\n\n1. First rule\n2. Second note"
	if formatted != expected {
		t.Fatalf("expected:\n%q\ngot:\n%q", expected, formatted)
	}

	if empty := FormatRecallContext(nil, "Heading"); empty != "" {
		t.Errorf("expected empty string for nil hits, got %q", empty)
	}
}

func TestPromptContext_And_SystemBlock(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "global.db")
	embedder := newFakeEmbedder(32)

	_ = Close()
	defer Close()

	if block := SystemBlock(context.Background(), "/tmp"); block != "" {
		t.Errorf("expected empty block when service closed, got %q", block)
	}
	if prompt := PromptContext(context.Background(), "/tmp", "test"); prompt != "" {
		t.Errorf("expected empty prompt context when service closed, got %q", prompt)
	}

	err := Open(Options{
		DBPath:   dbPath,
		Embedder: embedder,
	})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx := context.Background()
	cwd := filepath.Join(tmpDir, "proj")
	_ = Index(ctx, cwd, "Database migration guidelines")

	prompt := PromptContext(ctx, cwd, "Database migration guidelines")
	if prompt == "" {
		t.Error("expected non-empty prompt recall context")
	}
}

func TestMemoryService_ADKServiceCompliance(t *testing.T) {
	var _ adkmemory.Service = (*Service)(nil)

	// Closed service calls should fail gracefully or return empty response
	var closedSvc *Service
	if err := closedSvc.AddSessionToMemory(context.Background(), nil); err == nil {
		t.Error("expected error when adding session to closed service")
	}
	resp, err := closedSvc.SearchMemory(context.Background(), &adkmemory.SearchRequest{Query: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Errorf("expected 0 memories for closed service, got %d", len(resp.Memories))
	}
}

func TestMemoryService_AddSessionToMemory(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "session_mem.db")

	embedder := newFakeEmbedder(64)
	svc, err := NewService(Options{
		DBPath:   dbPath,
		Embedder: embedder,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	sessSvc := session.InMemoryService()
	sess, err := sessSvc.Create(ctx, &session.CreateRequest{
		AppName:   "ask",
		UserID:    "user",
		SessionID: "sess-123",
	})
	if err != nil {
		t.Fatalf("session create failed: %v", err)
	}

	// Append user and model events
	userEvent := session.NewEvent(ctx, "inv-1")
	userEvent.Content = genai.NewContentFromText("I prefer snake_case naming conventions across all Go packages.", genai.RoleUser)
	_ = sessSvc.AppendEvent(ctx, sess.Session, userEvent)

	modelEvent := session.NewEvent(ctx, "inv-2")
	modelEvent.Content = genai.NewContentFromText("Acknowledged, I will follow snake_case naming conventions.", genai.RoleModel)
	_ = sessSvc.AppendEvent(ctx, sess.Session, modelEvent)

	// Ingest session
	if err := svc.AddSessionToMemory(ctx, sess.Session); err != nil {
		t.Fatalf("AddSessionToMemory failed: %v", err)
	}

	// Query via SearchMemory
	searchResp, err := svc.SearchMemory(ctx, &adkmemory.SearchRequest{
		Query: "snake_case naming conventions",
	})
	if err != nil {
		t.Fatalf("SearchMemory failed: %v", err)
	}
	if len(searchResp.Memories) == 0 {
		t.Fatal("expected at least 1 memory entry, got 0")
	}

	found := false
	for _, m := range searchResp.Memories {
		if m.Content != nil && len(m.Content.Parts) > 0 && m.Content.Parts[0].Text != "" {
			if strings.Contains(m.Content.Parts[0].Text, "snake_case") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected to find ingested snake_case memory in SearchMemory response")
	}
}

func TestMemoryService_SearchMemory(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "search_mem.db")

	embedder := newFakeEmbedder(64)
	svc, err := NewService(Options{
		DBPath:   dbPath,
		Embedder: embedder,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	cwd := filepath.Join(tmpDir, "myproj")

	if err := svc.Index(ctx, cwd, "Deploy pipeline requires staging validation first"); err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Search matching query
	resp, err := svc.SearchMemory(ctx, &adkmemory.SearchRequest{
		Query: "staging validation pipeline",
	})
	if err != nil {
		t.Fatalf("SearchMemory failed: %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("expected 1 memory entry, got %d", len(resp.Memories))
	}

	entry := resp.Memories[0]
	if entry.ID == "" {
		t.Error("expected non-empty memory entry ID")
	}
	if entry.Content == nil || len(entry.Content.Parts) == 0 || entry.Content.Parts[0].Text != "Deploy pipeline requires staging validation first" {
		t.Errorf("unexpected content in memory entry: %+v", entry.Content)
	}
	if entry.CustomMetadata == nil || entry.CustomMetadata["project_id"] == "" {
		t.Errorf("expected project_id in custom metadata: %+v", entry.CustomMetadata)
	}

	// Search empty query
	emptyResp, err := svc.SearchMemory(ctx, &adkmemory.SearchRequest{
		Query: "",
	})
	if err != nil {
		t.Fatalf("SearchMemory empty query failed: %v", err)
	}
	if len(emptyResp.Memories) != 0 {
		t.Errorf("expected 0 memories for empty query, got %d", len(emptyResp.Memories))
	}
}

func TestRealModel_IfAvailable(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("user home not found")
	}

	modelPath := filepath.Join(realHome, ".local", "share", "ask", "models", "embeddinggemma-300M-Q8_0.gguf")
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skip("real GGUF model not found, skipping real model test")
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "real_model.db")

	svc, err := NewService(Options{
		DBPath:    dbPath,
		ModelPath: modelPath,
	})
	if err != nil {
		t.Fatalf("NewService with real model failed: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	cwd := filepath.Join(tmpDir, "real_proj")

	if err := svc.Index(ctx, cwd, "Refactoring user authentication logic"); err != nil {
		t.Fatalf("Index real model: %v", err)
	}

	hits, err := svc.Recall(ctx, cwd, "user authentication", 3)
	if err != nil {
		t.Fatalf("Recall real model: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least 1 hit with real model")
	}
	if hits[0].Text != "Refactoring user authentication logic" {
		t.Errorf("unexpected hit text: %s", hits[0].Text)
	}
}
