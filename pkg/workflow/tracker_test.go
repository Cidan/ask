package workflow

import (
	"sync"
	"testing"
)

func TestTracker_Lifecycle(t *testing.T) {
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
