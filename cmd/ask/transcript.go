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

	// inflight/errored are trToolCall runtime state for the ▸ glyph pulse.
	// inflight is set the moment the call is announced and cleared by
	// settleToolCall when its result arrives; errored records whether that
	// result was an error so a re-projection restores the resting glyph
	// color. Both are always false for replayed/saved transcripts (every
	// recorded call is already complete), so storing them here is safe.
	inflight bool
	errored  bool

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
			kind:         histToolCall,
			text:         renderToolCallBlockStyled(it.toolName, it.toolInput, mode, restingToolGlyphStyle(it.errored), restingToolGlyphStyle(it.errored)),
			toolName:     it.toolName,
			toolInput:    it.toolInput,
			toolInflight: it.inflight,
			toolErrored:  it.errored,
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
	// Reconcile the in-flight count with the freshly projected history so a
	// mode toggle mid-turn (which may add or drop the tool-call entries)
	// keeps the fingerprint's pulse term honest.
	m.inflightToolCount = 0
	for i := range out {
		if out[i].kind == histToolCall && out[i].toolInflight {
			m.inflightToolCount++
		}
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

// settleToolCall marks the in-flight tool call that a just-arrived result
// belongs to as finished: its ▸ glyph stops pulsing and lands on the resting
// color (red when errored). It flips both the source-of-truth transcript
// item and the live history entry so a later re-projection stays correct,
// and drops the in-flight count so the fingerprint's pulse term settles.
//
// Results carry no tool-use id on the live path (agent_run.go emits none), so
// matching is by tool name, oldest-first (FIFO): the result pairs with the
// earliest still-in-flight call of the same name. An empty name, or a name
// that matches no in-flight call, falls back to the oldest in-flight call of
// any name — correct for the common sequential case and a reasonable guess
// for parallel calls to the same tool.
func (m *model) settleToolCall(name string, errored bool) {
	if idx := oldestInflightTranscript(m.transcript, name); idx >= 0 {
		m.transcript[idx].inflight = false
		m.transcript[idx].errored = errored
	}
	if idx := oldestInflightHistory(m.history, name); idx >= 0 {
		e := &m.history[idx]
		e.toolInflight = false
		e.toolErrored = errored
		e.text = renderToolCallBlockStyled(e.toolName, e.toolInput, m.toolOutputMode, restingToolGlyphStyle(errored), restingToolGlyphStyle(errored))
		invalidateEntryRender(e)
		if m.inflightToolCount > 0 {
			m.inflightToolCount--
		}
	}
}

// oldestInflightTranscript returns the index of the earliest still-in-flight
// trToolCall, preferring a name match and falling back to any in-flight call.
// -1 when none is in flight.
func oldestInflightTranscript(items []transcriptItem, name string) int {
	fallback := -1
	for i := range items {
		it := items[i]
		if it.kind != trToolCall || !it.inflight {
			continue
		}
		if name != "" && it.toolName == name {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

// oldestInflightHistory is oldestInflightTranscript over the projected
// history entries (histToolCall). Returns -1 when the call has no visible
// entry (quiet/off mode) or none is in flight.
func oldestInflightHistory(hist []historyEntry, name string) int {
	fallback := -1
	for i := range hist {
		e := hist[i]
		if e.kind != histToolCall || !e.toolInflight {
			continue
		}
		if name != "" && e.toolName == name {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
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
