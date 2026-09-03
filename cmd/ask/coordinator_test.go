package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestCoordinator_RunWorkflowRestoreSession(t *testing.T) {
	isolateHome(t)
	stubWorkflowStepModel(t)

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
		env := newAgentToolEnv(args.Cwd, args.TabID, true, func(msg tea.Msg) {})
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
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 42, true, func(msg tea.Msg) {})

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
	reply, err := c.RunWorkflow(context.Background(), 42, workflowWorktree{root: cwd}, def, src)
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
	stubWorkflowStepModel(t)

	prov := newFakeProvider()
	prov.id = "fake-prov"

	stepStartCh := make(chan struct{})

	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		ch := make(chan tea.Msg, 8)
		proc := &providerProc{
			stdin: &bufferCloser{Buffer: nil},
		}

		env := newAgentToolEnv(args.Cwd, args.TabID, true, func(msg tea.Msg) {})
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
	parentSess.env = newAgentToolEnv(parentSess.args.Cwd, 42, true, func(msg tea.Msg) {})

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
		_, err := c.RunWorkflow(ctx, 42, workflowWorktree{root: cwd}, def, src)
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

// The graph engine runs a whole workflow on ONE agent session: the
// session's agent is the compiled graph, and ADK's scheduler walks the
// nodes. The old runner opened a fresh provider session per step, so a
// three-step workflow started three of them.
func TestCoordinator_RunWorkflowUsesOneSessionForTheWholeGraph(t *testing.T) {
	isolateHome(t)
	stubWorkflowStepModel(t)

	var sessionsStarted int32
	prov := newFakeProvider()
	prov.id = "fake-prov"
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		atomic.AddInt32(&sessionsStarted, 1)
		ch := make(chan tea.Msg, 8)
		env := newAgentToolEnv(args.Cwd, args.TabID, true, func(msg tea.Msg) {})
		proc := &providerProc{stdin: &bufferCloser{Buffer: nil}}
		proc.payload = &agentSession{
			args:   args,
			env:    env,
			sendCh: make(chan agentTurn, 8),
			closed: make(chan struct{}),
		}
		go func() {
			ch <- providerDoneMsg{res: providerResult{Result: "ok"}}
			ch <- turnCompleteMsg{}
			close(ch)
		}()
		return proc, ch, nil
	}
	withRegisteredProviders(t, prov)

	cwd := t.TempDir()
	c := globalCoordinator
	parent := &agentSession{args: ProviderSessionArgs{TabID: 77, Cwd: cwd}}
	parent.env = newAgentToolEnv(cwd, 77, true, func(msg tea.Msg) {})
	c.SetSession(77, parent)
	defer c.RemoveSession(77)

	def := workflowDef{Name: "three-step", Steps: []workflowStep{
		{Name: "one", Provider: "fake-prov", Model: "fake-model", Prompt: "a"},
		{Name: "two", Provider: "fake-prov", Model: "fake-model", Prompt: "b"},
		{Name: "three", Provider: "fake-prov", Model: "fake-model", Prompt: "c"},
	}}

	if _, err := c.RunWorkflow(context.Background(), 77, workflowWorktree{root: cwd}, def, workflowSource{Kind: workflowSourceChat}); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}
	if got := atomic.LoadInt32(&sessionsStarted); got != 1 {
		t.Errorf("workflow started %d provider sessions, want exactly 1 for the whole graph", got)
	}
}

