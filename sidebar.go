package main

// Sidebar tab mode: an alternate presentation of open tabs as a
// permanent right-hand column of task cards (cfg.UI.TabMode ==
// "sidebar"). Each card shows what the tab's agent is doing — a short
// LLM-generated title (tab_title.go), the provider/model, the
// session's accumulated dollar spend (usage.go), and a live
// activity line (current in_progress todo / stream status / workflow
// step) — plus attention badges so the user can see at a glance when
// a background tab needs input or finished.
//
// Geometry: the sidebar claims ~1/6 of the terminal (clamped to
// [sidebarMinWidth, sidebarMaxWidth]); below sidebarMinTotalWidth
// total columns the app silently degrades to the classic bottom bar
// rather than rendering an unusable sliver. The active tab's body is
// laid out at bodyWidth() and joined line-by-line with the rendered
// column (joinBodySidebar).
//
// Focus model: the sidebar list cursor IS app.active — selection has
// no view-local state, so the column is a pure projection of a.tabs.
// ActionSidebarFocus (default Tab) moves keyboard focus between the
// typing area and the list when the active tab has no local use for
// the key (model.wantsTabKey); while the list is focused, Up/Down
// switch tabs immediately (no Enter), typing any printable character
// bounces focus back to the input and forwards the keystroke, and
// Esc/Enter/Tab return without side effects. ActionTabPrevAlt /
// ActionTabNextAlt (Ctrl+Up / Ctrl+Down) switch tabs from anywhere.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

const (
	// sidebarMinWidth is the narrowest useful card column: enough for
	// "▸ <title>" plus a badge without truncating every word.
	sidebarMinWidth = 30
	// sidebarMaxWidth caps the column so huge terminals don't waste
	// a quarter of the screen on chrome.
	sidebarMaxWidth = 48
	// sidebarMinTotalWidth is the degrade threshold: below this many
	// total columns sidebar mode renders as the classic bottom bar.
	sidebarMinTotalWidth = 90
	// sidebarCardHeight is the fixed per-card footprint: title, meta,
	// cost, activity, separator blank.
	sidebarCardHeight = 5
	// sidebarHeaderHeight is the "tabs N/M" header plus its blank.
	sidebarHeaderHeight = 2
)

// tabModeSidebar reports whether sidebar mode is configured,
// regardless of whether the current width can render it. Behavioural
// consequences (workflow supplant, focus-steal suppression) follow
// the mode; rendering follows sidebarVisible.
func (a app) tabModeSidebarOn() bool { return a.tabMode == tabModeSidebar }

// sidebarVisible reports whether the sidebar column is actually
// rendered this frame: mode on AND the terminal wide enough.
func (a app) sidebarVisible() bool {
	return a.tabModeSidebarOn() && a.width >= sidebarMinTotalWidth
}

// sidebarWidth is the rendered column width: ~1/5 of the terminal,
// clamped. Zero when the sidebar isn't visible.
func (a app) sidebarWidth() int {
	if !a.sidebarVisible() {
		return 0
	}
	w := a.width / 5
	if w < sidebarMinWidth {
		w = sidebarMinWidth
	}
	if w > sidebarMaxWidth {
		w = sidebarMaxWidth
	}
	return w
}

// bodyWidth is what tabs get to lay out in: full width in bar mode,
// the remainder left of the sidebar otherwise.
func (a app) bodyWidth() int {
	w := a.width - a.sidebarWidth()
	if w < 1 {
		w = 1
	}
	return w
}

// sidebarVisibleCards returns how many whole cards fit below the
// header.
func (a app) sidebarVisibleCards() int {
	rows := a.height - sidebarHeaderHeight
	if rows < sidebarCardHeight {
		return 1
	}
	return rows / sidebarCardHeight
}

// sidebarScrollOffset keeps the active card inside the visible
// window. Stateless on purpose — derived from a.active so the column
// stays a pure projection (no view-local scroll state to drift).
func (a app) sidebarScrollOffset() int {
	visible := a.sidebarVisibleCards()
	if len(a.tabs) <= visible {
		return 0
	}
	off := a.active - visible + 1
	if off < 0 {
		off = 0
	}
	if max := len(a.tabs) - visible; off > max {
		off = max
	}
	return off
}

// sidebarCardAt maps a screen row (within the sidebar region) to the
// tab index of the card rendered there, or -1 for chrome rows.
func (a app) sidebarCardAt(y int) int {
	if y < sidebarHeaderHeight || y >= a.height {
		return -1
	}
	idx := a.sidebarScrollOffset() + (y-sidebarHeaderHeight)/sidebarCardHeight
	if idx < 0 || idx >= len(a.tabs) {
		return -1
	}
	return idx
}

