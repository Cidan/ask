package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/diff"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/workflow"
)

const (
	MaxToolOutput  = 30_000
	MaxLineLength  = 2_000
	MaxReadLines   = 2_000
	MaxReadBytes   = 200_000
	MaxSearchHits  = 100
	MaxListEntries = 1_000

	ToolPhraseFieldDoc = "one short human-readable phrase (under 10 words) telling the user what this call is doing"
)

// ImageExts are rejected by tools because text-only models cannot process raw images.
var ImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".bmp": true, ".ico": true, ".tiff": true,
}

// EndTurnSignal carries loop decision data emitted by the end_turn tool.
type EndTurnSignal struct {
	Decision string
	Summary  string
}

// FinishWorkflowData carries the final outcome data emitted by the finish_workflow tool.
type FinishWorkflowData struct {
	Description string
	Artifacts   []string
}

// ToolEnv is the per-session execution environment shared by all harness tools.
type ToolEnv struct {
	Cwd                   string
	TabID                 int
	SkipPermissions       bool
	GateTodosBeforeMutate bool
	PlanningMode          bool
	IsSubagent            bool
	SubagentID            string

	Emit        engine.EventListener
	Interaction engine.InteractionHandler
	Files       *FileTracker
	Jobs        *JobManager

	// Custom approval function if overriding standard interaction handler.
	Approve func(ctx context.Context, toolName string, input map[string]any) (bool, error)

	wfMu                  sync.Mutex
	WorkflowsAvailable    bool
	WorkflowsChecked      bool
	WorkflowRunDispatched bool
	WorkflowGuardFired    bool
	DecisionGuardFired    bool
	TodosApplied          bool

	PendingEndTurn    *EndTurnSignal
	PendingFinishData *FinishWorkflowData

	// WorkflowRunner runs a workflow definition when selected in finalized_plan.
	WorkflowRunner func(ctx context.Context, tabID int, def workflow.Def, src any) (string, error)
}

// NewToolEnv constructs a ToolEnv for a session.
func NewToolEnv(cwd string, tabID int, skipPermissions bool, gateTodosBeforeMutate bool, emit engine.EventListener, interaction engine.InteractionHandler) *ToolEnv {
	if interaction == nil {
		interaction = engine.HeadlessInteractionHandler{AutoApproveTools: skipPermissions}
	}
	env := &ToolEnv{
		Cwd:                   cwd,
		TabID:                 tabID,
		SkipPermissions:       skipPermissions,
		GateTodosBeforeMutate: gateTodosBeforeMutate,
		Emit:                  emit,
		Interaction:           interaction,
		Files:                 NewFileTracker(),
		Jobs:                  NewJobManager(),
		WorkflowsAvailable:    len(workflow.ListAll(cwd)) > 0,
	}
	env.Approve = env.approveViaInteraction
	return env
}

// NewSubagentToolEnv constructs an isolated ToolEnv for a subagent execution.
func NewSubagentToolEnv(parent *ToolEnv, subagentID string) *ToolEnv {
	if parent == nil {
		env := NewToolEnv(".", 0, true, false, nil, nil)
		env.IsSubagent = true
		env.SubagentID = subagentID
		return env
	}
	env := &ToolEnv{
		Cwd:                   parent.Cwd,
		TabID:                 parent.TabID,
		SkipPermissions:       true,
		GateTodosBeforeMutate: false,
		PlanningMode:          false,
		IsSubagent:            true,
		SubagentID:            subagentID,
		Emit:                  parent.Emit,
		Interaction:           parent.Interaction,
		Files:                 parent.Files,
		Jobs:                  parent.Jobs,
		WorkflowsAvailable:    parent.WorkflowsAvailable,
	}
	env.Approve = parent.Approve
	if env.Approve == nil {
		env.Approve = env.approveViaInteraction
	}
	return env
}

func (env *ToolEnv) approveViaInteraction(ctx context.Context, toolName string, input map[string]any) (bool, error) {
	if env.Interaction != nil {
		resp, err := env.Interaction.RequestApproval(ctx, env.TabID, engine.ApprovalRequest{
			ToolName: toolName,
			Input:    input,
		})
		if err != nil {
			return false, err
		}
		return resp.Allow, nil
	}
	return false, fmt.Errorf("approval required for %s but no interaction handler is configured", toolName)
}

// RequestApproval checks permission before executing a mutating tool.
func (env *ToolEnv) RequestApproval(ctx context.Context, toolName string, input map[string]any) *ToolResponse {
	if env == nil || env.SkipPermissions {
		return nil
	}
	approveFn := env.Approve
	if approveFn == nil {
		approveFn = env.approveViaInteraction
	}
	ok, err := approveFn(ctx, toolName, input)
	if err != nil {
		resp := NewTextErrorResponse("permission check failed: " + err.Error())
		resp.StopTurn = true
		return &resp
	}
	if !ok {
		resp := NewTextErrorResponse("The user denied permission for this tool call. Do not retry it; either proceed without it or end your turn and explain what you need.")
		resp.StopTurn = true
		return &resp
	}
	return nil
}