// TestCoordinator_Dispatch_CreatesWorktreeAndChip verifies the in-process
// worktree wiring restored after the CLI-provider removal: with worktree mode
// on in a git repo, Dispatch creates the worktree, repoints the session's cwd
// into it, and emits a providerCwdMsg so the tab can show the [🌳 name] chip.
func TestCoordinator_Dispatch_CreatesWorktreeAndChip(t *testing.T) {
	isolateHome(t)
	repo := initGitRepo(t) // skips if git is unavailable
	t.Chdir(repo)

	prov := newFakeProvider()
	prov.id = "fake-prov"
	sessCh := make(chan tea.Msg, 8)
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		proc := &providerProc{}
		sess := &agentSession{
			args:   args,
			env:    newAgentToolEnv(args.Cwd, args.TabID, true, func(msg tea.Msg) {}),
			ch:     sessCh,
			sendCh: make(chan agentTurn, 8),
			closed: make(chan struct{}),
		}
		proc.payload = sess
		sess.proc = proc
		return proc, sessCh, nil
	}
	withRegisteredProviders(t, prov)

	c := &Coordinator{sessions: map[int]*agentSession{}, workflowCancels: map[int]context.CancelFunc{}}
	args := ProviderSessionArgs{Cwd: repo, TabID: 1, Worktree: true, SkipAllPermissions: true}
	if err := c.Dispatch(1, prov, args, "hello", nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// StartSession must have been called with a cwd inside .claude/worktrees.
	prov.mu.Lock()
	starts := append([]ProviderSessionArgs(nil), prov.startArgs...)
	prov.mu.Unlock()
	if len(starts) != 1 {
		t.Fatalf("StartSession called %d times, want 1", len(starts))
	}
	gotCwd := starts[0].Cwd
	if worktreeNameFromCwd(gotCwd) == "" {
		t.Fatalf("session cwd %q is not inside .claude/worktrees", gotCwd)
	}
	// The worktree directory actually exists on disk.
	if fi, err := os.Stat(gotCwd); err != nil || !fi.IsDir() {
		t.Fatalf("worktree dir %q missing: %v", gotCwd, err)
	}
	// A providerCwdMsg with that cwd was emitted so the tab can show the chip.
	var sawCwd string
	for {
		select {
		case msg := <-sessCh:
			if cm, ok := msg.(providerCwdMsg); ok {
				sawCwd = cm.cwd
			}
			continue
		default:
		}
		break
	}
	if sawCwd == "" {
		t.Fatal("no providerCwdMsg emitted — the worktree chip would never appear")
	}
	if worktreeNameFromCwd(sawCwd) == "" {
		t.Errorf("providerCwdMsg cwd %q is not a worktree", sawCwd)
	}
}

