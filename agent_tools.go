package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

// agentToolEnv is the per-session state shared by every harness tool.
// One env is created per agent session (agent_run.go); tools close over
// it. emit pushes provider-protocol messages (toolDiffMsg, todo
// updates) onto the session's stream channel; ask/approval/end_turn
// requests instead go through teaProgramPtr like the MCP bridge does,
// because they are routed by tabID rather than by proc identity.
type agentToolEnv struct {
	cwd             string
	tabID           int
	skipPermissions bool
	emit            func(tea.Msg)
	files           *agentFileTracker
	jobs            *agentJobManager

	// gateTodosBeforeMutate gates the two guards below: when true (opt-in),
	// the todos tool enforces the workflow check and the write/edit tools
	// refuse to mutate until a task list has been applied. When false
	// (default), both guards are inert — the model can write/edit without
	// calling todos and the workflow guard never fires.
	gateTodosBeforeMutate bool

	// approve gates a mutating tool call behind the ask approval modal.
	// Overridable so tests can script decisions without a tea.Program.
	approve func(ctx context.Context, toolName string, input map[string]any) (bool, error)

	// Workflow guard: a two-stage, in-process checkpoint that forces the
	// model to consult the project's workflows before it starts tracking
	// multi-step work, AND to reconcile the workflow decision with the
	// user instead of silently proceeding inline. workflowsAvailable is
	// computed once at session start (true only when the project or user
	// scope actually defines a workflow). workflowsChecked flips when
	// the model invokes workflow_list; workflowRunDispatched flips when
	// a workflow run is dispatched. workflowGuardFired and decisionGuardFired
	// ensure the todos tool punts the model back at most once per stage,
	// so a model that legitimately proceeds inline is never blocked more
	// than these two checkpoints. The disarms now live in the
	// workflow_list core tool closure
	// (agent_tools_workflow.go) — when those tools were in the deferred
	// registry the disarms fired inside agentInvokeTool.Run, but
	// promoting them to the core wire toolset moved the hooks next to
	// the calls they guard. See the guards in agentTodosTool.
	wfMu                  sync.Mutex
	workflowsAvailable    bool
	workflowsChecked      bool
	workflowRunDispatched bool
	workflowGuardFired    bool
	decisionGuardFired    bool

	// todosApplied flips true the first time a todos call successfully
	// applies a task list this session. When gateTodosBeforeMutate is
	// true (opt-in), the write/edit tools refuse to mutate until it is
	// set (see requireTodosNotice), making the todos call a mandatory
	// chokepoint before any code change and guaranteeing the workflow
	// guard (which lives inside todos) is reached before the model starts
	// editing. When the gate is false (default), this flag is unused.
	// Guarded by wfMu.
	todosApplied bool
}

func newAgentToolEnv(cwd string, tabID int, skipPermissions bool, gateTodosBeforeMutate bool, emit func(tea.Msg)) *agentToolEnv {
	env := &agentToolEnv{
		cwd:                   cwd,
		tabID:                 tabID,
		skipPermissions:       skipPermissions,
		gateTodosBeforeMutate: gateTodosBeforeMutate,
		emit:                  emit,
		files:                 newAgentFileTracker(),
		jobs:                  newAgentJobManager(),
		workflowsAvailable:    len(listAllWorkflows(cwd)) > 0,
	}
	env.approve = env.approveViaModal
	return env
}

// markWorkflowsChecked disarms the workflow guard: once the model has
// invoked workflow_list, the todos tool stops punting it back. Called
// from the workflow_list core tool closure
// (agent_tools_workflow.go).
func (env *agentToolEnv) markWorkflowsChecked() {
	env.wfMu.Lock()
	env.workflowsChecked = true
	env.wfMu.Unlock()
}

// markWorkflowRunDispatched records that the model actually launched a
// workflow. This permanently satisfies the decision
// guard: a model that ran a workflow is never punted back to reconcile
// an inline decision.
func (env *agentToolEnv) markWorkflowRunDispatched() {
	env.wfMu.Lock()
	env.workflowRunDispatched = true
	env.wfMu.Unlock()
}

// workflowGuardShouldFire reports whether the todos tool should reject
// this call and steer the model to workflow_list first. It fires at
// most once per session and only when the project actually has
// workflows the model hasn't yet looked at. The first call that
// returns true latches workflowGuardFired so a later todos call always
// proceeds.
func (env *agentToolEnv) workflowGuardShouldFire() bool {
	env.wfMu.Lock()
	defer env.wfMu.Unlock()
	if !env.workflowsAvailable || env.workflowsChecked || env.workflowGuardFired {
		return false
	}
	env.workflowGuardFired = true
	return true
}

// workflowDecisionGuardShouldFire reports whether the todos tool should
// reject this call and steer the model to reconcile its workflow
// decision with the user. It is the second-stage guard: it fires only
// after the first guard is satisfied (the model has looked at the
// workflows, workflowsChecked == true) but the model is now starting
// inline work without ever having proposed/run a workflow — the exact
// failure where a weak model asks the user, gets a yes, then proceeds
// inline anyway. It fires at most once per session and never when a
// workflow was actually run. The first call that returns true latches
// decisionGuardFired so a later todos call always proceeds.
func (env *agentToolEnv) workflowDecisionGuardShouldFire() bool {
	env.wfMu.Lock()
	defer env.wfMu.Unlock()
	if !env.workflowsAvailable || !env.workflowsChecked || env.workflowRunDispatched || env.decisionGuardFired {
		return false
	}
	env.decisionGuardFired = true
	return true
}

