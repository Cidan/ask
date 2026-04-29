package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Cidan/memmy"
)

// Memory hooks that run synchronously inside ask's per-tab mcpBridge.
// Each handler:
//   1. Decodes the claude hook payload from the request body.
//   2. (For subagent contexts) skips work — sub-agents share the
//      parent's tenant, but we only inject/capture at parent
//      boundaries (DESIGN docs / sub-agent reasoning).
//   3. Calls memmy in-process via memoryRecall.
//   4. Writes a hookSpecificOutput JSON response that claude reads
//      from the hook subprocess's stdout, injecting it as
//      additionalContext into the model's prompt.
//
// Capture (the Stop / PostToolUse side) is NOT done via hooks; ask
// already sees every assistant message and tool call in its stream
// layer (`update.go`), so writes are issued at turn end from the
// bubbletea goroutine. This file is read-only on the model — every
// hook handler returns immediately and never blocks on tea.Msg
// round-trips.

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

// sessionStartHookInput is the subset of claude's SessionStart payload
// we care about. The full schema is large (transcript_path, model,
// agent_type, source, etc.); we read source so we can shape recall
// queries differently for startup vs compact, and ignore the rest.
type sessionStartHookInput struct {
	Source string `json:"source"`
}

// userPromptSubmitHookInput carries the prompt the user just typed,
// which is the query memmy embeds for its semantic recall. We also
// look at session_id only as a (currently unused) routing hint.
type userPromptSubmitHookInput struct {
	Prompt string `json:"prompt"`
}

// preToolUseHookInput is the structurally-narrow shape we need from
// claude's PreToolUse hook. tool_input is left as a generic map
// because each tool has its own schema; we only pull file_path
// (Read/Edit/Write) and notebook_path (Notebook tools).
type preToolUseHookInput struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	AgentID   string         `json:"agent_id"`
}

// hookContextResponse is the shape claude reads from a hook
// subprocess's stdout to inject text into the next prompt. The
// hookEventName must match the firing event verbatim or claude
// silently ignores it.
type hookContextResponse struct {
	HookSpecificOutput hookContextOutput `json:"hookSpecificOutput"`
}

type hookContextOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// writeHookContextResponse serializes a hookContextResponse to the
// HTTP response writer. Empty additionalContext yields a valid no-op
// response that claude treats as "nothing to inject."
func writeHookContextResponse(w http.ResponseWriter, eventName, additionalContext string) {
	body, err := json.Marshal(hookContextResponse{
		HookSpecificOutput: hookContextOutput{
			HookEventName:     eventName,
			AdditionalContext: additionalContext,
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// handleHookSessionStart injects a coarse project-level recall when a
// new session begins (startup) or after a context reset (clear /
// compact). The query string is intentionally generic — "current
// project context" — so memmy returns the most-weighted nodes for
// this {project, scope} tenant rather than a query-specific subset.
func (b *mcpBridge) handleHookSessionStart(w http.ResponseWriter, r *http.Request) {
	var ev sessionStartHookInput
	_ = json.NewDecoder(r.Body).Decode(&ev) // best-effort; fields are optional
	debugLog("hook SessionStart tab=%d source=%s", b.tabID, ev.Source)
	cwd := b.getCwd()
	ctx, cancel := context.WithTimeout(r.Context(), memoryHookCtxTimeout)
	defer cancel()
	hits, err := memoryRecall(ctx, cwd, "current project context", memoryRecallK)
	if err != nil {
		debugLog("memory recall (SessionStart): %v", err)
		writeHookContextResponse(w, "SessionStart", "")
		return
	}
	writeHookContextResponse(w, "SessionStart", formatRecallContext(hits, "Project memory"))
}

// handleHookUserPromptSubmit performs the per-prompt semantic recall
// that gives memory its biggest practical payoff: every user prompt
// gets re-keyed against the corpus, and the top-K most similar
// observations are injected.
func (b *mcpBridge) handleHookUserPromptSubmit(w http.ResponseWriter, r *http.Request) {
	var ev userPromptSubmitHookInput
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		debugLog("hook UserPromptSubmit decode: %v", err)
		writeHookContextResponse(w, "UserPromptSubmit", "")
		return
	}
	prompt := strings.TrimSpace(ev.Prompt)
	if prompt == "" {
		writeHookContextResponse(w, "UserPromptSubmit", "")
		return
	}
	debugLog("hook UserPromptSubmit tab=%d len=%d", b.tabID, len(prompt))
	cwd := b.getCwd()
	ctx, cancel := context.WithTimeout(r.Context(), memoryHookCtxTimeout)
	defer cancel()
	hits, err := memoryRecall(ctx, cwd, prompt, memoryRecallK)
	if err != nil {
		debugLog("memory recall (UserPromptSubmit): %v", err)
		writeHookContextResponse(w, "UserPromptSubmit", "")
		return
	}
	writeHookContextResponse(w, "UserPromptSubmit", formatRecallContext(hits, "Relevant memory"))
}

// handleHookPreToolUse fires before claude calls a file-touching tool
// (Read / Edit / Write — the matcher in claudeHookSettings). The hook
// pulls memory specific to that file path, so prior work on the same
// file lands in claude's context exactly when needed. Sub-agents
// share the parent's tenant, so we let recall fire here regardless
// of agent_id (cheap, no harm).
func (b *mcpBridge) handleHookPreToolUse(w http.ResponseWriter, r *http.Request) {
	var ev preToolUseHookInput
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		debugLog("hook PreToolUse decode: %v", err)
		writeHookContextResponse(w, "PreToolUse", "")
		return
	}
	path := preToolUseFilePath(ev)
	if path == "" {
		writeHookContextResponse(w, "PreToolUse", "")
		return
	}
	debugLog("hook PreToolUse tab=%d tool=%s file=%s", b.tabID, ev.ToolName, path)
	cwd := b.getCwd()
	ctx, cancel := context.WithTimeout(r.Context(), memoryHookCtxTimeout)
	defer cancel()
	// Query the file path directly — memmy's embedder will tokenize
	// the path and surface observations that mention it.
	hits, err := memoryRecall(ctx, cwd, path, memoryRecallK)
	if err != nil {
		debugLog("memory recall (PreToolUse): %v", err)
		writeHookContextResponse(w, "PreToolUse", "")
		return
	}
	heading := "Memory for " + path
	writeHookContextResponse(w, "PreToolUse", formatRecallContext(hits, heading))
}

// preToolUseFilePath extracts the file path from a tool's input map,
// covering Read/Edit/Write/MultiEdit (file_path) and notebook tools
// (notebook_path). Returns "" when the tool doesn't operate on a
// file or the field is missing.
func preToolUseFilePath(ev preToolUseHookInput) string {
	if ev.ToolInput == nil {
		return ""
	}
	switch ev.ToolName {
	case "Read", "Edit", "Write", "MultiEdit":
		if p, _ := ev.ToolInput["file_path"].(string); p != "" {
			return p
		}
	case "NotebookEdit":
		if p, _ := ev.ToolInput["notebook_path"].(string); p != "" {
			return p
		}
	}
	return ""
}

// formatRecallContext produces the markdown block injected as
// additionalContext. heading is a short label ("Project memory",
// "Relevant memory", "Memory for /abs/path") so claude sees what
// kind of recall this was; the bullets list each hit's text with
// rank and score for transparency.
//
// Empty hits → empty string (claude sees no injection at all rather
// than a header with no content underneath).
func formatRecallContext(hits []memmy.RecallHit, heading string) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", heading)
	for i, h := range hits {
		text := strings.TrimSpace(h.Text)
		if text == "" {
			text = strings.TrimSpace(h.SourceText)
		}
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, text)
	}
	return strings.TrimRight(b.String(), "\n")
}