// TestCoordinator_Dispatch_NoWorktreeWhenOff: worktree mode off leaves the
// session at the project root and emits no cwd message.
func TestCoordinator_Dispatch_NoWorktreeWhenOff(t *testing.T) {
	isolateHome(t)
	repo := initGitRepo(t)
	t.Chdir(repo)

	prov := newFakeProvider()
	prov.id = "fake-prov"
	sessCh := make(chan tea.Msg, 8)
	prov.startSessionFn = func(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
		proc := &providerProc{}
		sess := &agentSession{args: args, env: newAgentToolEnv(args.Cwd, args.TabID, true, func(msg tea.Msg) {}), ch: sessCh, sendCh: make(chan agentTurn, 8), closed: make(chan struct{})}
		proc.payload = sess
		sess.proc = proc
		return proc, sessCh, nil
	}
	withRegisteredProviders(t, prov)

	c := &Coordinator{sessions: map[int]*agentSession{}, workflowCancels: map[int]context.CancelFunc{}}
	args := ProviderSessionArgs{Cwd: repo, TabID: 1, Worktree: false, SkipAllPermissions: true}
	if err := c.Dispatch(1, prov, args, "hello", nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	prov.mu.Lock()
	got := prov.startArgs[0].Cwd
	prov.mu.Unlock()
	if got != repo {
		t.Errorf("worktree off must run at the project root; cwd = %q, want %q", got, repo)
	}
}

// workflowStepDoneSession is a fake StartSession for workflow-run tests:
// it records args (via the provider) and drives the run to a clean finish
// so RunWorkflow returns without a real agent loop.
func workflowStepDoneSession(args ProviderSessionArgs) (*providerProc, chan tea.Msg, error) {
	ch := make(chan tea.Msg, 8)
	proc := &providerProc{stdin: &bufferCloser{Buffer: nil}}
	proc.payload = &agentSession{
		args:   args,
		env:    newAgentToolEnv(args.Cwd, args.TabID, true, func(tea.Msg) {}),
		sendCh: make(chan agentTurn, 8),
		closed: make(chan struct{}),
	}
	go func() {
		ch <- providerDoneMsg{res: providerResult{Result: "ok"}}
		ch <- turnCompleteMsg{}
		close(ch)
	}()
	return proc, ch, nil
}

func workflowRunTestDef() workflowDef {
	return workflowDef{Name: "wf", Steps: []workflowStep{
		{Name: "s", Provider: "fake-prov", Model: "m", Prompt: "p"},
	}}
}

// TestCoordinator_RunWorkflow_ReusesTabWorktree: a workflow launched from a
// tab that already has a worktree runs *in that worktree*, not the project
// root — the regression that let a supplanting workflow write into main.
func TestCoordinator_RunWorkflow_ReusesTabWorktree(t *testing.T) {
	isolateHome(t)
	stubWorkflowStepModel(t)
	repo := initGitRepo(t)
	t.Chdir(repo)

	// The tab already has a worktree, as its first chat turn would create.
	wtPath, wtName, err := createWorktreeAt(repo)
	if err != nil {
		t.Fatalf("createWorktreeAt: %v", err)
	}

	prov := newFakeProvider()
	prov.id = "fake-prov"
	prov.startSessionFn = workflowStepDoneSession
	withRegisteredProviders(t, prov)

	c := &Coordinator{sessions: map[int]*agentSession{}, workflowCancels: map[int]context.CancelFunc{}}
	wt := workflowWorktree{root: repo, name: wtName, on: true}
	if _, err := c.RunWorkflow(context.Background(), 5, wt, workflowRunTestDef(), workflowSource{Kind: workflowSourceChat}); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	prov.mu.Lock()
	starts := append([]ProviderSessionArgs(nil), prov.startArgs...)
	prov.mu.Unlock()
	if len(starts) != 1 {
		t.Fatalf("StartSession called %d times, want 1", len(starts))
	}
	if starts[0].Cwd != wtPath {
		t.Errorf("workflow ran in %q, want the tab's worktree %q", starts[0].Cwd, wtPath)
	}
	if got := worktreeNameFromCwd(starts[0].Cwd); got != wtName {
		t.Errorf("workflow cwd worktree = %q, want %q", got, wtName)
	}
}

// TestCoordinator_RunWorkflow_CreatesWorktreeWhenTabHasNone: worktree mode on
// but the tab never created one (e.g. a workflow launched as the tab's first
// action) — the run makes a fresh worktree and announces it so the tab adopts
// it, instead of writing to the project root.
func TestCoordinator_RunWorkflow_CreatesWorktreeWhenTabHasNone(t *testing.T) {
	isolateHome(t)
	stubWorkflowStepModel(t)
	repo := initGitRepo(t)
	t.Chdir(repo)

	var emitted []providerCwdMsg
	prev := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool {
		if cm, ok := msg.(providerCwdMsg); ok {
			emitted = append(emitted, cm)
		}
		return true
	}
	t.Cleanup(func() { agentSendToProgram = prev })

	prov := newFakeProvider()
	prov.id = "fake-prov"
	prov.startSessionFn = workflowStepDoneSession
	withRegisteredProviders(t, prov)

	c := &Coordinator{sessions: map[int]*agentSession{}, workflowCancels: map[int]context.CancelFunc{}}
	wt := workflowWorktree{root: repo, on: true} // mode on, no worktree yet
	if _, err := c.RunWorkflow(context.Background(), 9, wt, workflowRunTestDef(), workflowSource{Kind: workflowSourceChat}); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	prov.mu.Lock()
	starts := append([]ProviderSessionArgs(nil), prov.startArgs...)
	prov.mu.Unlock()
	if len(starts) != 1 {
		t.Fatalf("StartSession called %d times, want 1", len(starts))
	}
	name := worktreeNameFromCwd(starts[0].Cwd)
	if name == "" {
		t.Fatalf("workflow ran at %q, not inside a worktree — the bug", starts[0].Cwd)
	}
	if fi, err := os.Stat(starts[0].Cwd); err != nil || !fi.IsDir() {
		t.Fatalf("worktree dir %q missing: %v", starts[0].Cwd, err)
	}
	if len(emitted) != 1 || worktreeNameFromCwd(emitted[0].cwd) != name {
		t.Errorf("expected one providerCwdMsg for worktree %q, got %+v", name, emitted)
	}
}

// TestCoordinator_RunWorkflow_NoWorktreeWhenOff: with worktree mode off the
// run stays at the project root and announces nothing.
func TestCoordinator_RunWorkflow_NoWorktreeWhenOff(t *testing.T) {
	isolateHome(t)
	stubWorkflowStepModel(t)
	repo := initGitRepo(t)
	t.Chdir(repo)

	var emitted int
	prev := agentSendToProgram
	agentSendToProgram = func(msg tea.Msg) bool {
		if _, ok := msg.(providerCwdMsg); ok {
			emitted++
		}
		return true
	}
	t.Cleanup(func() { agentSendToProgram = prev })

	prov := newFakeProvider()
	prov.id = "fake-prov"
	prov.startSessionFn = workflowStepDoneSession
	withRegisteredProviders(t, prov)

	c := &Coordinator{sessions: map[int]*agentSession{}, workflowCancels: map[int]context.CancelFunc{}}
	if _, err := c.RunWorkflow(context.Background(), 3, workflowWorktree{root: repo, on: false}, workflowRunTestDef(), workflowSource{Kind: workflowSourceChat}); err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}
	prov.mu.Lock()
	got := prov.startArgs[0].Cwd
	prov.mu.Unlock()
	if got != repo {
		t.Errorf("worktree off must run at the project root; cwd = %q, want %q", got, repo)
	}
	if emitted != 0 {
		t.Errorf("worktree off must announce no cwd; got %d providerCwdMsg", emitted)
	}
}
