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
	"github.com/Cidan/ask/pkg/providers"
	"github.com/Cidan/ask/pkg/workflow"
	"google.golang.org/adk/v2/agent"
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
	Cwd             string
	TabID           int
	SkipPermissions bool
	PlanningMode    bool
	IsSubagent      bool
	SubagentID      string

	Emit        engine.EventListener
	Interaction engine.InteractionHandler
	Files       *FileTracker
	Jobs        *JobManager

	// Custom approval function if overriding standard interaction handler.
	Approve func(ctx context.Context, toolName string, input map[string]any) (bool, error)

	// SupportsImages reports whether the active model can see images. It
	// gates feeding tool-rendered images (render_design, MCP image results,
	// read of an image file) back to the model as image parts. Nil means
	// unknown (treated as capable, so the images are still attached).
	SupportsImages func() bool

	// ImageSink is the per-session handoff for images a tool produces (a
	// render, an MCP image result) or opens (read of an image file). The
	// ImageInjectionHook drains it before each model call and feeds the
	// images to the model as inline image parts.
	ImageSink *ImageSink

	PendingEndTurn    *EndTurnSignal
	PendingFinishData *FinishWorkflowData

	// WorkflowRunner runs a workflow definition when selected in finalized_plan.
	WorkflowRunner func(ctx context.Context, tabID int, def workflow.Def, src any) (string, error)

	// Steering, when set, tells a tool blocked on a long wait that the user
	// sent the running turn a message, so it returns and the next model call
	// takes the message: job_output stops waiting, and a foreground bash
	// command or sub-agent is handed off to a background job.
	Steering SteerSignaler

	// Tools is the session's own tool set — wire tools and registry, as the
	// model calls them — which decide's inner calls run through. Set by
	// whatever assembles the session's tools.
	Tools ToolLookup

	nudgeMu sync.Mutex
	nudges  map[string]int

	decisionMu   sync.Mutex
	decisionSeq  int
	decisionSeen bool
}

// ToolLookup finds one of a session's tools by name.
type ToolLookup interface {
	LookupTool(name string) (Tool, bool)
}

// SessionTools is the ToolLookup over a session's wire tools and registry,
// both read at lookup time so it can be built before the lists are final.
type SessionTools struct {
	Core     func() []Tool
	Registry func() []Tool
}

func (s SessionTools) LookupTool(name string) (Tool, bool) {
	for _, list := range []func() []Tool{s.Core, s.Registry} {
		if list == nil {
			continue
		}
		for _, t := range list() {
			if t != nil && t.Name() == name {
				return t, true
			}
		}
	}
	return nil, false
}

// nextDecisionID allocates the session's next decision id (d1, d2, …),
// continuing from the records a resumed session already holds.
func (e *ToolEnv) nextDecisionID(ctx agent.Context) string {
	e.decisionMu.Lock()
	defer e.decisionMu.Unlock()
	if !e.decisionSeen {
		e.decisionSeq = engine.LastDecisionIndex(ctx.State())
		e.decisionSeen = true
	}
	e.decisionSeq++
	return fmt.Sprintf("d%d", e.decisionSeq)
}

// SteerSignaler hands out a channel that is closed once the running turn has
// user input waiting for its next model call. engine.TurnGate is one.
type SteerSignaler interface {
	SteerSignal() <-chan struct{}
}

func (e *ToolEnv) steerSignal() <-chan struct{} {
	if e.Steering == nil {
		return nil
	}
	return e.Steering.SteerSignal()
}

// shouldNudge counts one more call that earns the nudge named kind and
// reports whether this one carries it: the first three of a session, then
// every tenth, so a habit gets corrected without the reminder filling the
// context.
func (e *ToolEnv) shouldNudge(kind string) bool {
	e.nudgeMu.Lock()
	defer e.nudgeMu.Unlock()
	if e.nudges == nil {
		e.nudges = map[string]int{}
	}
	e.nudges[kind]++
	n := e.nudges[kind]
	return n <= 3 || n%10 == 0
}

// NewToolEnv constructs a ToolEnv for a session.
func NewToolEnv(cwd string, tabID int, skipPermissions bool, emit engine.EventListener, interaction engine.InteractionHandler) *ToolEnv {
	if interaction == nil {
		interaction = engine.HeadlessInteractionHandler{AutoApproveTools: skipPermissions}
	}
	env := &ToolEnv{
		Cwd:             cwd,
		TabID:           tabID,
		SkipPermissions: skipPermissions,
		Emit:            emit,
		Interaction:     interaction,
		Files:           NewFileTracker(),
		Jobs:            NewJobManager(),
	}
	env.Approve = env.approveViaInteraction
	return env
}

// NewSubagentToolEnv constructs an isolated ToolEnv for a subagent execution.
func NewSubagentToolEnv(parent *ToolEnv, subagentID string) *ToolEnv {
	if parent == nil {
		env := NewToolEnv(".", 0, true, nil, nil)
		env.IsSubagent = true
		env.SubagentID = subagentID
		return env
	}
	env := &ToolEnv{
		Cwd:             parent.Cwd,
		TabID:           parent.TabID,
		SkipPermissions: true,
		PlanningMode:    false,
		IsSubagent:      true,
		SubagentID:      subagentID,
		Emit:            parent.Emit,
		Interaction:     parent.Interaction,
		Files:           parent.Files,
		Jobs:            parent.Jobs,
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

// ApprovalDenied checks permission before a mutating tool runs. It
// returns the message to put in the tool result's Error field, or "" when
// the call is allowed.
func (env *ToolEnv) ApprovalDenied(ctx context.Context, toolName string, input map[string]any) string {
	if env == nil || env.SkipPermissions {
		return ""
	}
	approveFn := env.Approve
	if approveFn == nil {
		approveFn = env.approveViaInteraction
	}
	ok, err := approveFn(ctx, toolName, input)
	switch {
	case err != nil:
		return "permission check failed: " + err.Error()
	case !ok:
		return "The user denied permission for this tool call. Do not retry it; either proceed without it or end your turn and explain what you need."
	}
	return ""
}

// SpendDecision is the spend source of decision model calls.
const SpendDecision = "decision"

// RecordSpend reports a call a tool made on the session's behalf outside the
// session's own model calls, so it reaches the cost meter and the session's
// usage ledger.
func (env *ToolEnv) RecordSpend(source string, u providers.Usage) {
	if env == nil || env.Emit == nil || (u.InputTokens == 0 && !u.CostKnown()) {
		return
	}
	env.Emit(engine.SpendEvent{BaseEvent: engine.BaseEvent{TabID: env.TabID}, Source: source, Usage: u})
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
