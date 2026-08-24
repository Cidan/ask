package main

import (
	"github.com/Cidan/ask/pkg/diff"
)

// The chat model is split into a source of truth (m.transcript) and a
// derived view (m.history). The transcript records every semantic event
// of the conversation, faithfully and without regard to view modes; the
// projection (projectHistory) walks it under the current quiet /
// tool-output / diff settings to produce the historyEntry list the
// viewport renders. Keeping the two apart is what makes the view modes
// non-destructive: nothing is ever dropped at arrival time, so a mode
// toggle re-projects the whole history and tool calls appear or vanish
// retroactively.

type transcriptKind int

const (
	trUser transcriptKind = iota
	trUserQueued
	trAssistant
	trToolCall
	trToolResult
	trDiff
	trPrerendered
	trWorkflowDone
)

// transcriptItem is one typed entry in the conversation's source of
// truth. Only the fields relevant to its kind are populated. Tool
// calls/results/diffs store their raw payload (not a prerendered string)
// so the projection can re-render them at the current tool-output mode —
// that is what lets the tool-output tri-state cycle retroactively over
// history that is already on screen.
type transcriptItem struct {
	kind transcriptKind

	// text carries the body for trUser, trUserQueued, trAssistant,
	// trPrerendered (already-styled), and trWorkflowDone.
	text string

	// trToolCall.
	toolName  string
	toolInput map[string]any

	// background applies to trToolCall and trToolResult: it mirrors the
	// tool_use's run_in_background flag so the projection can drop the
	// launch-ack result in short/off modes.
	background bool

	// trToolResult.
	output  string
	isError bool
	// exitCode/hasExitCode mirror toolResultMsg: a shell tool's process
	// exit code, present only for bash/job_output/job_kill results.
	exitCode    int
	hasExitCode bool

	// trDiff.
	filePath string
	hunks    []diff.Hunk

	// trWorkflowDone chrome.
	workflowHeader string
	workflowIndent int
}

// projectItem maps one transcript item to the historyEntry(ies) it
// contributes under the given view modes, or nil when the item is
// hidden. Only tool calls, tool results, and diffs are mode-filtered;
// user messages, assistant blocks, prerendered banners, and workflow
// summaries always render. The gates here are the single source of the
// visibility rules that used to live in shouldRenderToolCall /
// shouldRenderToolResult and the inline toolDiffMsg guard.
func projectItem(it transcriptItem, quiet bool, mode toolOutputMode, diffs bool) []historyEntry {
	switch it.kind {
	case trUser:
		return []historyEntry{{kind: histUser, text: it.text}}
	case trUserQueued:
		return []historyEntry{{kind: histUserQueued, text: it.text}}
	case trAssistant:
		return []historyEntry{{kind: histResponse, text: it.text}}
	case trPrerendered:
		return []historyEntry{{kind: histPrerendered, text: it.text}}
	case trWorkflowDone:
		return []historyEntry{{
			kind:           histWorkflowDone,
			text:           it.text,
			workflowHeader: it.workflowHeader,
			workflowIndent: it.workflowIndent,
		}}
	case trToolCall:
		if quiet || mode == toolOutputOff {
			return nil
		}
		return []historyEntry{{
			kind: histToolCall,
			text: renderToolCallBlock(it.toolName, it.toolInput, mode),
		}}
	case trToolResult:
		if quiet || mode == toolOutputOff {
			return nil
		}
		// Background results carry only the launch ack; hide them
		// unless the user asked for the full audit trail.
		if it.background && mode != toolOutputFull {
			return nil
		}
		body := renderToolResultBlock(it.toolName, it.output, it.isError, it.exitCode, it.hasExitCode, mode)
		// A tool whose result is pure noise (a read's file dump, an
		// edit's replacement count that the diff already shows) renders
		// nothing — the call header stands alone.
		if body == "" {
			return nil
		}
		return []historyEntry{{
			kind: histToolResult,
			text: body,
		}}
	case trDiff:
		if quiet || !diffs {
			return nil
		}
		// Structured, not prerendered: the diff re-renders at layout
		// width in ensureEntryWrapped so its backgrounds span the column.
		return []historyEntry{{
			kind:     histDiff,
			filePath: it.filePath,
			hunks:    it.hunks,
		}}
	}
	return nil
}