// AbsPath resolves a relative or absolute path against the session Cwd.
func (env *ToolEnv) AbsPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return env.Cwd
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(env.Cwd, p)
}

// MarkWorkflowsChecked disarms the workflow check guard.
func (env *ToolEnv) MarkWorkflowsChecked() {
	if env == nil {
		return
	}
	env.wfMu.Lock()
	env.WorkflowsChecked = true
	env.wfMu.Unlock()
}

// MarkWorkflowRunDispatched records that a workflow has been launched.
func (env *ToolEnv) MarkWorkflowRunDispatched() {
	if env == nil {
		return
	}
	env.wfMu.Lock()
	env.WorkflowRunDispatched = true
	env.wfMu.Unlock()
}

// WorkflowGuardShouldFire reports whether the workflow check guard should fire.
func (env *ToolEnv) WorkflowGuardShouldFire() bool {
	if env == nil {
		return false
	}
	env.wfMu.Lock()
	defer env.wfMu.Unlock()
	if !env.WorkflowsAvailable || env.WorkflowsChecked || env.WorkflowGuardFired {
		return false
	}
	env.WorkflowGuardFired = true
	return true
}

// WorkflowDecisionGuardShouldFire reports whether the workflow decision guard should fire.
func (env *ToolEnv) WorkflowDecisionGuardShouldFire() bool {
	if env == nil {
		return false
	}
	env.wfMu.Lock()
	defer env.wfMu.Unlock()
	if !env.WorkflowsAvailable || !env.WorkflowsChecked || env.WorkflowRunDispatched || env.DecisionGuardFired {
		return false
	}
	env.DecisionGuardFired = true
	return true
}

// WorkflowGuardNotice returns the steering notice for the workflow guard if it should fire.
func (env *ToolEnv) WorkflowGuardNotice() string {
	if env == nil || !env.GateTodosBeforeMutate {
		return ""
	}
	if env.WorkflowGuardShouldFire() {
		return WorkflowGuardTodosNotice
	}
	if env.WorkflowDecisionGuardShouldFire() {
		return WorkflowDecisionGuardNotice
	}
	return ""
}

// MarkTodosApplied records that a task list has been successfully applied.
func (env *ToolEnv) MarkTodosApplied() {
	if env == nil {
		return
	}
	env.wfMu.Lock()
	env.TodosApplied = true
	env.wfMu.Unlock()
}

// RequireTodosNotice returns the notice requiring a task list before file modifications.
func (env *ToolEnv) RequireTodosNotice() string {
	if env == nil || !env.GateTodosBeforeMutate {
		return ""
	}
	env.wfMu.Lock()
	applied := env.TodosApplied
	env.wfMu.Unlock()
	if applied {
		return ""
	}
	return RequireTodosBeforeMutateNotice
}

// CheckReadBeforeMutate enforces read-before-edit semantics on existing files.
func (env *ToolEnv) CheckReadBeforeMutate(path string, modTime time.Time) string {
	if env == nil || env.Files == nil {
		return ""
	}
	last := env.Files.LastRead(path)
	if last.IsZero() {
		return "you must read " + path + " with the read tool before modifying it"
	}
	if modTime.After(last) {
		return path + " has changed on disk since you last read it — read it again before modifying"
	}
	return ""
}

// EmitFileDiff computes a unified diff and publishes a ToolDiffEvent.
func (env *ToolEnv) EmitFileDiff(path, oldBody, newBody string) {
	if env == nil || env.Emit == nil {
		return
	}
	d := diff.Unified(oldBody, newBody)
	if d == "" {
		return
	}
	env.Emit(engine.ToolDiffEvent{
		BaseEvent: engine.BaseEvent{TabID: env.TabID},
		Path:      path,
		Diff:      d,
	})
}

// FileTracker tracks when files were last read to enforce read-before-modify rules.
type FileTracker struct {
	mu   sync.Mutex
	read map[string]time.Time
}

func NewFileTracker() *FileTracker {
	return &FileTracker{read: make(map[string]time.Time)}
}

func (t *FileTracker) RecordRead(path string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.read[path] = time.Now()
}

func (t *FileTracker) LastRead(path string) time.Time {
	if t == nil {
		return time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.read[path]
}

// TruncateMiddle truncates a long string keeping head and tail.
func TruncateMiddle(s string) string {
	if len(s) <= MaxToolOutput {
		return s
	}
	half := MaxToolOutput / 2
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

// TruncateLine truncates a single line at MaxLineLength.
func TruncateLine(s string) string {
	if len(s) <= MaxLineLength {
		return s
	}
	return s[:MaxLineLength] + "…"
}

// LooksBinary detects binary files via NUL bytes in the header.
func LooksBinary(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}
