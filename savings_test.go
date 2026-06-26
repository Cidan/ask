package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRecordSavings(t *testing.T) {
	// Isolate home directory to avoid touching real config
	isolateHome(t)

	// Single record
	err := RecordSavings("go test", 100)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	configDir, _ := os.UserConfigDir()
	filePath := filepath.Join(configDir, "ask", "savings.json")
	b, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read savings.json: %v", err)
	}

	var savings TokenSavings
	if err := json.Unmarshal(b, &savings); err != nil {
		t.Fatalf("failed to parse json: %v", err)
	}

	if savings.TotalSavedTokens != 100 {
		t.Errorf("expected 100 total saved tokens, got %d", savings.TotalSavedTokens)
	}
	if savings.ByCommand["go test"].Count != 1 {
		t.Errorf("expected count 1, got %d", savings.ByCommand["go test"].Count)
	}

	// Concurrent writes
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = RecordSavings("npm install", 50)
		}()
	}
	wg.Wait()

	b, _ = os.ReadFile(filePath)
	_ = json.Unmarshal(b, &savings)

	if savings.TotalSavedTokens != 600 { // 100 + (10 * 50)
		t.Errorf("expected 600 total saved tokens, got %d", savings.TotalSavedTokens)
	}
	if savings.ByCommand["npm install"].Count != 10 {
		t.Errorf("expected count 10 for npm install, got %d", savings.ByCommand["npm install"].Count)
	}
	if savings.ByCommand["npm install"].SavedTokens != 500 {
		t.Errorf("expected 500 saved tokens for npm install, got %d", savings.ByCommand["npm install"].SavedTokens)
	}
}