// renderSidebar produces the full-height column: exactly a.height
// lines, each exactly sidebarWidth() cells wide, with a two-space
// gutter separating the cards from the chat body.
func (a app) renderSidebar() string {
	w := a.sidebarWidth()
	inner := w - 2 // left gutter
	if inner < 1 {
		inner = 1
	}

	headStyle := lipgloss.NewStyle().Foreground(activeTheme.dim)
	if a.sidebarFocus {
		headStyle = lipgloss.NewStyle().Foreground(activeTheme.accent).Bold(true)
	}

	lines := make([]string, 0, a.height)
	lines = append(lines,
		headStyle.Render(clipText(fmt.Sprintf("tabs %d/%d", a.active+1, len(a.tabs)), inner)),
		"")

	visible := a.sidebarVisibleCards()
	offset := a.sidebarScrollOffset()
	for i := offset; i < offset+visible && i < len(a.tabs); i++ {
		lines = append(lines, a.sidebarCardLines(i, inner)...)
	}

	for len(lines) < a.height {
		lines = append(lines, "")
	}
	lines = lines[:a.height]

	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  ")
		b.WriteString(padRight(ln, inner))
	}
	return b.String()
}

// sidebarCardLines renders one tab's card: title row (selection
// marker + badge), provider/model meta row, live activity row, and a
// blank separator. Exactly sidebarCardHeight lines.
func (a app) sidebarCardLines(i, inner int) []string {
	t := a.tabs[i]
	selected := i == a.active

	badge, badgeStyle := t.sidebarBadge()
	marker := "  "
	if selected {
		marker = "▸ "
	}
	titleW := inner - len(marker)
	if badge != "" {
		titleW -= lipgloss.Width(badge) + 1
	}
	title := marker + clipText(t.sidebarTitle(), titleW)
	var titleLine string
	switch {
	case selected && a.sidebarFocus:
		row := title
		if badge != "" {
			row += " " + badge
		}
		titleLine = themePickerRowStyle.Render(padRight(row, inner))
	case selected:
		titleLine = selectedStyle.Render(title)
		if badge != "" {
			titleLine += " " + badgeStyle.Render(badge)
		}
	default:
		titleLine = title
		if badge != "" {
			titleLine += " " + badgeStyle.Render(badge)
		}
	}

	meta := dimStyle.Render(clipText("  "+t.sidebarMeta(), inner))

	cost := dimStyle.Render(clipText("  "+t.sidebarCost(), inner))

	activity, activityStyle := t.sidebarActivity()
	activityLine := activityStyle.Render(clipText("  "+activity, inner))

	return []string{titleLine, meta, cost, activityLine, ""}
}

// sidebarTitle is the card headline: the generated/seeded tab title
// when one exists, the shortened cwd otherwise.
func (m *model) sidebarTitle() string {
	if m.tabTitle != "" {
		return m.tabTitle
	}
	if label := shortCwdOf(m.cwd); label != "" {
		return label
	}
	return "?"
}

// sidebarMeta is the dim second row: provider/model.
func (m *model) sidebarMeta() string {
	if m.provider == nil {
		return ""
	}
	return providerMeta(m.provider.ID(), m.providerModel)
}

// sidebarCost is the dim third row: the session's accumulated API
// spend in dollars and cents, live-updated per step (usage.go prices
// each call via the catwalk catalog). Before any priceable call lands
// it shows an honest $0.00 only when the current provider/model pair
// is in the catalog; unpriceable models (custom ids, providers
// without a catalog) render an empty row instead of a fake zero.
func (m *model) sidebarCost() string {
	if m.sessionCostKnown {
		return formatUSD(m.sessionCostUSD)
	}
	if m.provider != nil && modelPricingKnown(m.provider.ID(), m.effectiveModelID()) {
		return formatUSD(0)
	}
	return ""
}

// effectiveModelID resolves the model the next turn would actually
// run: the tab's explicit pick, else the provider's default.
func (m *model) effectiveModelID() string {
	if m.providerModel != "" {
		return m.providerModel
	}
	if m.provider != nil {
		if spec, ok := agentSpecByID(m.provider.ID()); ok {
			return spec.defaultModel
		}
	}
	return ""
}

// needsUserInput reports whether the tab is blocked on a human — an
// ask-question or approval modal is up. In sidebar mode these no
// longer steal focus (see dispatchByTabID); the badge is how the
// user finds them.
func (m *model) needsUserInput() bool {
	return m.mode == modeAskQuestion || m.mode == modeApproval
}

// sidebarBadge returns the attention glyph for the card's title row
// and the style to draw it with. Empty when there is nothing to
// flag.
func (m *model) sidebarBadge() (string, lipgloss.Style) {
	switch {
	case m.needsUserInput():
		return "⚠", lipgloss.NewStyle().Foreground(activeTheme.warn).Bold(true)
	case m.workflowRun != nil && m.workflowRun.failed:
		return "✗", errStyle
	case m.workflowRun != nil && m.workflowRun.done:
		return "✓", lipgloss.NewStyle().Foreground(activeTheme.success)
	case m.busy:
		return "●", lipgloss.NewStyle().Foreground(activeTheme.accentAlt)
	}
	return "", lipgloss.Style{}
}

