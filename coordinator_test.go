package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
		env.pendingEndTurn = &endTurnSignal{summary: "step completed", decision: "break"}
		env.pendingFinishData = &finishWorkflowData{Description: "completed successfully", Artifacts: []string{"art1"}}
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
		env.pendingEndTurn = &endTurnSignal{summary: "step completed", decision: "break"}
		env.pendingFinishData = &finishWorkflowData{Description: "completed successfully", Artifacts: []string{"art1"}}
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
