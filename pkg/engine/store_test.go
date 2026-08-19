package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/sessiontestsuite"
	"google.golang.org/genai"
)

func TestFileSessionService_ADKTestSuite(t *testing.T) {
	opts := sessiontestsuite.SuiteOptions{
		SupportsUserProvidedSessionID: true,
		ProvidesServerAssignedEventID: false,
		AppName:                       "ask",
	}
	sessiontestsuite.RunServiceTests(t, opts, func(t *testing.T) session.Service {
		tmpDir := t.TempDir()
		return NewFileSessionServiceWithBaseDir("vertex", tmpDir, tmpDir)
	})
}

func TestFileSessionService_CreateGetDelete(t *testing.T) {
	tmpDir := t.TempDir()
	svc := NewFileSessionServiceWithBaseDir("vertex", tmpDir, tmpDir)
	ctx := context.Background()

	// Create session
	sessionID := uuid.New().String()
	createResp, err := svc.Create(ctx, &session.CreateRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: sessionID,
		State: map[string]any{
			"key1": "val1",
		},
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if createResp.Session == nil {
		t.Fatal("expected non-nil Session in CreateResponse")
	}
	if createResp.Session.ID() != sessionID {
		t.Errorf("session ID mismatch: got %q, want %q", createResp.Session.ID(), sessionID)
	}
	if val, err := createResp.Session.State().Get("key1"); err != nil || val != "val1" {
		t.Errorf("expected state key1=val1, got %v, err=%v", val, err)
	}

	// Append event
	ev := &session.Event{
		Author:      "user",
		LLMResponse: adkmodelLLMResponse(genai.RoleUser, "User prompt 1"),
		Timestamp:   time.Now(),
	}
	if err := svc.AppendEvent(ctx, createResp.Session, ev); err != nil {
		t.Fatalf("AppendEvent failed: %v", err)
	}

	// Get session
	getResp, err := svc.Get(ctx, &session.GetRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if getResp.Session == nil || getResp.Session.Events().Len() != 1 {
		t.Fatalf("expected 1 event, got %v", getResp.Session.Events().Len())
	}

	// Delete session
	if err := svc.Delete(ctx, &session.DeleteRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: sessionID,
	}); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Get deleted session should fail
	if _, err := svc.Get(ctx, &session.GetRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: sessionID,
	}); err == nil {
		t.Error("expected error getting deleted session, got nil")
	}
}

func TestFileSessionService_AtomicPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	svc := NewFileSessionServiceWithBaseDir("vertex", tmpDir, tmpDir)
	ctx := context.Background()

	sessionID := "atomic-sess-1"
	_, err := svc.Create(ctx, &session.CreateRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	filePath, err := svc.PathFor(sessionID)
	if err != nil {
		t.Fatalf("PathFor failed: %v", err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("expected 0600 file permissions, got %o", perm)
	}

	// Ensure no dangling temp files left behind
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(filePath), ".*.tmp-*"))
	if len(matches) > 0 {
		t.Errorf("found dangling temp files: %+v", matches)
	}
}

func TestFileSessionService_ListNewestFirst(t *testing.T) {
	tmpDir := t.TempDir()
	svc := NewFileSessionServiceWithBaseDir("vertex", tmpDir, tmpDir)
	ctx := context.Background()

	// Create older session
	sess1, err := svc.Create(ctx, &session.CreateRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: "sess-older",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.AppendEvent(ctx, sess1.Session, &session.Event{
		Author:      "user",
		LLMResponse: adkmodelLLMResponse(genai.RoleUser, "first task"),
		Timestamp:   time.Now().Add(-1 * time.Hour),
	})

	// Create newer session
	sess2, err := svc.Create(ctx, &session.CreateRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: "sess-newer",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.AppendEvent(ctx, sess2.Session, &session.Event{
		Author:      "user",
		LLMResponse: adkmodelLLMResponse(genai.RoleUser, "second task"),
		Timestamp:   time.Now(),
	})

	listResp, err := svc.List(ctx, &session.ListRequest{
		AppName: "ask",
		UserID:  "user-1",
	})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listResp.Sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(listResp.Sessions))
	}
	if listResp.Sessions[0].ID() != "sess-newer" {
		t.Errorf("expected newest session first, got %s", listResp.Sessions[0].ID())
	}
}

