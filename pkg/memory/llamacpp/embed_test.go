package llamacpp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cidan/ask/pkg/memory"
)

func TestRealModel_IfAvailable(t *testing.T) {
	modelPath, err := DefaultModelPath()
	if err != nil {
		t.Skip("user home not found")
	}
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skip("real GGUF model not found, skipping real model test")
	}
	embedder, err := LoadEmbeddingModel(modelPath)
	if err != nil {
		t.Fatalf("LoadEmbeddingModel: %v", err)
	}
	if embedder.EmbdSize() <= 0 {
		t.Fatalf("EmbdSize = %d, want > 0", embedder.EmbdSize())
	}
	svc, err := memory.NewService(memory.Options{DBPath: filepath.Join(t.TempDir(), "real.db"), Embedder: embedder})
	if err != nil {
		t.Fatalf("NewService with real model failed: %v", err)
	}
	defer svc.Close()
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "real_proj")
	if _, err := svc.Upsert(ctx, memory.Concept{Scope: memory.ScopeFor(cwd), Kind: memory.KindProject, Title: "Refactoring user authentication logic"}); err != nil {
		t.Fatalf("Upsert real model: %v", err)
	}
	res, err := svc.Recall(ctx, memory.RecallQuery{Cwd: cwd, Query: "user authentication", K: 3})
	if err != nil || len(res.Concepts) == 0 {
		t.Fatalf("Recall real model: %+v err=%v", res, err)
	}
	if res.Concepts[0].Title != "Refactoring user authentication logic" {
		t.Errorf("unexpected hit: %s", res.Concepts[0].Title)
	}
}
