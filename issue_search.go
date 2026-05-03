package main

import (
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// searchMode toggles between raw provider-syntax mode and the
// (v1: scaffolded only) AI passthrough mode.
//
// Tab swaps mode WITHOUT rewriting the typed text — that's the
// non-negotiable UX invariant the user pinned in the design
// review. AI mode in v1 is a placeholder: Tab toggle, border
// recolor, [AI] chip, and a "not yet implemented" Enter
// response. No translation, no inference.
type searchMode int

const (
	searchModeRaw searchMode = iota
	searchModeAI
)

// issueSearchBox is the inline overlay rendered above the body
// when the user hits "/" on the list or kanban view. textinput
// owns the cursor and edit state; we layer mode + parseErr +
// aiNotice around it.
type issueSearchBox struct {
	input    textinput.Model
	mode     searchMode
	parseErr string
	aiNotice string
	width    int
}

// newIssueSearchBox builds the overlay with sensible defaults
// (raw mode, empty input). Callers focus the input and resize
// against the screen width before the first render.
func newIssueSearchBox(syntaxHelp string) *issueSearchBox {
	ti := textinput.New()
	ti.Placeholder = syntaxHelp
	ti.Focus()
	ti.SetVirtualCursor(false)
	return &issueSearchBox{input: ti, mode: searchModeRaw}
}

// resize reflows the input width. Called from the screen view()
// so a window resize keeps the box at the right width.
func (b *issueSearchBox) resize(width int) {
	b.width = width
	// Bubbles' textinput uses Width to bound the *visible* slice
	// of the line — leave a few cols for chrome (border + chip).
	w := width - 8
	if w < 16 {
		w = 16
	}
	b.input.SetWidth(w)
}

// updateKey is the per-key dispatcher for the search box. The
// returned bool is "consumed" — true means the key was handled
// here and should NOT propagate to the underlying view. The
// returned tea.Cmd is what the screen handler should run after
// state mutates (typically nil for typed keys, non-nil for the
// raw-mode Enter dispatch).
//
// Keys handled here:
//   - Esc: close the box
//   - Backspace on empty input: also close
//   - Tab: toggle mode (text carries verbatim)
//   - Enter (raw): parse + dispatch fresh page-1 load
//   - Enter (AI): set aiNotice; do NOT close, do NOT dispatch
//   - everything else: forward to the textinput.Model
//
// The "close" decision is signalled by the returned bool tuple's
// second value (closed). The caller (issuesScreen) clears
// s.search when closed=true.
func (b *issueSearchBox) updateKey(s *issuesState, msg tea.KeyPressMsg) (closed bool, cmd tea.Cmd) {
	switch {
	case msg.Mod == 0 && msg.Code == tea.KeyEsc:
		return true, nil
	case msg.Mod == 0 && msg.Code == tea.KeyBackspace && b.input.Value() == "":
		return true, nil
	case msg.Mod == 0 && msg.Code == tea.KeyTab:
		// Tab is dumb on purpose — flip the mode flag, recolour
		// the border, leave the typed text alone. NEVER infer,
		// translate, or rewrite. The user pinned this in design
		// review and it's the load-bearing UX invariant.
		if b.mode == searchModeRaw {
			b.mode = searchModeAI
		} else {
			b.mode = searchModeRaw
		}
		b.parseErr = ""
		b.aiNotice = ""
		return false, nil
	case msg.Mod == 0 && msg.Code == tea.KeyEnter:
		switch b.mode {
		case searchModeRaw:
			cmd := b.submitRaw(s)
			// Only close on success: a parse error keeps the box
			// open so the user can correct the typo without
			// re-typing from scratch.
			if b.parseErr != "" {
				return false, nil
			}
			return true, cmd
		case searchModeAI:
			b.aiNotice = "AI search not yet implemented — Tab to switch to raw mode"
			return false, nil
		}
	}
	// Forward everything else to the textinput. Touching the
	// input clears any stale parseErr / aiNotice — the user is
	// editing, the previous attempt's feedback is irrelevant.
	if b.parseErr != "" || b.aiNotice != "" {
		b.parseErr = ""
		b.aiNotice = ""
	}
	ti, _ := b.input.Update(msg)
	b.input = ti
	return false, nil
}

// submitRaw handles raw-mode Enter: parse the input, on success
// reset the issuesState for the new query and dispatch a
// page-1 load command; on parse failure stash the error in
// parseErr and DON'T close.
//
// Returns nil when the parse failed (the caller looks at
// b.parseErr to decide whether to render the inline error).
// Returns a tea.Cmd when the dispatch should fire — the
// caller's "closed" bool already drives the close.
func (b *issueSearchBox) submitRaw(s *issuesState) tea.Cmd {
	q, err := s.provider.ParseQuery(b.input.Value())
	if err != nil {
		b.parseErr = err.Error()
		return nil
	}
	// resetForNewQuery cancels any in-flight load (cuts the
	// network round-trip short on supersede) and bumps gen so
	// any responses already on their way drop on the receive
	// side.
	ctx := s.resetForNewQuery(q)
	// If we already have cached chunks for this query (search
	// resubmit on the same fingerprint), skip the network round
	// trip — the screen will rebuild the active sub-view against
	// the cache when it processes the closed=true return.
	if s.hasAnyCachedPage(q) {
		return nil
	}
	pagination := IssuePagination{Cursor: "", PerPage: githubDefaultPerPage}
	return loadIssuesPageCmd(
		ctx, s.tabID, s.screen, s.provider, s.projectCfg, s.cwd,
		q, pagination, s.queryGen,
	)
}

// view renders the overlay box. mode controls the border colour
// and presence of the [AI] chip. parseErr / aiNotice are
// rendered on a second line below the input when set.
func (b *issueSearchBox) view() string {
	border := lipgloss.RoundedBorder()
	borderColor := activeTheme.accent
	if b.mode == searchModeAI {
		borderColor = activeTheme.accentAlt
	}
	box := lipgloss.NewStyle().
		Border(border).
		BorderForeground(borderColor).
		Padding(0, 1)
	chip := ""
	if b.mode == searchModeAI {
		chip = lipgloss.NewStyle().
			Foreground(activeTheme.accentAlt).
			Bold(true).
			Render("[AI] ")
	}
	body := chip + b.input.View()
	rendered := box.Render(body)
	if b.parseErr != "" {
		rendered += "\n" + errStyle.Render(b.parseErr)
	}
	if b.aiNotice != "" {
		rendered += "\n" + dimStyle.Render(b.aiNotice)
	}
	return rendered
}