func TestFileSessionService_PreserveMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	svc := NewFileSessionServiceWithBaseDir("vertex", tmpDir, tmpDir)
	ctx := context.Background()

	sessionID := "meta-sess-1"
	sess, err := svc.Create(ctx, &session.CreateRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	thoughtSig := []byte("secret-crypto-signature")
	ev := &session.Event{
		Author:             "ask_coder",
		InvocationID:       "inv-999",
		Branch:             "agent-branch",
		IsolationScope:     "isolated-scope-1",
		LongRunningToolIDs: []string{"tool-bg-1", "tool-bg-2"},
		Routes:             []string{"route-a", "route-b"},
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{
						Text:             "Reasoning",
						Thought:          true,
						ThoughtSignature: thoughtSig,
					},
					genai.NewPartFromText("Final answer text"),
				},
			},
		},
		Actions: session.EventActions{
			StateDelta: map[string]any{
				"custom_state": "saved_val",
			},
			TransferToAgent:   "sub_agent_coder",
			Escalate:          false,
			SkipSummarization: true,
		},
		Timestamp: time.Now(),
	}

	if err := svc.AppendEvent(ctx, sess.Session, ev); err != nil {
		t.Fatalf("AppendEvent failed: %v", err)
	}

	getResp, err := svc.Get(ctx, &session.GetRequest{
		AppName:   "ask",
		UserID:    "user-1",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	events := getResp.Session.Events()
	if events.Len() != 1 {
		t.Fatalf("expected 1 event, got %d", events.Len())
	}
	gotEv := events.At(0)
	if gotEv.Author != "ask_coder" {
		t.Errorf("Author mismatch: got %q, want %q", gotEv.Author, "ask_coder")
	}
	if gotEv.InvocationID != "inv-999" {
		t.Errorf("InvocationID mismatch: got %q, want %q", gotEv.InvocationID, "inv-999")
	}
	if gotEv.Branch != "agent-branch" {
		t.Errorf("Branch mismatch: got %q, want %q", gotEv.Branch, "agent-branch")
	}
	if gotEv.IsolationScope != "isolated-scope-1" {
		t.Errorf("IsolationScope mismatch: got %q, want %q", gotEv.IsolationScope, "isolated-scope-1")
	}
	if len(gotEv.LongRunningToolIDs) != 2 || gotEv.LongRunningToolIDs[0] != "tool-bg-1" {
		t.Errorf("LongRunningToolIDs mismatch: %+v", gotEv.LongRunningToolIDs)
	}
	if len(gotEv.Routes) != 2 || gotEv.Routes[0] != "route-a" {
		t.Errorf("Routes mismatch: %+v", gotEv.Routes)
	}
	if gotEv.Actions.TransferToAgent != "sub_agent_coder" {
		t.Errorf("TransferToAgent mismatch: %q", gotEv.Actions.TransferToAgent)
	}
	if !gotEv.Actions.SkipSummarization {
		t.Errorf("SkipSummarization should be true")
	}

	// Verify thought signature survived
	if len(gotEv.LLMResponse.Content.Parts) < 2 {
		t.Fatalf("expected at least 2 parts in content, got %d", len(gotEv.LLMResponse.Content.Parts))
	}
	if string(gotEv.LLMResponse.Content.Parts[0].ThoughtSignature) != string(thoughtSig) {
		t.Errorf("ThoughtSignature mismatch: got %s, want %s", gotEv.LLMResponse.Content.Parts[0].ThoughtSignature, thoughtSig)
	}

	// Verify StateDelta persisted to session state
	val, err := getResp.Session.State().Get("custom_state")
	if err != nil || val != "saved_val" {
		t.Errorf("expected session state custom_state=saved_val, got %v, err=%v", val, err)
	}
}

func adkmodelLLMResponse(role string, text string) adkmodel.LLMResponse {
	return adkmodel.LLMResponse{
		Content: &genai.Content{
			Role: role,
			Parts: []*genai.Part{
				genai.NewPartFromText(text),
			},
		},
	}
}