// workflowGuardNotice runs the two-stage workflow guard and returns the
// steering notice the calling tool should return INSTEAD of doing its
// work, or "" when the call may proceed. It is called only from the
// todos tool. When gateTodosBeforeMutate is true (opt-in), write/edit
// refuse to mutate before a todos call has applied (see
// requireTodosNotice), so the model cannot reach an edit without first
// passing through this guard. Inert when env is nil, the project defines
// no workflows, or the gate is false (default).
func (env *agentToolEnv) workflowGuardNotice() string {
	if env == nil {
		return ""
	}
	if !env.gateTodosBeforeMutate {
		return ""
	}
	if env.workflowGuardShouldFire() {
		return workflowGuardTodosNotice
	}
	if env.workflowDecisionGuardShouldFire() {
		return workflowDecisionGuardNotice
	}
	return ""
}

// markTodosApplied records that a todos call applied a task list this
// session, satisfying the require-todos gate on write/edit.
func (env *agentToolEnv) markTodosApplied() {
	env.wfMu.Lock()
	env.todosApplied = true
	env.wfMu.Unlock()
}

// requireTodosNotice returns the steering notice a mutating tool
// (write/edit) should return INSTEAD of mutating when no todos call has
// applied a task list yet this session, or "" once one has. When
// gateTodosBeforeMutate is true (opt-in), this makes the todos call a
// mandatory precondition for any code change: the user always gets a
// live task list, and the workflow guard inside todos is reached before
// the model starts editing. Inert when env is nil or the gate is false
// (default).
func (env *agentToolEnv) requireTodosNotice() string {
	if env == nil {
		return ""
	}
	if !env.gateTodosBeforeMutate {
		return ""
	}
	env.wfMu.Lock()
	applied := env.todosApplied
	env.wfMu.Unlock()
	if applied {
		return ""
	}
	return requireTodosBeforeMutateNotice
}

// approveViaModal is the production approval path: route an
// approvalRequestMsg to the owning tab (same wire the MCP
// permission-prompt uses) and block until the user answers. Sessions
// with permissions skipped never get here — callers check
// skipPermissions through requestApproval.
func (env *agentToolEnv) approveViaModal(ctx context.Context, toolName string, input map[string]any) (bool, error) {
	p := teaProgramPtr.Load()
	if p == nil {
		return false, fmt.Errorf("approval required for %s but no UI is available", toolName)
	}
	reply := make(chan approvalReply, 1)
	p.Send(approvalRequestMsg{
		tabID:    env.tabID,
		toolName: toolName,
		input:    input,
		reply:    reply,
	})
	select {
	case r := <-reply:
		return r.allow, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// requestApproval is the gate mutating tools call before acting.
// Returns nil to proceed. A denial (or an approval-channel failure)
// comes back as a ToolResponse the tool returns verbatim: the model
// sees the denial and — per StopTurn — does not get another tool call
// this turn, mirroring crush's permission semantics.
func (env *agentToolEnv) requestApproval(ctx context.Context, toolName string, input map[string]any) *fantasy.ToolResponse {
	if env.skipPermissions {
		return nil
	}
	ok, err := env.approve(ctx, toolName, input)
	if err != nil {
		resp := fantasy.NewTextErrorResponse("permission check failed: " + err.Error())
		resp.StopTurn = true
		return &resp
	}
	if !ok {
		resp := fantasy.NewTextErrorResponse("The user denied permission for this tool call. Do not retry it; either proceed without it or end your turn and explain what you need.")
		resp.StopTurn = true
		return &resp
	}
	return nil
}

// absPath resolves a model-supplied path against the session cwd.
func (env *agentToolEnv) absPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return env.cwd
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(env.cwd, p)
}

// agentFileTracker records when each file was last read so edit/write
// can enforce read-before-edit and detect concurrent modification.
type agentFileTracker struct {
	mu   sync.Mutex
	read map[string]time.Time
}

func newAgentFileTracker() *agentFileTracker {
	return &agentFileTracker{read: map[string]time.Time{}}
}

func (t *agentFileTracker) recordRead(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.read[path] = time.Now()
}

func (t *agentFileTracker) lastRead(path string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.read[path]
}

// Output bounds shared by the harness tools. Middle-out truncation
// keeps both the head (command banner, first error) and the tail (the
// part the model usually needs) of oversized output.
const (
	agentMaxToolOutput  = 30_000
	agentMaxLineLength  = 2_000
	agentMaxReadLines   = 2_000
	agentMaxReadBytes   = 200_000
	agentMaxSearchHits  = 100
	agentMaxListEntries = 1_000
)

// truncateMiddle caps s at agentMaxToolOutput chars by cutting the
// middle on line boundaries where possible.
func truncateMiddle(s string) string {
	if len(s) <= agentMaxToolOutput {
		return s
	}
	half := agentMaxToolOutput / 2
	head := s[:half]
	tail := s[len(s)-half:]
	if i := strings.LastIndexByte(head, '\n'); i > 0 {
		head = head[:i+1]
	}
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	cut := strings.Count(s, "\n") - strings.Count(head, "\n") - strings.Count(tail, "\n")
	return fmt.Sprintf("%s… [%d lines truncated] …\n%s", head, cut, tail)
}

// truncateLine caps one line at agentMaxLineLength chars.
func truncateLine(s string) string {
	if len(s) <= agentMaxLineLength {
		return s
	}
	return s[:agentMaxLineLength] + "…"
}

// looksBinary reports whether the head of a file smells like binary
// content (NUL byte heuristic, same one git uses).
func looksBinary(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

// agentImageExts are rejected outright: deepseek models do not accept
// image input, so reading one can never help the agent.
var agentImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".bmp": true, ".ico": true, ".tiff": true,
}
