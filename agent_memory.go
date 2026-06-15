package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/Cidan/memmy"
)

// agent_memory.go is the in-process twin of hook_memory.go: the claude
// CLI gets memory recall injected through HTTP hook callbacks
// (SessionStart / UserPromptSubmit / PreToolUse), and the fantasy
// agent sessions get the same three injections natively. All paths
// no-op instantly when the memory service is closed.

// memoryServiceOpened is a seam: tests swap it to a stub that
// pretends the memory service is open/closed without touching the
// real memmy singleton.
var memoryServiceOpened = memoryServiceOpen

// memoryRecallFn is a seam: tests swap it to inject canned hits
// (or errors) without spinning a real embedder.
var memoryRecallFn = memoryRecall

// agentMemorySystemBlock is the SessionStart twin: a coarse
// project-level recall computed once per session and appended to the
// system prompt. Computing it once keeps the prompt byte-stable for
// the session, which prefix caching depends on.
func agentMemorySystemBlock(cwd string) string {
	if !memoryServiceOpened() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), memoryHookCtxTimeout)
	defer cancel()
	hits, err := memoryRecallFn(ctx, cwd, "current project context", memoryRecallK)
	if err != nil {
		debugLog("agent memory (session start): %v", err)
		return ""
	}
	return formatRecallContext(hits, "Project memory")
}

// agentMemoryPromptContext is the UserPromptSubmit twin: per-turn
// semantic recall keyed on the user's prompt, appended to the wire
// prompt (and persisted with it, wire-true — same as claude's JSONL
// recording hook-injected context).
func agentMemoryPromptContext(cwd, prompt string) string {
	if !memoryServiceOpened() || strings.TrimSpace(prompt) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), memoryHookCtxTimeout)
	defer cancel()
	hits, err := memoryRecallFn(ctx, cwd, prompt, memoryRecallK)
	if err != nil {
		debugLog("agent memory (prompt): %v", err)
		return ""
	}
	return formatRecallContext(hits, "Relevant memory")
}

// memoryAwareTool is the PreToolUse twin: it decorates a file tool
// (read / edit / write) so the result carries a clearly-delimited
// memory footer for the touched path — prior work on the same file
// lands in context exactly when needed.
type memoryAwareTool struct {
	fantasy.AgentTool
	cwd string
}

// wrapFileToolsWithMemory decorates the file tools in place. Cheap
// when memory is closed: the wrapper checks per call.
func wrapFileToolsWithMemory(tools []fantasy.AgentTool, cwd string) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(tools))
	for i, t := range tools {
		switch t.Info().Name {
		case "read", "edit", "write":
			out[i] = &memoryAwareTool{AgentTool: t, cwd: cwd}
		default:
			out[i] = t
		}
	}
	return out
}

func (m *memoryAwareTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := m.AgentTool.Run(ctx, call)
	if err != nil || resp.IsError || resp.Type != "text" || !memoryServiceOpened() {
		return resp, err
	}
	path := fileToolPath(call.Input)
	if path == "" {
		return resp, err
	}
	recallCtx, cancel := context.WithTimeout(ctx, memoryHookCtxTimeout)
	defer cancel()
	hits, rerr := memoryRecallFn(recallCtx, m.cwd, path, memoryRecallK)
	if rerr != nil {
		debugLog("agent memory (file %s): %v", path, rerr)
		return resp, err
	}
	if block := formatRecallContext(hits, "Memory for "+path); block != "" {
		resp.Content = resp.Content + "\n\n" + block
	}
	return resp, err
}

// fileToolPath pulls file_path out of a file tool's JSON input.
func fileToolPath(input string) string {
	var in struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return ""
	}
	return strings.TrimSpace(in.FilePath)
}

// memoryRecallK caps the number of nodes injected into any single
// hook response. Five is the deliberate first-slice number — tight
// enough that the prompt isn't dominated by recall, generous enough
// that meaningfully-similar memory has room to surface.
const memoryRecallK = 5

// memoryHookCtxTimeout caps how long a hook can spend talking to
// memmy + the embedder. claude's default hook timeout is on the order
// of seconds; failing fast and returning empty additionalContext is
// always preferable to blocking the whole turn waiting for a remote
// embedder.
const memoryHookCtxTimeout = 8 * time.Second

// formatRecallContext produces the markdown block injected as
// additionalContext. heading is a short label ("Project memory",
// "Relevant memory", "Memory for /abs/path") so claude sees what
// kind of recall this was; the bullets list each hit's text with
// rank and score for transparency.
//
// Empty hits → empty string (claude sees no injection at all rather
// than a header with no content underneath).
func formatRecallContext(hits []memmy.RecallHit, heading string) string {
	// First pass: collect the text of every hit that would render,
	// dropping entries whose Text and SourceText are both blank. We
	// renumber from 1 in the second pass so a skipped hit doesn't
	// leave a gap in the rank the user sees.
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		text := strings.TrimSpace(h.Text)
		if text == "" {
			text = strings.TrimSpace(h.SourceText)
		}
		if text == "" {
			continue
		}
		lines = append(lines, text)
	}
	if len(lines) == 0 {
		// Doc: "Empty hits → empty string (claude sees no injection
		// at all rather than a header with no content underneath)."
		// We treat "no hits that would render text" the same as
		// "no hits at all" — both paths produce no additionalContext
		// block, no bare heading.
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", heading)
	for i, text := range lines {
		fmt.Fprintf(&b, "%d. %s\n", i+1, text)
	}
	return strings.TrimRight(b.String(), "\n")
}