// projectTranscript maps a whole transcript to renderable history under
// the given view modes. It is the pure core shared by projectHistory and
// by replay/tests, so there is exactly one place that decides what a
// given transcript looks like in a given mode.
func projectTranscript(items []transcriptItem, quiet bool, mode toolOutputMode, diffs bool) []historyEntry {
	out := make([]historyEntry, 0, len(items))
	for i := range items {
		out = append(out, projectItem(items[i], quiet, mode, diffs)...)
	}
	return out
}

// projectHistory rebuilds m.history from m.transcript under the current
// view modes. It is the full-rebuild path used by mode toggles, session
// replay, and provider-swap — not a per-frame call. Rebuilding discards
// the per-entry glamour/wrap caches, so the visible window re-renders
// lazily on the next frame; that is acceptable for a deliberate toggle
// and never happens during plain streaming (which appends incrementally
// via pushTranscript). If a shell command is streaming, its history
// index is remapped to the shell item's new position.
func (m *model) projectHistory() {
	out := make([]historyEntry, 0, len(m.transcript))
	newShellIdx := -1
	for i := range m.transcript {
		if m.shellOutTrIdx >= 0 && i == m.shellOutTrIdx {
			newShellIdx = len(out)
		}
		out = append(out, projectItem(m.transcript[i], m.quietMode, m.toolOutputMode, m.renderDiffs)...)
	}
	m.history = out
	if m.shellOutIdx >= 0 {
		m.shellOutIdx = newShellIdx
	}
	m.lastContentFP = ""
}

// pushTranscript appends a new item to the source of truth and
// incrementally projects it onto m.history, leaving every existing
// entry (and its render cache) untouched. This is the arrival path:
// cheap, and identical in outcome to a full projectHistory over the
// same transcript.
func (m *model) pushTranscript(it transcriptItem) {
	m.transcript = append(m.transcript, it)
	m.history = append(m.history, projectItem(it, m.quietMode, m.toolOutputMode, m.renderDiffs)...)
}

// addAssistantDelta records a chunk of assistant text. Consecutive
// deltas coalesce into the open assistant block (responseActive); a tool
// call/result/diff between them clears responseActive, so the next delta
// starts a fresh block. Because the block boundary is structural, quiet
// and normal modes share this path — the only difference is that quiet
// hides the tool items in the projection, which is exactly why a
// preamble and the final answer no longer scrunch together.
func (m *model) addAssistantDelta(text string) {
	if m.responseActive && len(m.transcript) > 0 &&
		m.transcript[len(m.transcript)-1].kind == trAssistant &&
		len(m.history) > 0 && m.history[len(m.history)-1].kind == histResponse {
		m.transcript[len(m.transcript)-1].text += text
		last := &m.history[len(m.history)-1]
		last.text += text
		invalidateEntryRender(last)
		return
	}
	m.appendResponse(text)
	m.responseActive = true
}

// refreshHistory re-projects the transcript synchronously and relays
// out the viewport, preserving stick-to-bottom. It replaces the old
// async disk reload: because the transcript already lives in memory it
// works mid-turn and before the session is ever saved, and it uses the
// exact same projection as live streaming (so replay can never drift
// from the live view again).
func (m *model) refreshHistory() {
	m.projectHistory()
	m.layout()
}

// neutralTurnsFromTranscript distills the transcript to the provider-
// agnostic user/assistant turns used for cross-provider translation and
// re-materialization. Trailing user turns (a prompt with no reply yet)
// are trimmed so a half-finished turn is not carried across providers.
func neutralTurnsFromTranscript(items []transcriptItem) []NeutralTurn {
	out := make([]NeutralTurn, 0, len(items))
	for _, it := range items {
		switch it.kind {
		case trUser:
			out = append(out, NeutralTurn{Role: "user", Text: it.text})
		case trAssistant:
			out = append(out, NeutralTurn{Role: "assistant", Text: it.text})
		}
	}
	for len(out) > 0 && out[len(out)-1].Role == "user" {
		out = out[:len(out)-1]
	}
	return out
}
