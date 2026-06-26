package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryService_Lifecycle(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get real home: %v", err)
	}

	fakeHome := isolateHome(t)

	modelName := "embeddinggemma-300M-Q8_0.gguf"
	realModelPath := filepath.Join(realHome, ".local", "share", "ask", "models", modelName)
	if _, err := os.Stat(realModelPath); os.IsNotExist(err) {
		t.Skipf("real model not found at %s, skipping end-to-end memory test", realModelPath)
	}

	fakeModelDir := filepath.Join(fakeHome, ".local", "share", "ask", "models")
	if err := os.MkdirAll(fakeModelDir, 0755); err != nil {
		t.Fatalf("mkdir fake model dir: %v", err)
	}

	fakeModelPath := filepath.Join(fakeModelDir, modelName)
	if err := os.Symlink(realModelPath, fakeModelPath); err != nil {
		t.Fatalf("failed to symlink model: %v", err)
	}

	closeMemoryService()
	defer closeMemoryService()

	if memoryServiceOpen() {
		t.Fatal("expected memory service to be closed initially")
	}

	err = openMemoryService(askConfig{})
	if err != nil {
		t.Fatalf("openMemoryService failed: %v", err)
	}

	if !memoryServiceOpen() {
		t.Fatal("expected memory service to be open")
	}

	ctx := context.Background()
	cwd := filepath.Join("test", "cwd")

	err = memoryIndex(ctx, cwd, "hello world")
	if err != nil {
		t.Fatalf("memoryIndex failed: %v", err)
	}

	hits, err := memoryRecall(ctx, cwd, "hello", 5)
	if err != nil {
		t.Fatalf("memoryRecall failed: %v", err)
	}

	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}

	if hits[0].Text != "hello world" {
		t.Errorf("expected text 'hello world', got '%s'", hits[0].Text)
	}

	// Verify sweep runs without error by invoking it directly.
	sweepOldMemories()
}
