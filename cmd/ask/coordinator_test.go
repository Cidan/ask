package main

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/engine"
	"google.golang.org/genai"
)

func TestCoordinator_RunWorkflowRestoreSession(t *testing.T) {
	isolateHome(t)

	// Create a fake provider
	prov := newFakeProvider()
	prov.id = "fake-prov"

	// Mock StartSession for the step
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		ch := make(chan tea.Msg, 8)
		proc := &providerProc{
			stdin: &bufferCloser{Buffer: nil},
		}

		// Create a session and set it as payload so the coordinator can set/remove it
		env := newAgentToolEnv(args.Cwd, args.TabID, true, true, func(msg tea.Msg) {})
		env.PendingEndTurn = &endTurnSignal{Summary: "step completed", Decision: "break"}
		env.PendingFinishData = &finishWorkflowData{Description: "completed successfully", Artifacts: []string{"art1"}}
		sess := &agentSession{
			args:   args,
			env:    env,
			sendCh: make(chan agentTurn, 8),
			closed: make(chan struct{}),
		}
		proc.payload = sess

		// Simulate step running and completing
		go func() {
			time.Sleep(10 * time.Millisecond)
			ch <- assistantTextMsg{text: "step result"}
			// Emit a done message with a mock end_turn tool call
			ch <- providerDoneMsg{
				res: providerResult{
					Result: "done",
				},
			}
			close(ch)
		}()

		return proc, ch, nil
	}

	withRegisteredProviders(t, prov)

	cwd := t.TempDir()
	startDir := filepath.Join(cwd, "ask", "plans", "start")
	if err := os.MkdirAll(startDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(startDir, "plan.txt"), []byte("dummy plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create parent session
	parentSess := &agentSession{
		args: ProviderSessionArgs{TabID: 42, Cwd: cwd},
	}
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 42, true, true, func(msg tea.Msg) {})

	// Setup Coordinator and active parent session
	c := globalCoordinator
	c.SetSession(42, parentSess)

	// Mock workflow with one simple step
	def := workflowDef{
		Name: "test-wf",
		Steps: []workflowStep{
			{
				Name:     "step-1",
				Provider: "fake-prov",
				Model:    "fake-model",
				Prompt:   "do something",
			},
		},
	}
	src := workflowSource{Kind: workflowSourceChat}

	// Run the workflow
	reply, err := c.RunWorkflow(context.Background(), 42, def, src)
	if err != nil {
		t.Fatalf("expected workflow to complete, got err: %v", err)
	}

	if !reply.workflowDone {
		t.Errorf("expected workflow to be marked done")
	}

	// Verify parent session was successfully restored!
	restored := c.GetSession(42)
	if restored != parentSess {
		t.Errorf("parent session was not restored correctly: got %v, want %v", restored, parentSess)
	}
}

func TestCoordinator_RunWorkflowCancellationStopRetries(t *testing.T) {
	isolateHome(t)

	prov := newFakeProvider()
	prov.id = "fake-prov"

	stepStartCh := make(chan struct{})

	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		ch := make(chan tea.Msg, 8)
		proc := &providerProc{
			stdin: &bufferCloser{Buffer: nil},
		}

		env := newAgentToolEnv(args.Cwd, args.TabID, true, true, func(msg tea.Msg) {})
		sess := &agentSession{
			args:   args,
			env:    env,
			sendCh: make(chan agentTurn, 8),
			closed: make(chan struct{}),
		}
		proc.payload = sess

		go func() {
			close(stepStartCh) // Notify test that the step has started

			// Stay running until step or workflow is cancelled
			time.Sleep(100 * time.Millisecond)
			ch <- providerDoneMsg{
				err: context.Canceled,
			}
			close(ch)
		}()

		return proc, ch, nil
	}

	withRegisteredProviders(t, prov)

	cwd := t.TempDir()
	startDir := filepath.Join(cwd, "ask", "plans", "start")
	if err := os.MkdirAll(startDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(startDir, "plan.txt"), []byte("dummy plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	parentSess := &agentSession{
		args: ProviderSessionArgs{TabID: 42, Cwd: cwd},
	}
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 42, true, true, func(msg tea.Msg) {})

	c := globalCoordinator
	c.SetSession(42, parentSess)

	def := workflowDef{
		Name: "test-wf",
		Steps: []workflowStep{
			{
				Name:     "step-1",
				Provider: "fake-prov",
				Model:    "fake-model",
				Prompt:   "do something",
			},
		},
	}
	src := workflowSource{Kind: workflowSourceChat}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run workflow in a separate goroutine
	errCh := make(chan error, 1)
	go func() {
		_, err := c.RunWorkflow(ctx, 42, def, src)
		errCh <- err
	}()

	// Wait for step to start
	<-stepStartCh

	// Call CancelWorkflow on coordinator
	c.CancelWorkflow(42)

	// Check if workflow exited quickly without getting stuck in 5 retries
	select {
	case err := <-errCh:
		if err == nil {
			t.Errorf("expected cancellation error, got nil")
		} else if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("workflow took too long to exit - likely retrying indefinitely")
	}
}