// sidebarActivity is the card's live third row: what the tab's agent
// is doing right now. Workflow runs report their step cursor; busy
// chat turns surface the agent's own in_progress todo (the freshest
// LLM-authored description of the work), falling back to the stream
// status; blocked tabs say so; idle tabs say idle.
func (m *model) sidebarActivity() (string, lipgloss.Style) {
	warnStyle := lipgloss.NewStyle().Foreground(activeTheme.warn)
	successStyle := lipgloss.NewStyle().Foreground(activeTheme.success)
	if r := m.workflowRun; r != nil {
		switch {
		case r.failed:
			return "✗ workflow failed", errStyle
		case r.done:
			if r.supplanted != nil {
				return "✓ done · enter returns", successStyle
			}
			return "✓ workflow done", successStyle
		default:
			return fmt.Sprintf("⟳ %s · step %d/%d",
				r.Workflow.Name, r.StepIdx+1, len(r.Workflow.Steps)), todoProgressStyle
		}
	}
	if m.needsUserInput() {
		return "⚠ waiting for your input", warnStyle
	}
	if m.busy {
		for _, t := range m.todos {
			if t.Status == "in_progress" {
				return "▸ " + nonEmpty(t.ActiveForm, t.Content), todoProgressStyle
			}
		}
		if m.status != "" {
			return m.status, dimStyle
		}
		return "working…", dimStyle
	}
	return "idle", dimStyle
}

// wantsTabKey reports whether the active tab has a local meaning for
// the bare Tab keypress, in which case the app layer must NOT
// intercept it for sidebar focus. True for every modal/picker, every
// non-chat screen (issues kanban, workflows builder, both use Tab
// for their own navigation), and the chat input while a completion
// popover (path picker / slash menu) is open.
func (m *model) wantsTabKey() bool {
	if m.workflowRun != nil {
		// Workflow tabs absorb all typing — Tab is free.
		return false
	}
	if m.mode != modeInput {
		return true
	}
	if m.screen != screenAsk {
		return true
	}
	if m.workflowPicker != nil || m.cancelTurnConfirming || m.closeTabConfirming || m.mergePRConfirming {
		return true
	}
	if !m.busy && m.pathPickerActive() && len(m.pathMatches) > 0 {
		return true
	}
	if !m.busy && m.historyIdx < 0 && len(m.filterSlashCmds()) > 0 {
		return true
	}
	return false
}

// handleSidebarKey processes a keypress at the app layer when the
// sidebar is visible. handled=false means the key falls through to
// the active tab as usual.
func (a app) handleSidebarKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd, bool) {
	km := currentKeyMap()
	if !a.sidebarFocus {
		if km.Matches(ActionSidebarFocus, msg) && !a.activeTab().wantsTabKey() {
			a.sidebarFocus = true
			return a, nil, true
		}
		return a, nil, false
	}
	switch {
	case km.Matches(ActionSidebarFocus, msg),
		msg.Mod == 0 && msg.Code == tea.KeyEsc,
		msg.Mod == 0 && msg.Code == tea.KeyEnter:
		a.sidebarFocus = false
		return a, nil, true
	case msg.Mod == 0 && (msg.Code == tea.KeyUp || msg.Code == 'k'):
		nm, cmd := a.switchTab(a.active - 1)
		return nm, cmd, true
	case msg.Mod == 0 && (msg.Code == tea.KeyDown || msg.Code == 'j'):
		nm, cmd := a.switchTab(a.active + 1)
		return nm, cmd, true
	case km.Matches(ActionTabClose, msg):
		nm, cmd := a.closeTab(a.activeTab().id)
		return nm, cmd, true
	}
	// Type-to-return: any printable character bounces focus back to
	// the typing area and lands in the input, so starting to type is
	// never a wasted keystroke. j/k are list nav above, matching the
	// kanban convention.
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		a.sidebarFocus = false
		nm, cmd := a.dispatchActive(msg)
		return nm, cmd, true
	}
	// Everything else is absorbed while the list owns the keyboard.
	return a, nil, true
}

// joinBodySidebar composes the active tab's body (laid out at bodyW)
// with the rendered sidebar column, line by line. Both inputs are
// expected at equal heights; the shorter side pads with blanks.
func joinBodySidebar(body, sidebar string, bodyW int) string {
	bl := strings.Split(body, "\n")
	sl := strings.Split(sidebar, "\n")
	n := len(bl)
	if len(sl) > n {
		n = len(sl)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var b, s string
		if i < len(bl) {
			b = bl[i]
		}
		if i < len(sl) {
			s = sl[i]
		}
		out = append(out, padRight(b, bodyW)+s)
	}
	return strings.Join(out, "\n")
}

// clipText hard-truncates s to at most w cells, appending an
// ellipsis when something was cut. ANSI-free input expected (style
// is applied after clipping).
func clipText(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}
