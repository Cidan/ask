package main

import (
	tea "charm.land/bubbletea/v2"
)

// screenID names a top-level screen the tab can show. Each value
// corresponds to a registered screen handler in screensRegistry. Adding
// a new top-level surface (e.g. a settings screen) is two changes: a new
// screenID constant and an entry in newScreenRegistry.
type screenID int

const (
	screenAsk screenID = iota
	screenIssues
	screenWorkflows
)

// screen is the per-tab top-level UI handler. Implementations are
// stateless dispatchers — screen-specific state lives on model in
// dedicated fields (model.issues, the chat fields, etc.). Keeping the
// interface stateless avoids fighting Update's value-receiver pattern:
// every Update returns a fresh model, and we look up the handler each
// time via activeScreen() rather than carrying a pointer.
//
// Background work (claude streaming, MCP modal requests, shell output,
// usage updates) is routed by message type at the model.Update layer,
// not through this interface. That means an inactive screen still
// receives updates to the model state it owns — when the user flips
// back, the latest state is already there.
type screen interface {
	id() screenID
	// updateKey handles a keypress when this screen is active and no
	// modal/picker is up. Returns the new model, any tea.Cmd, and a
	// handled flag. handled=false lets the caller fall through to the
	// shared no-op path so unrouted keys don't accidentally mutate
	// state.
	updateKey(m model, msg tea.KeyPressMsg) (model, tea.Cmd, bool)
	// view returns the full screen body. This is what the outer View()
	// composes modal overlays, toasts, and the cursor on top of.
	view(m model) string
}

// screensRegistry is the singleton screen handler table. Initialised
// once; lookup is by screenID. Tabs share handlers because
// implementations are stateless.
var screensRegistry = newScreenRegistry()

func newScreenRegistry() map[screenID]screen {
	return map[screenID]screen{
		screenAsk:       askScreen{},
		screenIssues:    issuesScreen{},
		screenWorkflows: workflowsScreen{},
	}
}

func (m model) activeScreen() screen {
	if s, ok := screensRegistry[m.screen]; ok {
		return s
	}
	return askScreen{}
}

// modalOpen reports whether a modal/picker is currently blocking
// regular input. Screen-switching keys (Ctrl+I, Ctrl+O) are gated on
// !modalOpen so a half-completed modal interaction can't be silently
// abandoned by flipping screens.
//
// modeInput with a confirm overlay (cancel-turn, close-tab) also
// counts as a modal — those overlays steal y/n keystrokes that would
// otherwise be the screen's, so blocking the swap there is the safe
// thing to do until the user dismisses.
func (m model) modalOpen() bool {
	if m.mode != modeInput {
		return true
	}
	if m.cancelTurnConfirming || m.closeTabConfirming {
		return true
	}
	return false
}

// switchScreen flips the active screen. Returns the model unchanged
// when the request is a no-op (already on that screen) or rejected
// (modal open). Cache invalidation is handled by including m.screen
// in contentFingerprint, so the next render rebuilds against the new
// screen body without an explicit reset here.
func (m model) switchScreen(target screenID) model {
	if m.modalOpen() {
		return m
	}
	if m.screen == target {
		return m
	}
	m.screen = target
	// Seed per-screen state on first entry so callers (and tests)
	// don't have to wait for the next Update tick for it to exist.
	// Idempotent on re-entry: the nil-check skips re-seeding so user
	// state (cursor position, future filters) persists across flips.
	if target == screenIssues && m.issues == nil {
		m.issues = newIssuesState()
	}
	// Clear the chat-view text selection on the way out of ask — the
	// selection's anchor coordinates are tied to the chat viewport,
	// which is no longer being painted. Without this, dragging in
	// issues would re-arm a stale selection on return.
	m.clearSelection()
	m.lastContentFP = ""
	if m.fc != nil {
		m.fc.vpFP = ""
		m.fc.vbFP = ""
	}
	return m
}

// askScreen is the chat surface — what existed before screens were a
// concept. updateKey delegates to the legacy updateInput dispatcher so
// no behaviour shifts when the user is on this screen.
type askScreen struct{}

func (askScreen) id() screenID { return screenAsk }

func (askScreen) updateKey(m model, msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	newM, cmd := m.updateInput(msg)
	mm, ok := newM.(model)
	if !ok {
		return m, cmd, true
	}
	return mm, cmd, true
}

func (askScreen) view(m model) string {
	return m.viewAskBody()
}