func TestCoordinator_RunWorkflowMissingPlanDirReminder(t *testing.T) {
	isolateHome(t)

	prov := newFakeProvider()
	prov.id = "fake-prov"

	var receivedPrompt string
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		ch := make(chan tea.Msg, 8)
		proc := &providerProc{
			stdin: &bufferCloser{Buffer: nil},
		}

		env := newAgentToolEnv(args.Cwd, args.TabID, true, true, func(msg tea.Msg) {})
		env.PendingEndTurn = &endTurnSignal{Summary: "step completed", Decision: "break"}
		env.PendingFinishData = &finishWorkflowData{Description: "completed successfully", Artifacts: []string{"art1"}}
		sess := &agentSession{
			args:   args,
			env:    env,
			sendCh: make(chan agentTurn, 8),
			closed: make(chan struct{}),
		}
		proc.payload = sess

		go func() {
			select {
			case turn := <-sess.sendCh:
				receivedPrompt = turn.text
			case <-time.After(500 * time.Millisecond):
			}
		}()

		go func() {
			time.Sleep(10 * time.Millisecond)
			ch <- assistantTextMsg{text: "step result"}
			ch <- providerDoneMsg{
				res: providerResult{
					Result: "done",
				},
			}
			close(ch)
		}()

		return proc, ch, nil
	}

	withRegisteredProviders(t, prov)

	cwd := t.TempDir()

	parentSess := &agentSession{
		args: ProviderSessionArgs{TabID: 43, Cwd: cwd},
	}
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 43, true, true, func(msg tea.Msg) {})

	c := globalCoordinator
	c.SetSession(43, parentSess)

	def := workflowDef{
		Name: "test-wf",
		Steps: []workflowStep{
			{
				Name:     "step-1",
				Provider: "fake-prov",
				Model:    "fake-model",
				Prompt:   "do something",
			},
		},
	}
	src := workflowSource{Kind: workflowSourceChat}

	reply, err := c.RunWorkflow(context.Background(), 43, def, src)
	if err != nil {
		t.Fatalf("expected workflow to complete, got err: %v", err)
	}

	if !reply.workflowDone {
		t.Errorf("expected workflow to be marked done")
	}

	wantSub := "REMINDER: the workflow notes directory is not usable"
	if receivedPrompt == "" {
		t.Errorf("did not receive any prompt")
	} else if !strings.Contains(receivedPrompt, wantSub) {
		t.Errorf("expected prompt to contain %q, but got:\n%s", wantSub, receivedPrompt)
	}
}

