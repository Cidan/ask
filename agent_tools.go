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

	// approve gates a mutating tool call behind the ask approval modal.
	// Overridable so tests can script decisions without a tea.Program.
	approve func(ctx context.Context, toolName string, input map[string]any) (bool, error)
}

func newAgentToolEnv(cwd string, tabID int, skipPermissions bool, emit func(tea.Msg)) *agentToolEnv {
	env := &agentToolEnv{
		cwd:             cwd,
		tabID:           tabID,
		skipPermissions: skipPermissions,
		emit:            emit,
		files:           newAgentFileTracker(),
		jobs:            newAgentJobManager(),
	}
	env.approve = env.approveViaModal
	return env
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
