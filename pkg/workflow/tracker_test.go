package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTracker_Lifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tr := NewTracker()
	cwd := t.TempDir()
	key := "issue-123"
	workflowName := "ship-feature"
	tabID := 7

	var statusLog []string
	var mu sync.Mutex
	tr.SetListener(func(k, status string) {
		mu.Lock()
		defer mu.Unlock()
		if k == key {
			statusLog = append(statusLog, status)
		}
	})

	// 1. Initial lookup should be empty
	if _, ok := tr.Lookup(cwd, key); ok {
		t.Fatalf("expected key %q to not exist initially", key)
	}

	// 2. Mark working
	tr.MarkWorking(cwd, key, workflowName, tabID)
	entry, ok := tr.Lookup(cwd, key)
	if !ok || entry.Status != StatusWorking {
		t.Fatalf("expected status %q, got %+v (ok=%v)", StatusWorking, entry, ok)
	}
	if entry.TabID != tabID {
		t.Errorf("expected tabID %d, got %d", tabID, entry.TabID)
	}
	if entry.Workflow != workflowName {
		t.Errorf("expected workflow %q, got %q", workflowName, entry.Workflow)
	}

	// Active tab and names
	if activeTab, ok := tr.ActiveTabFor(key); !ok || activeTab != tabID {
		t.Errorf("expected active tab %d, got %d (ok=%v)", tabID, activeTab, ok)
	}
	activeNames := tr.ActiveWorkflowNames()
	if _, ok := activeNames[workflowName]; !ok {
		t.Errorf("expected %q in active workflow names", workflowName)
	}

	// 3. Mark step
	tr.MarkStep(key, 2)
	entry, _ = tr.Lookup(cwd, key)
	if entry.StepIndex != 2 {
		t.Errorf("expected step index 2, got %d", entry.StepIndex)
	}

	// 4. Mark final (Done)
	tr.MarkFinal(cwd, key, workflowName, StatusDone, 3)
	entry, ok = tr.Lookup(cwd, key)
	if !ok || entry.Status != StatusDone {
		t.Fatalf("expected status %q, got %+v", StatusDone, entry)
	}
	if entry.StepIndex != 3 {
		t.Errorf("expected step index 3, got %d", entry.StepIndex)
	}

	// Not active anymore
	if _, ok := tr.ActiveTabFor(key); ok {
		t.Errorf("expected ActiveTabFor to return false after marking final")
	}

	// 5. Clear
	tr.Clear(key)
	mu.Lock()
	lastStatus := ""
	if len(statusLog) > 0 {
		lastStatus = statusLog[len(statusLog)-1]
	}
	mu.Unlock()
	if lastStatus != "" {
		t.Errorf("expected empty status broadcast on clear, got %q", lastStatus)
	}
}

func TestTracker_ResetForTest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tr := NewTracker()
	tr.MarkWorking(t.TempDir(), "k1", "w1", 1)
	if _, ok := tr.ActiveTabFor("k1"); !ok {
		t.Fatalf("expected k1 to be active")
	}
	tr.ResetForTest()
	if _, ok := tr.ActiveTabFor("k1"); ok {
		t.Errorf("expected k1 to be cleared after ResetForTest")
	}
}

func TestTracker_MarkFinal_PreservesSkipAllPermissions(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	configDir := filepath.Join(tmpHome, ".config", "ask")
	_ = os.MkdirAll(configDir, 0755)
	initialConfig := []byte(`{
  "provider": "openai",
  "ui": {
    "skipAllPermissions": true,
    "quietMode": true,
    "cursorBlink": false,
    "renderDiffs": false,
    "toolOutput": "short"
  }
}
`)
	configFile := filepath.Join(configDir, "ask.json")
	if err := os.WriteFile(configFile, initialConfig, 0600); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	tr := NewTracker()
	cwd := t.TempDir()
	tr.MarkFinal(cwd, "test-issue", "ship", StatusDone, 1)

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("failed to read config file after MarkFinal: %v", err)
	}

	var parsed struct {
		UI struct {
			SkipAllPermissions *bool   `json:"skipAllPermissions"`
			QuietMode          *bool   `json:"quietMode"`
			CursorBlink        *bool   `json:"cursorBlink"`
			RenderDiffs        *bool   `json:"renderDiffs"`
			ToolOutput         *string `json:"toolOutput"`
		} `json:"ui"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal saved config: %v", err)
	}

	if parsed.UI.SkipAllPermissions == nil || !*parsed.UI.SkipAllPermissions {
		t.Errorf("BUG REPRODUCED: skipAllPermissions was stripped: %s", string(data))
	}
	if parsed.UI.QuietMode == nil || !*parsed.UI.QuietMode {
		t.Errorf("BUG REPRODUCED: quietMode was stripped: %s", string(data))
	}
}