func TestCoordinator_RunWorkflowLoopWithDecisionAndFinish(t *testing.T) {
	isolateHome(t)

	// Since we are running the workflow, we can override agentSendToProgram
	// to capture the message or return true.
	oldSend := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool { return true }
	t.Cleanup(func() { agentSendToProgram = oldSend })

	origStream := engine.GenerateStream
	defer func() { engine.GenerateStream = origStream }()

	var turnIdx int
	var mu sync.Mutex
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		mu.Lock()
		idx := turnIdx
		turnIdx++
		mu.Unlock()

		var chunk *genai.GenerateContentResponse
		switch idx {
		case 0:
			chunk = genaiToolCallChunk("read", map[string]any{"file_path": "ask/plans/start/plan.txt", "description": "read start plan"})
		case 1:
			chunk = genaiToolCallChunk("end_turn", map[string]any{"summary": "validated the plan"})
		case 2:
			chunk = genaiTextChunk("completed", 10, 10)
		case 3:
			chunk = genaiToolCallChunk("write", map[string]any{"file_path": "hello.go", "content": "package main\n\nfunc main() {}\n", "description": "create hello.go with main function"})
		case 4:
			chunk = genaiToolCallChunk("end_turn", map[string]any{"summary": "implemented changes"})
		case 5:
			chunk = genaiTextChunk("completed", 10, 10)
		case 6:
			chunk = genaiToolCallChunk("read", map[string]any{"file_path": "hello.go", "description": "verify hello.go before continuing"})
		case 7:
			chunk = genaiToolCallChunk("end_turn", map[string]any{"summary": "validated changes", "decision": "continue"})
		case 8:
			chunk = genaiTextChunk("completed", 10, 10)
		case 9:
			chunk = genaiToolCallChunk("read", map[string]any{"file_path": "hello.go", "description": "read hello.go before editing"})
		case 10:
			chunk = genaiToolCallChunk("edit", map[string]any{"file_path": "hello.go", "old_string": "package main\n\nfunc main() {}\n", "new_string": "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n", "description": "add print to main"})
		case 11:
			chunk = genaiToolCallChunk("end_turn", map[string]any{"summary": "implemented more changes"})
		case 12:
			chunk = genaiTextChunk("completed", 10, 10)
		case 13:
			chunk = genaiToolCallChunk("read", map[string]any{"file_path": "hello.go", "description": "verify hello.go before break"})
		case 14:
			chunk = genaiToolCallChunk("end_turn", map[string]any{"summary": "validated and broke loop", "decision": "break"})
		case 15:
			chunk = genaiTextChunk("completed", 10, 10)
		case 16:
			chunk = genaiToolCallChunk("finish_workflow", map[string]any{"description": "workflow executed successfully", "artifacts": []any{"hello.go"}})
		case 17:
			chunk = genaiToolCallChunk("end_turn", map[string]any{"summary": "finalized the workflow"})
		case 18:
			chunk = genaiTextChunk("completed", 10, 10)
		default:
			chunk = genaiTextChunk("done", 10, 10)
		}

		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(chunk, nil)
		}
	}

	prov := newFakeProvider()
	prov.id = "fake-prov"

	stepCount := 0
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		stepCount++

		sess := &agentSession{
			args:          args,
			system:        "test system prompt",
			contextWindow: 1_048_576,
			modelID:       "fake-model",
			ch:            make(chan tea.Msg, 32),
			sendCh:        make(chan agentTurn, 8),
			closed:        make(chan struct{}),
			sessionID:     "ses-test",
		}
		cfg, _ := loadConfig()
		sess.env = newAgentToolEnv(args.Cwd, args.TabID, true, false, sess.emit)
		setupAgentSessionTools(sess, cfg)
		sess.tools = sess.coreTools

		proc := &providerProc{
			stdin:   agentStdin{s: sess},
			stderr:  &stderrBuf{},
			payload: sess,
		}
		sess.proc = proc
		go sess.run()
		return proc, sess.ch, nil
	}

	withRegisteredProviders(t, prov)

	cwd := t.TempDir()
	startDir := filepath.Join(cwd, "ask", "plans", "start")
	if err := os.MkdirAll(startDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(startDir, "plan.txt"), []byte("dummy plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	parentSess := &agentSession{
		args: ProviderSessionArgs{TabID: 44, Cwd: cwd},
	}
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 44, true, true, func(msg tea.Msg) {})

	c := globalCoordinator
	c.SetSession(44, parentSess)

	def := workflowDef{
		Name: "ship",
		Steps: []workflowStep{
			{
				Name:     "Validate plan",
				Provider: "fake-prov",
				Model:    "fake-model",
				Prompt:   "validate plan",
			},
			{
				Name: "Loop: Execute/Validate",
				Kind: "loop",
				Steps: []workflowStep{
					{
						Name:     "Implement changes",
						Provider: "fake-prov",
						Model:    "fake-model",
						Prompt:   "implement",
					},
					{
						Name:     "Validate changes",
						Provider: "fake-prov",
						Model:    "fake-model",
						Prompt:   "validate",
					},
				},
				MaxIterations: 3,
			},
			{
				Name:     "Finalize",
				Provider: "fake-prov",
				Model:    "fake-model",
				Prompt:   "finalize",
			},
		},
	}
	src := workflowSource{Kind: workflowSourceChat}

	reply, err := c.RunWorkflow(context.Background(), 44, def, src)
	if err != nil {
		t.Fatalf("expected workflow to complete, got err: %v", err)
	}

	if !reply.workflowDone {
		t.Errorf("expected workflow to be marked done")
	}

	if stepCount != 6 {
		t.Errorf("expected exactly 6 steps to run, got %d", stepCount)
	}

	if reply.outcome != "workflow executed successfully" {
		t.Errorf("expected outcome to be 'workflow executed successfully', got %q", reply.outcome)
	}

	var foundFiles []string
	_ = filepath.Walk(cwd, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			foundFiles = append(foundFiles, path)
		}
		return nil
	})
	t.Logf("Found files in cwd: %v", foundFiles)

	// Verify file was written, read, edited, and validated correctly on disk!
	helloPath := filepath.Join(cwd, "hello.go")
	content, err := os.ReadFile(helloPath)
	if err != nil {
		t.Fatalf("expected hello.go to be written, got err: %v", err)
	}
	wantContent := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	if string(content) != wantContent {
		t.Errorf("hello.go has wrong content:\ngot:\n%q\nwant:\n%q", string(content), wantContent)
	}
}
