package main

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// issueLoadingMessages is the rotation pool for the loading modal.
// Callers pick once at dispatch time and stash on issuesState so the
// message stays stable across renders instead of flickering.
var issueLoadingMessages = []string{
	"Loading issues...",
	"Getting bugs...",
	"Reticulating splines...",
	"Herding gophers...",
	"Polishing tickets...",
	"Untangling backlog...",
	"Counting open PRs...",
	"Sweeping the project board...",
	"Brewing fresh issues...",
	"Asking GitHub nicely...",
}

func pickLoadingMessage() string {
	return issueLoadingMessages[rand.Intn(len(issueLoadingMessages))]
}

// issueMoveTimeout caps how long a single carry-and-drop provider
// call is allowed to run before the cmd resolves with a context.Canceled
// error (which the rollback handler surfaces as a toast). 30s is a
// conservative bound — the GitHub MCP server typically responds in
// under a second, but a saturated network shouldn't strand the user
// without feedback. Lives at the provider-agnostic layer so future
// backends with their own latency profiles are still bounded.
const issueMoveTimeout = 30 * time.Second

// issueLoadingTickInterval is the cadence of the loader animation.
// ~30fps lands smooth on every terminal we've tried without burning
// CPU. Bumping to 16ms (60fps) felt great on a 120Hz monitor but
// produced visible jitter on slow VTE terminals; 33ms is the happy
// medium.
const issueLoadingTickInterval = 33 * time.Millisecond

// issueLoadingSpinnerFrames is the high-FPS braille glyph that
// pulses next to the loading message. Cycles at every tick so the
// loader has a visible "still alive" cue between cube frames
// (the cube changes more slowly so each rotation stage stays
// readable).
var issueLoadingSpinnerFrames = []string{
	"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏",
}

// issueLoadingTickMsg fires every issueLoadingTickInterval while
// the loader modal is up. tabID lets the handler ignore ticks
// targeting a different tab; the handler also drops the tick
// silently when s.loading is false so the animation cleanly stops
// once the first chunk arrives.
type issueLoadingTickMsg struct{ tabID int }

// issueLoadingTickCmd schedules the next loader animation tick.
// Returned by every entry path that flips s.loading=true (Ctrl+I
// screen entry, search-box submit, reloadCurrentQuery) and
// re-emitted by the handler as long as loading is still true.
func issueLoadingTickCmd(tabID int) tea.Cmd {
	return tea.Tick(issueLoadingTickInterval, func(time.Time) tea.Msg {
		return issueLoadingTickMsg{tabID: tabID}
	})
}

// issue is the in-memory representation of a single tracked issue.
// Fields are deliberately provider-neutral: the eventual GitHub /
// ClickUp / Linear backends will all map onto this same shape, with
// per-backend extras kept in a separate sidecar struct so the
// kanban cards stay homogeneous. For the mock UI it's just
// hardcoded data.
type issue struct {
	number    int
	title     string
	assignee  string
	status    string
	createdAt time.Time

	// description is the issue body in markdown. Rendered through the
	// project glamour renderer in the detail sub-view. Keep it as raw
	// markdown so the renderer's width-aware wrap can re-flow it on
	// resize without us having to remember the previous width.
	description string

	// comments is the comment thread, oldest-first. Each entry is
	// rendered as a small header line (author · date) plus a markdown
	// body, also through glamour. Order is preserved as-is — sorting
	// belongs to the detail view, not the data, so we don't smear "the
	// canonical thread order" across read sites.
	comments []issueComment
}

// issueComment is one comment in an issue's thread. Provider-neutral
// like issue itself: GitHub / ClickUp / Linear all map onto these
// three fields cleanly.
type issueComment struct {
	author    string
	createdAt time.Time
	body      string
}

// issueSort is the comparator strategy applied to incoming chunks
// before they're cached. Defaults to byNumber ascending; future
// sort-by toggles will install other comparators here without
// restructuring the state.
type issueSort int

const (
	issueSortByNumber issueSort = iota
)

// issuesState is the per-tab state for the issues screen. Holds the
// collection of issues and whichever sub-view is currently rendering
// (list today, kanban later). The screen interface lookup is in
// screens.go; this struct holds only data + the sub-view dispatcher
// so adding kanban is one new file plus a setView call.
// viewLayer is one entry in the Ctrl+I cycle. The cycle is the set of
// "primary" sub-views the user can flip between (list, kanban, future
// per-assignee swimlanes, …). Detail view is not in this list — it's
// reached via Enter and exited via Esc, not via cycling.
type viewLayer struct {
	name    string
	builder func(*issuesState) issueView
}

// issueViewLayers is the canonical cycle order. Adding a new
// top-level view type is appending here. Order matters: it's the
// order Ctrl+I walks through. Today it's just kanban — the
// flat-list view was removed because the column picker reads
// better at every terminal width — but the registry shape is
// preserved so the next view (per-assignee swimlanes, milestone
// grid, …) drops in as one entry.
var issueViewLayers = []viewLayer{
	{name: "kanban", builder: func(s *issuesState) issueView { return newKanbanIssueView(s) }},
}

// viewIndexForName returns the layer index whose builder
// produces a view of the given name. Returns 0 when no match —
// the cycle layers are guaranteed to contain at least one entry.
func viewIndexForName(name string) int {
	for i, l := range issueViewLayers {
		if l.name == name {
			return i
		}
	}
	return 0
}

// issuePageLoadedMsg is the cursor-based load result. The
// handler drops the message when gen != s.queryGen (stale
// supersede) and reads requestedCursor (not page.NextCursor)
// to identify first-chunk-of-query, since page is zero-valued
// on error paths.
type issuePageLoadedMsg struct {
	tabID           int
	gen             int
	query           IssueQuery
	requestedCursor string
	page            IssueListPage
	err             error
}

// loadIssuesPageCmd dispatches a cursor-based provider call off
// the main loop. The 30s timeout lines up with the per-call MCP
// timeout — we'd rather surface a clean "request timed out"
// toast than have the Bubble Tea Update goroutine blocked
// indefinitely if the network stalls.
//
// ctx allows the screen to cancel an in-flight request when a
// new query supersedes it. Pass context.Background() for one-off
// loads where cancellation isn't needed.
func loadIssuesPageCmd(ctx context.Context, tabID int, p IssueProvider, pc projectConfig, cwd string, query IssueQuery, pagination IssuePagination, gen int) tea.Cmd {
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		page, err := p.ListIssues(cctx, pc, cwd, query, pagination)
		return issuePageLoadedMsg{
			tabID:           tabID,
			gen:             gen,
			query:           query,
			requestedCursor: pagination.Cursor,
			page:            page,
			err:             err,
		}
	}
}

type issuesState struct {
	sort issueSort

	view issueView

	// provider is the active IssueProvider for this tab/cwd.
	// Captured on screen entry so query fingerprinting and
	// kanban column lookups don't have to re-resolve through
	// activeIssueProvider on every render.
	provider IssueProvider

	// pageCache holds the loaded chunks of every query the user
	// has touched in this session. Outer key is the query
	// fingerprint (FormatQuery on the IssueQuery, with nil →
	// empty string); the inner slice is the chunk chain in
	// fetch order, where chunks[i].nextCursor == chunks[i+1].cursor
	// links the chain. Cursor-based pagination has no stable page
	// numbers so we key strictly on chain ordinal; Ctrl+R wipes
	// the entry for the active query without disturbing other
	// queries' caches. The cache stays valid across screen swaps
	// so Esc-back-to-list doesn't have to re-fetch.
	pageCache map[string][]issuePageChunk

	// queryGen is the monotonically-increasing generation
	// counter. Each Enter from the search box bumps it; in-flight
	// loads tag their dispatch with the gen they saw and the
	// receiver drops messages whose gen doesn't match.
	queryGen int

	// loadCtx + cancelLoad scope every dispatched ListIssues call
	// for the active query. beginLoad replaces this pair, so a new
	// query (search-box submit) or screen re-entry cancels every
	// in-flight fetch — list-view next-page, kanban initialLoad,
	// kanban per-column scroll — in one shot. Stale-gen drop on
	// the receive side is the secondary correctness mechanism;
	// this one cuts the network round-trip short.
	loadCtx    context.Context
	cancelLoad context.CancelFunc

	// currentQuery is the IssueQuery for the active filter. nil
	// means "default" — kanban dispatches per-column queries from
	// provider.KanbanColumns() under it; when a search-box submit
	// stages a non-nil query the same per-column queries layer the
	// extra filter on top.
	currentQuery IssueQuery

	// projectCfg + cwd + tabID are the dispatch context the views
	// need to fire follow-up loads (next-page on scroll, refresh
	// on search-box submit). Stashed on the state at screen-entry
	// time so the views don't have to thread these through every
	// updateKey signature.
	projectCfg projectConfig
	cwd        string
	tabID      int

	// search is the optional inline search overlay. Non-nil when
	// the user has hit "/" — Esc closes it.
	search *issueSearchBox

	// Selection state mirrors the chat-side fields (selDragging /
	// selActive / selAnchor / selFocus) but lives here so it doesn't
	// leak into ask-screen behaviour. Anchor/focus are in the active
	// sub-view's *content* coordinates: detail's selectionYOffset is
	// applied at click time so a scroll keeps the highlight tracked
	// against absolute content rows; list's selectionYOffset is 0 so
	// rows are screen-relative and the screen handler clears the
	// selection on cursor moves to keep it from drifting.
	selDragging bool
	selActive   bool
	selAnchor   cellPos
	selFocus    cellPos

	// scrollbarDragging is true while the user is mid-drag on the
	// scrollbar thumb. Mouse motion translates the cursor's Y back to
	// the active sub-view's setYOffset.
	scrollbarDragging bool

	// loading flips true when a provider call is in flight. The screen
	// renders a centered modal in place of the (still-empty) kanban
	// body so the user sees activity instead of a blank table.
	// loadingMessage is picked once per dispatch and held stable for the
	// duration of the load so it doesn't flicker through the pool on
	// every render frame. loadingFrame is the high-fps animation
	// counter incremented by issueLoadingTickMsg while loading is
	// true; the modal renders a braille spinner + bouncing marquee
	// keyed off it.
	loading        bool
	loadingMessage string
	loadingFrame   int

	// loadErr is non-nil when the most recent provider call failed.
	// Holds the error verbatim so the modal can show the underlying
	// network/auth/parse failure to the user instead of a generic
	// "something went wrong". Cleared when the modal is dismissed
	// (Enter/Esc returns to ask) or when a new load is dispatched.
	loadErr error

	// bodyTopRow / bodyContentH / bodyLeftCol record where the body
	// area lives on screen during the most recent view() pass. Mouse
	// handlers consult these to know whether a click landed inside
	// the body area, on the scrollbar column, or in chrome above /
	// below it.
	bodyTopRow   int
	bodyContentH int
	bodyLeftCol  int
	scrollbarCol int
}

// issueView is a sub-view inside the issues screen. Kanban is the
// only top-level implementation today (a flat-list variant lived
// here briefly and was retired); detail is the read-mode surface
// reached via Enter. Each implementation owns the rendering and
// key-handling for its surface and is fed the parent issuesState
// so it can read the current collection without each variant
// re-fetching.
type issueView interface {
	name() string
	// resize is called on WindowSizeMsg and on screen entry with the
	// available body area after the screen chrome is accounted for.
	resize(width, height int)
	// updateKey handles keys when this sub-view is active. Returns the
	// (possibly mutated) view, a tea.Cmd, and handled=true if the key
	// was consumed.
	updateKey(s *issuesState, msg tea.KeyPressMsg) (issueView, tea.Cmd, bool)
	// view returns the rendered body for the sub-view, sized to the
	// width/height previously passed to resize.
	view(s *issuesState) string
	// header is the screen-chrome title line. Sub-views own this so
	// the detail view can show "Issue #15 · title" while kanban
	// shows "Issues (10) — kanban view" without the screen handler
	// having to branch on view type.
	header(s *issuesState) string
	// hint is the screen-chrome footer line. Same reason as header:
	// kanban and detail views advertise different keybindings, and
	// this is where each declares its own.
	hint() string
	// scroll returns the current scroll position triple — yOffset,
	// total scrollable lines, and viewport height — so the screen
	// can render a scrollbar without each sub-view having to know
	// how to draw one. The values are sub-view-defined: list returns
	// (cursor, len(rows), table.Height); detail returns
	// (vp.YOffset, lipgloss.Height(rendered), vp.Height).
	scroll() (yOffset, total, viewH int)
	// setYOffset is invoked by the scrollbar drag handler with a row
	// index in [0, total). Sub-views are free to clamp.
	setYOffset(int)
	// wheel applies a mouse-wheel delta (+down, -up). Sub-views own
	// the per-view scroll semantics — the list moves the table cursor,
	// detail scrolls the viewport.
	wheel(delta int)
	// selectableBody returns the *full* rendered content the
	// selection layer should treat as the source of truth — for
	// detail this is the entire glamour-rendered body even past the
	// viewport bottom, so a selection that scrolls off-screen still
	// has the right line to slice when copied. For list this is just
	// table.View() (header + visible rows), since list selection is
	// transient (cleared on cursor move).
	selectableBody() string
	// selectionYOffset is the row offset between selection content
	// rows and the visible body. Detail returns vp.YOffset so a
	// click at screenY → contentRow includes the scrolled-past lines;
	// list returns 0 because its selection is screen-relative and
	// clears on navigation anyway.
	selectionYOffset() int
}

// newIssuesState builds an empty issues state. The provider is
// installed by the screen-entry path (Ctrl+I in update.go) once
// it's resolved which provider the project uses; tests can
// install a fake provider directly.
func newIssuesState() *issuesState {
	s := &issuesState{
		sort:      issueSortByNumber,
		pageCache: map[string][]issuePageChunk{},
		provider:  noneIssueProvider{},
		loadCtx:   context.Background(),
	}
	s.view = issueViewLayers[0].builder(s)
	return s
}

// issuePageChunk is one chunk in a query's fetched chain.
// hasMore is tracked separately from nextCursor so end-of-data
// is captured even on backends that return an empty cursor mid-
// chain.
type issuePageChunk struct {
	cursor     string
	nextCursor string
	hasMore    bool
	issues     []issue
}

// queryFingerprint maps an IssueQuery (opaque interface{}) to a
// stable string key by delegating to provider.FormatQuery. nil
// query → empty string, which matches noneIssueProvider's
// FormatQuery output. Different providers will fingerprint the
// same logical query differently — that's fine, the fingerprint
// only has to be stable within one tab's lifetime.
func (s *issuesState) queryFingerprint(q IssueQuery) string {
	if s.provider == nil {
		return ""
	}
	return s.provider.FormatQuery(q)
}

func (s *issuesState) cachedChunks(q IssueQuery) []issuePageChunk {
	if s.pageCache == nil {
		return nil
	}
	return s.pageCache[s.queryFingerprint(q)]
}

func (s *issuesState) appendChunk(q IssueQuery, chunk issuePageChunk) {
	if s.pageCache == nil {
		s.pageCache = map[string][]issuePageChunk{}
	}
	fp := s.queryFingerprint(q)
	s.pageCache[fp] = append(s.pageCache[fp], chunk)
}

// clearQueryCache wipes the chunk chain for q without touching
// any other query's cache. Used by Ctrl+R reload so a
// re-dispatch starts a fresh chain instead of reusing stale
// cursors that the backend may have invalidated.
func (s *issuesState) clearQueryCache(q IssueQuery) {
	if s.pageCache == nil {
		return
	}
	delete(s.pageCache, s.queryFingerprint(q))
}

// removeIssueFromCache strips the issue with matching number from
// q's cached chunk chain, returning the removed issue, an absolute
// row index across the flattened chain (so a later insertIssueIntoCache
// call can put it back where it was), and an ok flag. The cursor /
// nextCursor / hasMore fields of surviving chunks are untouched —
// removal is a content-only edit, not a chain truncation. Used by
// the kanban carry flow to rip a card out of its origin column.
func (s *issuesState) removeIssueFromCache(q IssueQuery, issueNumber int) (issue, int, bool) {
	if s.pageCache == nil {
		return issue{}, 0, false
	}
	fp := s.queryFingerprint(q)
	chain, ok := s.pageCache[fp]
	if !ok {
		return issue{}, 0, false
	}
	flatIdx := 0
	for ci := range chain {
		for ii := range chain[ci].issues {
			if chain[ci].issues[ii].number == issueNumber {
				removed := chain[ci].issues[ii]
				chain[ci].issues = append(chain[ci].issues[:ii], chain[ci].issues[ii+1:]...)
				s.pageCache[fp] = chain
				return removed, flatIdx, true
			}
			flatIdx++
		}
	}
	return issue{}, 0, false
}

// insertIssueIntoCache puts it back into q's cached chunk chain at
// the given absolute index across the flattened chain. Negative
// indices clamp to 0; indices past the end clamp to append. When no
// cache entry exists for q yet, a fresh single-chunk entry is created
// (cursor / nextCursor empty, hasMore=false) so a later ListIssues
// resumes from "first chunk" cleanly without colliding on cursors.
func (s *issuesState) insertIssueIntoCache(q IssueQuery, it issue, index int) {
	if s.pageCache == nil {
		s.pageCache = map[string][]issuePageChunk{}
	}
	fp := s.queryFingerprint(q)
	chain := s.pageCache[fp]
	if len(chain) == 0 {
		s.pageCache[fp] = []issuePageChunk{{issues: []issue{it}}}
		return
	}
	if index < 0 {
		index = 0
	}
	flatIdx := 0
	for ci := range chain {
		chunkLen := len(chain[ci].issues)
		if index <= flatIdx+chunkLen {
			local := index - flatIdx
			if local < 0 {
				local = 0
			}
			chain[ci].issues = append(chain[ci].issues[:local],
				append([]issue{it}, chain[ci].issues[local:]...)...)
			s.pageCache[fp] = chain
			return
		}
		flatIdx += chunkLen
	}
	last := len(chain) - 1
	chain[last].issues = append(chain[last].issues, it)
	s.pageCache[fp] = chain
}

func (s *issuesState) hasAnyCachedPage(q IssueQuery) bool {
	return len(s.cachedChunks(q)) > 0
}

// beginLoad cancels any in-flight load and installs a fresh
// cancellable context as s.loadCtx. Returns the new context
// so the caller can hand it straight to loadIssuesPageCmd.
// Used by every dispatch path that establishes a "new wave" of
// fetches (screen entry, kanban-on-cycle, search submit).
func (s *issuesState) beginLoad() context.Context {
	if s.cancelLoad != nil {
		s.cancelLoad()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.loadCtx = ctx
	s.cancelLoad = cancel
	return ctx
}

// resetForNewQuery is beginLoad + bumps the gen counter and
// stages currentQuery for a fresh dispatch. The chunk cache for
// OTHER queries is preserved so the user can flip back without
// re-fetching.
func (s *issuesState) resetForNewQuery(q IssueQuery) context.Context {
	ctx := s.beginLoad()
	s.queryGen++
	s.currentQuery = q
	return ctx
}

// discardOnLeave is invoked when the user navigates away from the
// issues screen (Ctrl+O). It cancels any in-flight load, drops
// every cached chunk, bumps queryGen so late responses can't
// pollute the next entry, and clears all UI flags. The next Ctrl+I
// entry sees an empty cache and dispatches fresh — appendChunk
// otherwise stacks into the chain and the user sees duplicates.
func (s *issuesState) discardOnLeave() {
	if s.cancelLoad != nil {
		s.cancelLoad()
		s.cancelLoad = nil
	}
	s.loadCtx = context.Background()
	s.pageCache = nil
	s.queryGen++
	s.loading = false
	s.loadingMessage = ""
	s.loadingFrame = 0
	s.loadErr = nil
	s.search = nil
}

// reloadCurrentQuery is the Ctrl+R entry point. It clears the
// active query's chunk cache plus every kanban-column query's
// cache (other queries stay intact), cancels any in-flight fetch
// via beginLoad, bumps queryGen so any responses already on the
// wire drop on the receive side, raises the animated loading
// modal, and rebuilds the active sub-view against the now-empty
// cache. Returns the dispatch tea.Cmd: N parallel column fetches
// batched with the loader animation tick. The view will
// threshold-fetch additional chunks lazily as the user scrolls.
// Eager-walking the chain on reload would be a regression on huge
// repos.
func (s *issuesState) reloadCurrentQuery() tea.Cmd {
	_ = s.beginLoad()
	s.queryGen++
	s.loading = true
	s.loadingMessage = pickLoadingMessage()
	s.loadingFrame = 0
	s.loadErr = nil
	switch s.view.(type) {
	case *kanbanIssueView:
		// Kanban's "current query" is the union of column queries
		// — wipe each so the rebuilt view's initialLoad refetches
		// every column. Other queries stay cached.
		s.clearQueryCache(s.currentQuery)
		for _, spec := range s.provider.KanbanColumns() {
			s.clearQueryCache(spec.Query)
		}
		nv := newKanbanIssueView(s)
		s.view = nv
		return tea.Batch(nv.initialLoad(s), issueLoadingTickCmd(s.tabID))
	}
	return nil
}

// cycleView advances the active view to the next entry in
// issueViewLayers. Returns true when the cycle moved (current view
// participates in the layer cycle and the registry has more than
// one entry), false when it didn't (the user is on the detail
// view, or there's nothing else to cycle to). Selection is
// dropped on swap so a stale highlight against the previous
// layer's body can't leak into the new layer. With a single-entry
// registry — the current state with kanban-only — Ctrl+I is a
// quiet no-op that never rebuilds the view (rebuilding would
// reset the kanban cursor for no user-visible benefit).
func (s *issuesState) cycleView() bool {
	if s.view == nil || len(issueViewLayers) <= 1 {
		return false
	}
	cur := s.view.name()
	idx := -1
	for i, l := range issueViewLayers {
		if l.name == cur {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false
	}
	s.clearSelection()
	next := issueViewLayers[(idx+1)%len(issueViewLayers)]
	s.view = next.builder(s)
	return true
}

// applySort reorders the supplied issues slice in place according
// to s.sort. Stable so secondary columns aren't reshuffled when a
// tie breaks; cheap enough that callers can re-invoke any time the
// comparator or collection changes.
func (s *issuesState) applySort(issues []issue) {
	switch s.sort {
	case issueSortByNumber:
		sort.SliceStable(issues, func(i, j int) bool {
			return issues[i].number < issues[j].number
		})
	}
}

// setView swaps the active sub-view, fitting the new view to the
// previous one's dimensions so the user doesn't see a one-frame layout
// glitch on swap.
func (s *issuesState) setView(v issueView) {
	s.view = v
}

// issueScreenChromeBase is the row budget the screen always reserves
// around the active sub-view: header line + spacer above + hint line
// below. Optional chrome, such as the search row, is layered on top
// by issueScreenChromeHeight so the body does not lose a line when
// the extra row is absent.
const issueScreenChromeBase = 3

func issueScreenChromeHeight(s *issuesState) int {
	chrome := issueScreenChromeBase
	if s != nil && s.search != nil {
		chrome++
	}
	return chrome
}

// issueScreenIndent is the left margin every line on the issues screen
// shares — matching the chat side's outputStyle (MarginLeft(5)) so the
// list and detail bodies don't sit flush against the terminal edge.
// Sub-views are sized for `width - issueScreenIndent` and rendered
// flush-left; the screen handler prefixes the whole composed body with
// spaces so the indent applies uniformly to header, body, and hint.
const issueScreenIndent = 5

// indentLines prefixes every line of s with n spaces. Used by the
// issues screen to apply a single, consistent left margin across the
// whole composed body (table/viewport included) without each sub-view
// having to bake the indent into its own widgets — keeps the bubbles
// table and viewport thinking they own the full width they were sized
// for.
func indentLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// clearSelection drops both an in-flight drag and any finalized
// selection. Called from screen handlers (Esc, screen swap,
// kanban nav-key) and from the right-click copy path so the
// highlight disappears the instant copy completes.
func (s *issuesState) clearSelection() {
	s.selDragging = false
	s.selActive = false
	s.selAnchor = cellPos{}
	s.selFocus = cellPos{}
}

// selectionRange returns the normalized inclusive bounds of the live
// or finalized selection. ok=false means there's nothing to render or
// copy. Mirrors the chat-side selectionRange but reads from
// issuesState fields so selection state stays per-screen.
func (s *issuesState) selectionRange() (selectionBounds, bool) {
	if !s.selDragging && !s.selActive {
		return selectionBounds{}, false
	}
	if s.selAnchor == s.selFocus {
		return selectionBounds{}, false
	}
	a, b := s.selAnchor, s.selFocus
	if a.row > b.row || (a.row == b.row && a.col > b.col) {
		a, b = b, a
	}
	return selectionBounds{
		minRow: a.row, minCol: a.col,
		maxRow: b.row, maxCol: b.col,
	}, true
}

// selectionMask returns the inclusive-start / exclusive-end column
// range to paint with the selection background on a given content
// row. Block-selection semantics: first row from minCol, last row up
// to maxCol, middle rows full width. Unlike the chat-side mask there
// is no left-margin clamp here — the issues body has no decorative
// gutter, the screen-level indent is applied after this mask runs.
func (s *issuesState) selectionMask(contentRow, lineWidth int) (start, end int, ok bool) {
	b, hasRange := s.selectionRange()
	if !hasRange {
		return 0, 0, false
	}
	if contentRow < b.minRow || contentRow > b.maxRow {
		return 0, 0, false
	}
	switch {
	case b.minRow == b.maxRow:
		start = b.minCol
		end = b.maxCol + 1
	case contentRow == b.minRow:
		start = b.minCol
		end = lineWidth
	case contentRow == b.maxRow:
		start = 0
		end = b.maxCol + 1
	default:
		start = 0
		end = lineWidth
	}
	end = min(end, lineWidth)
	if start < 0 {
		start = 0
	}
	if end <= start {
		return 0, 0, false
	}
	return start, end, true
}

// buildCopyText assembles the clipboard payload for the current
// selection by walking selectableBody() row-by-row and slicing each
// line to the column range selectionMask returns. ANSI is stripped so
// the user gets the displayed glyphs, not styled bytes. Rows past the
// end of the body slice copy as empty lines so the height of the
// payload matches the selection rectangle.
func (s *issuesState) buildCopyText() string {
	b, ok := s.selectionRange()
	if !ok {
		return ""
	}
	body := strings.Split(s.view.selectableBody(), "\n")
	rows := make([]string, 0, b.maxRow-b.minRow+1)
	for r := b.minRow; r <= b.maxRow; r++ {
		if r < 0 || r >= len(body) {
			rows = append(rows, "")
			continue
		}
		line := body[r]
		lineW := lipgloss.Width(line)
		start, end, ok := s.selectionMask(r, lineW)
		if !ok {
			rows = append(rows, "")
			continue
		}
		rows = append(rows, xansi.Strip(xansi.Cut(line, start, end)))
	}
	return strings.Join(rows, "\n")
}

// applyIssuesSelectionHighlight paints the selection background over
// the visible body slice. Each visible line at index i corresponds to
// content row i + selectionYOffset, so a scrolled detail view's
// selection highlight tracks the correct rows under the user's cursor.
func applyIssuesSelectionHighlight(s *issuesState, lines []string) []string {
	if !s.selDragging && !s.selActive {
		return lines
	}
	yOff := s.view.selectionYOffset()
	style := selectionStyle()
	out := make([]string, len(lines))
	for i, line := range lines {
		contentRow := i + yOff
		lineW := lipgloss.Width(line)
		start, end, ok := s.selectionMask(contentRow, lineW)
		if !ok {
			out[i] = line
			continue
		}
		out[i] = lipgloss.StyleRanges(line, lipgloss.NewRange(start, end, style))
	}
	return out
}

// issuesScreenToContent converts a screen-space mouse coordinate to
// the active sub-view's content cell. selectionYOffset is added so
// detail's content-row anchoring works automatically; for list it's
// 0 (selection is screen-relative, cleared on cursor move).
func issuesScreenToContent(s *issuesState, screenX, screenY int) cellPos {
	bodyRow := max(0, screenY-s.bodyTopRow)
	bodyCol := max(0, screenX-s.bodyLeftCol)
	return cellPos{row: bodyRow + s.view.selectionYOffset(), col: bodyCol}
}

// renderIssuesScrollbar produces a per-row character slice of the
// scrollbar column. Mirrors view.go's scrollbarChars but parameterised
// on raw scroll triple instead of a chatView so it works with both the
// list and detail sub-views.
func renderIssuesScrollbar(viewportH, total, yOffset int) []string {
	if viewportH <= 0 {
		return nil
	}
	visible := min(total-yOffset, viewportH)
	if visible < 0 {
		visible = 0
	}
	thumbSize := 1
	thumbStart := 0
	if total > visible && visible > 0 {
		thumbSize = viewportH * visible / total
		if thumbSize < 1 {
			thumbSize = 1
		}
		if thumbSize > viewportH {
			thumbSize = viewportH
		}
		var pct float64
		maxYOff := total - viewportH
		if maxYOff > 0 {
			pct = float64(yOffset) / float64(maxYOff)
		}
		if pct < 0 {
			pct = 0
		}
		if pct > 1 {
			pct = 1
		}
		thumbStart = int(float64(viewportH-thumbSize) * pct)
		if thumbStart < 0 {
			thumbStart = 0
		}
		if thumbStart+thumbSize > viewportH {
			thumbStart = viewportH - thumbSize
		}
	}
	out := make([]string, viewportH)
	for i := 0; i < viewportH; i++ {
		if i >= thumbStart && i < thumbStart+thumbSize {
			out[i] = scrollThumbStyle.Render("█")
		} else {
			out[i] = scrollTrackStyle.Render("│")
		}
	}
	return out
}

// issuesScreen is the screen interface implementation; state lives on
// the model (m.issues), not here, so the implementation can be
// stateless and shared across tabs.
type issuesScreen struct{}

func (issuesScreen) id() screenID { return screenIssues }

func (issuesScreen) updateKey(m model, msg tea.KeyPressMsg) (model, tea.Cmd, bool) {
	if m.issues == nil {
		m.issues = newIssuesState()
	}
	// Ctrl+D closes the current tab from any screen — keep parity with
	// askScreen so the user isn't trapped in issues.
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id), true
	}
	// Workflow picker overlay owns the keyboard when it's open. It
	// pops over the issues body on `f`; Esc closes; Enter dispatches
	// a spawnWorkflowTabMsg the app layer turns into a fresh tab.
	if m.workflowPicker != nil {
		newM, cmd := m.updateWorkflowPicker(msg)
		if mm, ok := newM.(model); ok {
			return mm, cmd, true
		}
		return m, cmd, true
	}
	// Double-Ctrl+C exit, matching ask. The first press arms
	// m.exitArmed (a tab-level field shared with ask, which is fine
	// — Ctrl+C semantics are tab-scoped). The hint line swaps to
	// "Press ctrl+c again to exit" while armed (see view()), and
	// any non-Ctrl+C key disarms below.
	isCtrlC := msg.Mod == tea.ModCtrl && msg.Code == 'c'
	if isCtrlC {
		if m.exitArmed {
			return m, closeTabCmd(m.id), true
		}
		m.exitArmed = true
		return m, nil, true
	}
	m.exitArmed = false
	// Loading and error states own the screen until they resolve. While
	// loading, Ctrl+R is honoured (cancels the in-flight + dispatches a
	// fresh load); every other key is consumed silently so an early
	// keypress can't fall through and mutate the (still-empty) list.
	// While in error state, Enter/Esc dismiss back to ask, Ctrl+R is
	// the retry affordance (clears the error + dispatches a fresh
	// load), and every other key is consumed so a stray press can't
	// whisk the user past the failure without acknowledgment.
	isCtrlR := msg.Mod == tea.ModCtrl && msg.Code == 'r'
	if m.issues.loading {
		if isCtrlR {
			return m, m.issues.reloadCurrentQuery(), true
		}
		return m, nil, true
	}
	if m.issues.loadErr != nil {
		if msg.Mod == 0 && (msg.Code == tea.KeyEnter || msg.Code == tea.KeyEsc) {
			// Same hygiene as the Ctrl+O path: drop the cache + cancel
			// in-flight on screen-leave so re-entry doesn't stack
			// duplicate chunks onto the chain.
			m.issues.discardOnLeave()
			return m.switchScreen(screenAsk), nil, true
		}
		if isCtrlR {
			// Reload from error: clear the error first so
			// reloadCurrentQuery's loading-modal swap is the active
			// overlay, not the persistent error modal.
			m.issues.loadErr = nil
			return m, m.issues.reloadCurrentQuery(), true
		}
		return m, nil, true
	}
	// Search box has top-level priority when open: it owns every
	// keypress until Esc / empty-backspace closes it. Routes the
	// dispatched search via cmd back through Update.
	if m.issues.search != nil {
		closed, cmd := m.issues.search.updateKey(m.issues, msg)
		if closed {
			m.issues.search = nil
			// First-page-of-this-query → modal + animation tick.
			// Subsequent re-fetches of an already-cached query
			// stay inline.
			if cmd != nil && !m.issues.hasAnyCachedPage(m.issues.currentQuery) {
				m.issues.loading = true
				m.issues.loadingMessage = pickLoadingMessage()
				m.issues.loadingFrame = 0
				m.issues.loadErr = nil
				cmd = tea.Batch(cmd, issueLoadingTickCmd(m.issues.tabID))
			}
			// Rebuild the active view against the new query so
			// kanban columns pick up the new spec query targets.
			m.issues.view = issueViewLayers[viewIndexForName(m.issues.view.name())].builder(m.issues)
		}
		return m, cmd, true
	}
	// Ctrl+R reloads the active query on kanban (not detail). The
	// search-box branch above already short-circuits when /-mode
	// is open, so the textinput consumes Ctrl+R as a normal key
	// when the user is typing — design choice (search-box wins)
	// so the user can keep editing without the reload yanking the
	// screen out from under them.
	if isCtrlR {
		if kv, ok := m.issues.view.(*kanbanIssueView); ok {
			kv.cancelCarry(m.issues)
			return m, m.issues.reloadCurrentQuery(), true
		}
	}
	// "/" opens the search overlay from kanban (not detail, not
	// while loading or in error state). Consume the keystroke so
	// the typed "/" doesn't bleed into the kanban layer behind
	// the box.
	if msg.Mod == 0 && msg.Code == '/' {
		if kv, ok := m.issues.view.(*kanbanIssueView); ok {
			kv.cancelCarry(m.issues)
			help := ""
			if m.issues.provider != nil {
				help = m.issues.provider.QuerySyntaxHelp()
			}
			m.issues.search = newIssueSearchBox(help)
			return m, nil, true
		}
	}
	// "f" dispatches a workflow run against the focused issue. Works
	// from kanban (focused card) and the detail view; ignored on any
	// other sub-view that doesn't surface a single issue. We
	// intercept here, before the inner view's updateKey, so the
	// modifier-naked 'f' can never get consumed by something else
	// downstream (e.g. a future fuzzy-find keybind on a card list).
	if msg.Mod == 0 && msg.Code == 'f' {
		return m.dispatchIssueWorkflow()
	}
	v, cmd, handled := m.issues.view.updateKey(m.issues, msg)
	if handled {
		m.issues.setView(v)
	}
	return m, cmd, handled
}

// dispatchIssueWorkflow resolves the issue currently in focus on the
// issues screen, then either focuses an existing workflow tab for
// that issue or pops the workflow picker. Returns (model, cmd,
// handled=true) — the screen handler always treats `f` as consumed.
func (m model) dispatchIssueWorkflow() (model, tea.Cmd, bool) {
	if m.issues == nil {
		return m, nil, true
	}
	it, ok := focusedIssue(m.issues.view)
	if !ok {
		return m, nil, true
	}
	if m.issues.provider == nil {
		return m, m.toast.show("issues provider not configured"), true
	}
	ref, err := m.issues.provider.IssueRef(m.issues.projectCfg, m.cwd, it)
	if err != nil {
		return m, m.toast.show("could not resolve issue ref: " + err.Error()), true
	}
	if tabID, alive := workflowTracker().activeTabFor(ref.Key()); alive {
		// A workflow run is already in flight for this issue. Focus
		// the existing tab rather than spawn a duplicate — same
		// `f` lift on the same issue should always land on the
		// same agent.
		return m, focusTabCmd(tabID), true
	}
	items := projectWorkflows(m.cwd)
	if len(items) == 0 {
		return m, m.toast.show("no workflows configured · ctrl+w opens the builder"), true
	}
	m = m.openWorkflowPicker(items, ref)
	return m, nil, true
}

// focusedIssue returns the issue currently in focus on the active
// sub-view. ok=false when the focus is undefined (e.g. carry mode,
// empty column, view layer that doesn't surface an issue). Pulled
// out of dispatchIssueWorkflow so the same logic can serve the
// detail view, future per-issue keybinds, and tests.
func focusedIssue(v issueView) (issue, bool) {
	switch view := v.(type) {
	case *kanbanIssueView:
		if view.carry.active {
			return issue{}, false
		}
		if view.selColIdx < 0 || view.selColIdx >= len(view.columns) {
			return issue{}, false
		}
		col := view.columns[view.selColIdx]
		if view.selRowIdx < 0 || view.selRowIdx >= len(col.loaded) {
			return issue{}, false
		}
		return col.loaded[view.selRowIdx], true
	case *issueDetailView:
		return view.issue, true
	}
	return issue{}, false
}

// focusTabCmd emits a focusTabMsg the app layer handles by switching
// to the named tab. Used by the workflow `f` path when an in-flight
// run already exists for the focused issue.
func focusTabCmd(tabID int) tea.Cmd {
	return func() tea.Msg { return focusTabMsg{tabID: tabID} }
}

func (issuesScreen) view(m model) string {
	if m.issues == nil {
		// Shouldn't happen under normal flow (newTab/newTestModel seed
		// the state on construction), but a defensive lazy-init keeps
		// the screen renderable from any entry point.
		m.issues = newIssuesState()
	}
	width := m.width
	height := m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	// Loading or error: replace the whole body area with a centered
	// modal. The chrome (header + hint) is suppressed too — there is
	// nothing meaningful to advertise above an unpopulated list, and
	// the modal carries its own dismissal hint when in error state.
	if m.issues.loading || m.issues.loadErr != nil {
		return renderIssuesOverlay(m.issues, width, height)
	}
	// Workflow picker takes the whole body on `f`. Same trick as the
	// loading overlay: replace the body, keep the user's context in
	// their head. The picker is small (one screen of pipeline names)
	// and confirm-driven so they always know where they are.
	if m.workflowPicker != nil {
		return m.renderWorkflowPicker()
	}
	// Reserve one column on the right for the scrollbar — fixed
	// allocation, even when there's no overflow, so selection /
	// click-routing math stays stable as content grows or shrinks.
	contentW := max(20, width-issueScreenIndent-1)
	contentH := max(4, height-issueScreenChromeHeight(m.issues))
	m.issues.view.resize(contentW, contentH)

	bodyView := m.issues.view.view(m.issues)
	bodyLines := strings.Split(bodyView, "\n")

	// Track the screen footprint of the body so mouse handlers can
	// translate clicks back to (sub-view content row, column). Header
	// is one line, then a blank; the optional search row sits between
	// that chrome and the body when active.
	bodyTopRow := 2
	if m.issues.search != nil {
		bodyTopRow++
	}
	s := m.issues
	s.bodyTopRow = bodyTopRow
	s.bodyContentH = len(bodyLines)
	s.bodyLeftCol = issueScreenIndent
	s.scrollbarCol = width - 1

	bodyLines = applyIssuesSelectionHighlight(s, bodyLines)

	yOff, total, viewH := m.issues.view.scroll()
	if total > viewH {
		bar := renderIssuesScrollbar(viewH, total, yOff)
		for i := range bodyLines {
			if i < len(bar) {
				bodyLines[i] += bar[i]
			}
		}
	}

	hint := m.issues.view.hint()
	if m.exitArmed {
		// Mirror ask's "press ctrl+c again to exit" affordance, but
		// swap the hint line in place since the issues screen has no
		// chat history to append a transient message to. The next
		// keypress disarms (handled in updateKey) and the hint
		// switches back automatically on the following render.
		hint = dimStyle.Render("Press ctrl+c again to exit")
	}

	var b strings.Builder
	b.WriteString(m.issues.view.header(m.issues))
	b.WriteString("\n\n")
	if m.issues.search != nil {
		m.issues.search.resize(width - issueScreenIndent)
		b.WriteString(m.issues.search.view())
		b.WriteString("\n")
	}
	b.WriteString(strings.Join(bodyLines, "\n"))
	b.WriteString("\n")
	b.WriteString(hint)
	// Single, uniform left indent applied at the screen level so the
	// table widget, viewport content, header, and hint all sit at the
	// same column. Doing it per-piece (outputStyle on each fragment)
	// produced inconsistent margins where bubbles' table/viewport
	// rendered flush-left while the header lines were indented.
	return indentLines(b.String(), issueScreenIndent)
}

// renderIssuesOverlay draws a centered loading-or-error modal in
// place of the list/kanban body. The error variant uses an
// errorFG-tinted border and carries a dismissal hint so a network
// glitch can't strand the user on a screen with no obvious exit.
func renderIssuesOverlay(s *issuesState, width, height int) string {
	width = max(width, 20)
	height = max(height, 5)
	border := lipgloss.NewStyle().Border(lipgloss.RoundedBorder())

	var box string
	if s.loadErr != nil {
		wrapW := max(width-10, 16)
		title := errStyle.Bold(true).Render("Failed to load issues")
		wrapped := lipgloss.NewStyle().Width(wrapW).Render(s.loadErr.Error())
		hint := dimStyle.Render("press enter to go back · esc dismiss")
		body := lipgloss.JoinVertical(lipgloss.Left, title, "", wrapped, "", hint)
		box = border.BorderForeground(activeTheme.errorFG).Padding(1, 3).Render(body)
	} else {
		msg := s.loadingMessage
		if msg == "" {
			msg = "Loading issues..."
		}
		// Single line: braille glyph + 2 spaces + fun message. The
		// glyph cycles every tick for the high-FPS "still alive"
		// cue; the message is stable for the duration of the load.
		// Outer Place call centers the box itself in the screen.
		spinnerGlyph := issueLoadingSpinnerFrames[s.loadingFrame%len(issueLoadingSpinnerFrames)]
		body := promptStyle.Render(spinnerGlyph + "  " + msg)
		box = border.BorderForeground(activeTheme.accent).Padding(1, 3).Render(body)
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

// issuesHandleMouse is the entry point for every mouse event when the
// issues screen is active. update.go routes here once it's confirmed
// the event came from a non-modal state and m.screen == screenIssues.
// Returning the model and a tea.Cmd matches the rest of the Update
// dispatch shape so the routing in update.go is uniform across screens.
func (m model) issuesHandleMouse(msg tea.Msg) (model, tea.Cmd) {
	if m.issues == nil {
		return m, nil
	}
	s := m.issues
	// Loading and error states cover the body; mouse events targeting
	// the (suppressed) list/kanban underneath should be no-ops so the
	// user can't drag-select against an empty surface or wheel-scroll
	// a list that isn't there.
	if s.loading || s.loadErr != nil {
		return m, nil
	}
	switch ev := msg.(type) {
	case tea.MouseWheelMsg:
		switch ev.Button {
		case tea.MouseWheelDown:
			s.view.wheel(3)
		case tea.MouseWheelUp:
			s.view.wheel(-3)
		}
		// Wheel scrolling invalidates whatever the user had highlighted —
		// for kanban it advances the focused-row cursor, for detail it
		// shifts content rows under the highlight in a way that's
		// confusing if the drag was still in flight. Drop and start
		// fresh.
		s.clearSelection()
		return m, nil

	case tea.MouseClickMsg:
		inBodyRows := ev.Y >= s.bodyTopRow && ev.Y < s.bodyTopRow+s.bodyContentH
		_, total, viewH := s.view.scroll()
		hasOverflow := total > viewH
		onScrollbar := inBodyRows && hasOverflow && ev.X == s.scrollbarCol
		switch ev.Button {
		case tea.MouseLeft:
			if onScrollbar {
				s.scrollbarDragging = true
				issuesScrollByMouse(s, ev.Y)
				return m, nil
			}
			if inBodyRows && ev.X >= s.bodyLeftCol && ev.X < s.scrollbarCol {
				s.clearSelection()
				cell := issuesScreenToContent(s, ev.X, ev.Y)
				s.selAnchor = cell
				s.selFocus = cell
				s.selDragging = true
				return m, nil
			}
		case tea.MouseRight:
			if s.selActive {
				return m.issuesCopySelectionAndClear()
			}
		}
		return m, nil

	case tea.MouseMotionMsg:
		if s.scrollbarDragging {
			issuesScrollByMouse(s, ev.Y)
			return m, nil
		}
		if s.selDragging {
			x := max(s.bodyLeftCol, min(s.scrollbarCol-1, ev.X))
			y := max(s.bodyTopRow, min(s.bodyTopRow+s.bodyContentH-1, ev.Y))
			s.selFocus = issuesScreenToContent(s, x, y)
			return m, nil
		}
		return m, nil

	case tea.MouseReleaseMsg:
		if s.scrollbarDragging {
			s.scrollbarDragging = false
		}
		if s.selDragging {
			s.selDragging = false
			if s.selAnchor == s.selFocus {
				s.clearSelection()
			} else {
				s.selActive = true
			}
		}
		return m, nil
	}
	return m, nil
}

// issuesCopySelectionAndClear is the right-click handler entry: builds
// the clipboard payload off the current selection, clears the
// highlight, and dispatches the async clipboard write + toast through
// the same copyTextCmd the chat side uses.
func (m model) issuesCopySelectionAndClear() (model, tea.Cmd) {
	if m.issues == nil {
		return m, nil
	}
	text := m.issues.buildCopyText()
	m.issues.clearSelection()
	if text == "" {
		return m, nil
	}
	return m, copyTextCmd(m.toast, text)
}

// issuesScrollByMouse maps a screen Y inside the body strip to a
// scroll target on the active sub-view, then setYOffsets it. The mid-
// thumb conversion mirrors the chat side: pct = relY / (bodyH-1),
// target = pct * (total - viewH).
func issuesScrollByMouse(s *issuesState, screenY int) {
	if s.bodyContentH <= 1 {
		return
	}
	rel := screenY - s.bodyTopRow
	if rel < 0 {
		rel = 0
	}
	if rel > s.bodyContentH-1 {
		rel = s.bodyContentH - 1
	}
	_, total, viewH := s.view.scroll()
	if total <= viewH {
		return
	}
	pct := float64(rel) / float64(s.bodyContentH-1)
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	target := int(pct * float64(total-viewH))
	s.view.setYOffset(target)
}

// issueDetailView is the read-mode detail surface for a single issue:
// glamour-rendered markdown description on top, then a separator and
// the comment thread (each comment header + glamour-rendered body)
// below. Both flow into a single bubbles viewport so the user can
// scroll the whole thing as one document — j/k/up/down/pgup/pgdn/g/G
// all "just work" because viewport's keymap covers them.
//
// parent is the layer view the user opened from (list or kanban)
// preserved as an interface so Esc / Backspace can drop the user
// back into whatever surface they were looking at — and with state
// (cursor, selected card) intact, since we hand the same instance
// back rather than a fresh one.
type issueDetailView struct {
	parent issueView
	issue  issue

	vp viewport.Model

	// width/height mirror the last resize call. rendered + renderedFor
	// cache the glamour output keyed on width so a window resize
	// re-flows once and a steady-state scroll doesn't re-render every
	// frame.
	width  int
	height int

	rendered    string
	renderedFor int
}

func newIssueDetailView(parent issueView, it issue, width, height int) *issueDetailView {
	v := &issueDetailView{
		parent: parent,
		issue:  it,
		vp:     viewport.New(),
	}
	v.resize(width, height)
	return v
}

func (v *issueDetailView) name() string { return "detail" }

func (v *issueDetailView) resize(width, height int) {
	width = max(20, width)
	v.width = width
	v.height = max(4, height)
	v.vp.SetWidth(width)
	v.vp.SetHeight(v.height)
	if v.renderedFor != width || v.rendered == "" {
		v.rendered = v.renderBody(width)
		v.renderedFor = width
		v.vp.SetContent(v.rendered)
	}
}

// renderBody composes the description and comments into one
// glamour-rendered string sized to width. Falls back to the raw text
// when glamour returns an error so the user always sees content even
// if the markdown is malformed.
func (v *issueDetailView) renderBody(width int) string {
	r := newRenderer(width)
	desc := strings.TrimSpace(v.issue.description)
	if desc == "" {
		desc = "_(no description)_"
	}
	body, err := r.Render(desc)
	if err != nil {
		body = desc
	}

	var b strings.Builder
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n")

	if len(v.issue.comments) == 0 {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("(no comments)"))
		return b.String()
	}

	// All chrome inside the body sits at column 0 and inherits the
	// screen-level indent. The separator's width matches the body
	// width so it spans the indented column up to the right edge.
	sepW := max(8, width-2)
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", sepW)))
	b.WriteString("\n\n")
	b.WriteString(promptStyle.Render(
		fmt.Sprintf("Comments (%d)", len(v.issue.comments))))
	b.WriteString("\n\n")

	for i, c := range v.issue.comments {
		if i > 0 {
			b.WriteString("\n")
		}
		head := fmt.Sprintf("%s · %s",
			c.author, c.createdAt.Format("2006-01-02"))
		b.WriteString(dimStyle.Render(head))
		b.WriteString("\n")
		body, err := r.Render(strings.TrimSpace(c.body))
		if err != nil {
			body = c.body
		}
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

func (v *issueDetailView) updateKey(s *issuesState, msg tea.KeyPressMsg) (issueView, tea.Cmd, bool) {
	// Esc / Backspace return to the kanban view we came from; parent
	// is the same kanbanIssueView instance so its column/row cursor
	// is preserved across the round trip. Defensive fallback rebuilds
	// kanban from scratch on the (impossible-under-normal-flow)
	// no-parent path so detail can't strand the user.
	if msg.Mod == 0 && (msg.Code == tea.KeyEsc || msg.Code == tea.KeyBackspace) {
		if v.parent != nil {
			return v.parent, nil, true
		}
		return newKanbanIssueView(s), nil, true
	}
	vp, cmd := v.vp.Update(msg)
	v.vp = vp
	return v, cmd, true
}

func (v *issueDetailView) view(s *issuesState) string {
	return v.vp.View()
}

func (v *issueDetailView) header(s *issuesState) string {
	prefix, hasPrefix := workflowKeyPrefix(s)
	glyph := workflowStatusForIssue(s, prefix, hasPrefix, v.issue.number)
	header := promptStyle.Render(fmt.Sprintf("Issue #%d", v.issue.number))
	if glyph != "" {
		header = glyph + " " + header
	}
	return header + dimStyle.Render("  · ") + v.issue.title
}

func (v *issueDetailView) hint() string {
	return dimStyle.Render(
		"↑/↓ scroll · pgup/pgdn page · f run workflow · esc/backspace back · ctrl+o back to ask",
	)
}

func (v *issueDetailView) scroll() (int, int, int) {
	return v.vp.YOffset(), lipgloss.Height(v.rendered), v.vp.Height()
}

func (v *issueDetailView) setYOffset(n int) {
	v.vp.SetYOffset(n)
}

func (v *issueDetailView) wheel(delta int) {
	switch {
	case delta > 0:
		v.vp.ScrollDown(delta)
	case delta < 0:
		v.vp.ScrollUp(-delta)
	}
}

// selectableBody is the *full* glamour-rendered body, not just the
// visible slice. Selection content rows are absolute, so the copy
// path needs the full string to slice past v.vp.YOffset() correctly.
func (v *issueDetailView) selectableBody() string { return v.rendered }

func (v *issueDetailView) selectionYOffset() int { return v.vp.YOffset() }

// kanbanIssueView lays issues out as a tab strip across the top
// (one tab per provider-defined column, the focused one
// highlighted) with the focused column's cards rendered
// full-width below. Column taxonomy is supplied by
// provider.KanbanColumns() — never inferred from data — so
// columns the user expects (Open / Closed: completed / Closed:
// not planned / Closed: duplicate for GitHub) appear even when
// they're empty.
//
// Each column carries its own page state (page, hasMore,
// fetching) and dispatches loadIssuesPageCmd independently. All
// columns share the same queryGen so a search-box query applied
// while on kanban supersedes every column's in-flight load
// cleanly.
//
// (We tried a side-by-side wide layout first; the column picker
// turned out to read better at every terminal width because each
// card gets the full body width instead of being squeezed into
// 18-30 cols. Dropped the wide path entirely — one render mode is
// simpler to reason about and resize-handles trivially.)
type kanbanIssueView struct {
	width, height int

	columns []kanbanColumn

	// Selection cursor in column/row coordinates. Clamped to the
	// live columns by clampSelection on rebuild and on every nav
	// event so columns disappearing (data refresh) can't strand the
	// cursor in an invalid position.
	selColIdx int
	selRowIdx int

	// carry tracks an in-flight pickup. While carrying, the picked-up
	// card is stripped from its origin column.loaded + cache and
	// drawn pinned to the top of whichever column is currently
	// focused. ←/→/Tab change the focused column under the carry;
	// Space drops; Esc cancels (re-inserts at origin). Only one
	// carry at a time — the pickup handler is a no-op when carrying
	// is already true.
	carry kanbanCarry

	// lastRendered caches the most recent body so the screen-level
	// drag-select / right-click-copy stack has something to slice.
	// Selection on kanban isn't hugely useful (cards are short and
	// already styled), but participating in the same machinery as
	// the other views keeps mouse semantics uniform.
	lastRendered string
}

// kanbanCarry is the carry-mode bookkeeping. Lives on the view
// (not on issuesState) because every other column-mutation field
// is view-local; centralising prevents stale carry state surviving
// a screen-leave by accident.
//
// item is the snapshot of the issue at pickup time, with the original
// status field preserved so a rollback can restore it bit-for-bit.
// originColIdx + originRowIdx record where the card was so a cancel
// (Esc, screen-leave, same-column drop) can put it back exactly there.
type kanbanCarry struct {
	active       bool
	item         issue
	originStatus string
	originColIdx int
	originRowIdx int
}

// kanbanColumn is one provider-defined column. spec carries the
// label + query; loaded is the running flat list of issues
// fetched so far across chunks. nextCursor is the cursor to feed
// to the next ListIssues call (empty string both for "first
// chunk not yet fetched" and for "end of data" — disambiguate
// with hasMore + len(loaded)). hasMore reflects the latest
// fetch; fetching guards against double-dispatch while a load
// is in flight.
type kanbanColumn struct {
	spec       KanbanColumnSpec
	loaded     []issue
	nextCursor string
	hasMore    bool
	fetching   bool
}

func newKanbanIssueView(s *issuesState) *kanbanIssueView {
	v := &kanbanIssueView{}
	v.rebuildColumnsFromSpecs(s)
	v.resize(80, 20)
	return v
}

// rebuildColumnsFromSpecs builds the column slice from the
// provider's KanbanColumns() and seeds each column's `loaded`
// from any cached chunks. Called on construction and whenever
// the provider's column list might have changed (e.g. swap to
// a different provider mid-session).
func (v *kanbanIssueView) rebuildColumnsFromSpecs(s *issuesState) {
	specs := []KanbanColumnSpec{}
	if s.provider != nil {
		specs = s.provider.KanbanColumns()
	}
	cols := make([]kanbanColumn, 0, len(specs))
	for _, spec := range specs {
		col := kanbanColumn{spec: spec}
		// Stitch any cached chunks into loaded so re-entering the
		// kanban view doesn't blank columns we've already fetched.
		// nextCursor + hasMore come from the *last* cached chunk so
		// the threshold check can decide to dispatch the next chunk
		// without re-fetching anything we already have.
		if chain := s.cachedChunks(spec.Query); len(chain) > 0 {
			for _, c := range chain {
				col.loaded = append(col.loaded, c.issues...)
			}
			last := chain[len(chain)-1]
			col.nextCursor = last.nextCursor
			col.hasMore = last.hasMore
		}
		cols = append(cols, col)
	}
	v.columns = cols
	v.clampSelection()
}

// pickupCarry strips the focused card from its column (loaded slice
// AND cached chunk chain) and stages it on v.carry for follow-on
// navigation. No-op when already carrying or when the cursor isn't
// over a real card. Returns true when a pickup actually happened so
// the caller can decide whether to fire follow-up effects.
func (v *kanbanIssueView) pickupCarry(s *issuesState) bool {
	if v.carry.active {
		return false
	}
	if v.selColIdx < 0 || v.selColIdx >= len(v.columns) {
		return false
	}
	col := &v.columns[v.selColIdx]
	if v.selRowIdx < 0 || v.selRowIdx >= len(col.loaded) {
		return false
	}
	it := col.loaded[v.selRowIdx]
	v.carry = kanbanCarry{
		active:       true,
		item:         it,
		originStatus: it.status,
		originColIdx: v.selColIdx,
		originRowIdx: v.selRowIdx,
	}
	col.loaded = append(col.loaded[:v.selRowIdx], col.loaded[v.selRowIdx+1:]...)
	s.removeIssueFromCache(col.spec.Query, it.number)
	v.clampSelection()
	return true
}

// dropCarry commits the carry into the currently-focused column. A
// same-column drop is a no-op (re-inserts the issue at origin without
// any provider call); a cross-column drop applies the optimistic
// move locally and returns a tea.Cmd that dispatches the provider's
// MoveIssue. Returns (cmd, dropped=true) only when carry was active.
func (v *kanbanIssueView) dropCarry(s *issuesState, tabID int) (tea.Cmd, bool) {
	if !v.carry.active {
		return nil, false
	}
	if v.selColIdx == v.carry.originColIdx {
		v.cancelCarry(s)
		return nil, true
	}
	if v.selColIdx < 0 || v.selColIdx >= len(v.columns) {
		v.cancelCarry(s)
		return nil, true
	}
	target := v.columns[v.selColIdx].spec
	// Snapshot every value the async cmd needs BEFORE we mutate
	// v.carry — by the time the closure runs we'd otherwise read
	// the zero-valued struct.
	originIdx := v.carry.originColIdx
	originRow := v.carry.originRowIdx
	targetIdx := v.selColIdx
	originalSnapshot := v.carry.item

	moved := v.carry.item
	if newStatus := s.provider.KanbanIssueStatus(target); newStatus != "" {
		moved.status = newStatus
	}
	col := &v.columns[v.selColIdx]
	col.loaded = append([]issue{moved}, col.loaded...)
	s.insertIssueIntoCache(target.Query, moved, 0)
	v.carry = kanbanCarry{}
	v.selRowIdx = 0

	provider := s.provider
	pc := s.projectCfg
	cwd := s.cwd
	cmd := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), issueMoveTimeout)
		defer cancel()
		err := provider.MoveIssue(ctx, pc, cwd, originalSnapshot, target)
		return issueMoveDoneMsg{
			tabID:        tabID,
			issueNumber:  originalSnapshot.number,
			originSnap:   originalSnapshot,
			originColIdx: originIdx,
			originRowIdx: originRow,
			targetColIdx: targetIdx,
			err:          err,
		}
	}
	return cmd, true
}

// cancelCarry restores the carried issue to its origin column.loaded
// at originRowIdx and the cached chunk chain at the same flat index,
// then clears v.carry. Used by Esc, same-column drop, search-box
// open, Ctrl+R reload, and screen-leave paths.
func (v *kanbanIssueView) cancelCarry(s *issuesState) {
	if !v.carry.active {
		return
	}
	idx := v.carry.originColIdx
	if idx < 0 || idx >= len(v.columns) {
		v.carry = kanbanCarry{}
		return
	}
	col := &v.columns[idx]
	it := v.carry.item
	it.status = v.carry.originStatus
	row := v.carry.originRowIdx
	if row < 0 {
		row = 0
	}
	if row > len(col.loaded) {
		row = len(col.loaded)
	}
	col.loaded = append(col.loaded[:row], append([]issue{it}, col.loaded[row:]...)...)
	s.insertIssueIntoCache(col.spec.Query, it, row)
	v.selColIdx = idx
	v.selRowIdx = row
	v.carry = kanbanCarry{}
	v.clampSelection()
}

// issueMoveDoneMsg fires when provider.MoveIssue returns. err==nil
// is a silent ack — the optimistic state already reflects the move.
// err!=nil triggers a rollback: the moved issue is yanked out of the
// target column and re-inserted at originRowIdx in the origin column,
// status restored from the pre-mutation snapshot, and a toast surfaces
// the underlying provider error.
type issueMoveDoneMsg struct {
	tabID        int
	issueNumber  int
	originSnap   issue
	originColIdx int
	originRowIdx int
	targetColIdx int
	err          error
}

func (v *kanbanIssueView) name() string { return "kanban" }

// clampSelection pulls col/row back into bounds after a column
// disappears or a column's issue count shrinks. Called from
// rebuildColumnsFromSpecs and from any nav handler that could
// leave the cursor stranded.
func (v *kanbanIssueView) clampSelection() {
	if len(v.columns) == 0 {
		v.selColIdx, v.selRowIdx = 0, 0
		return
	}
	if v.selColIdx >= len(v.columns) {
		v.selColIdx = len(v.columns) - 1
	}
	if v.selColIdx < 0 {
		v.selColIdx = 0
	}
	col := v.columns[v.selColIdx]
	if v.selRowIdx >= len(col.loaded) {
		v.selRowIdx = max(0, len(col.loaded)-1)
	}
	if v.selRowIdx < 0 {
		v.selRowIdx = 0
	}
}

// columnByQueryFingerprint returns the column whose spec.Query
// matches fp, or -1 when no column matches. Used by the
// page-loaded message router so a fetch result lands in the
// right column even if the user has tabbed since dispatch.
func (v *kanbanIssueView) columnByQueryFingerprint(s *issuesState, fp string) int {
	for i, c := range v.columns {
		if s.queryFingerprint(c.spec.Query) == fp {
			return i
		}
	}
	return -1
}

// initialLoad fires one loadIssuesPageCmd per column whose
// `loaded` slice is empty and which isn't already fetching, in
// parallel via tea.Batch. All commands share s.queryGen so a
// search-box query lands consistently across columns. Each
// dispatched column flips fetching=true so a re-render or
// re-cycle to kanban can't double-fire. The first chunk request
// is always cursor="" — providers treat the empty cursor as
// "give me the first chunk". Returns nil when every column is
// already loaded or in flight.
func (v *kanbanIssueView) initialLoad(s *issuesState) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(v.columns))
	for i := range v.columns {
		col := &v.columns[i]
		if col.fetching || len(col.loaded) > 0 {
			continue
		}
		col.fetching = true
		cmds = append(cmds, loadIssuesPageCmd(
			s.loadCtx, s.tabID, s.provider, s.projectCfg, s.cwd,
			col.spec.Query, IssuePagination{Cursor: "", PerPage: githubDefaultPerPage},
			s.queryGen,
		))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// maybeFetchNextPage returns a tea.Cmd that fetches the next
// chunk of the focused column when the threshold has been
// crossed. Returns nil when no fetch is needed. Marks the
// column's fetching=true so the threshold doesn't refire while
// the network round-trip is in flight.
func (v *kanbanIssueView) maybeFetchNextPage(s *issuesState) tea.Cmd {
	idx := v.selColIdx
	if !v.shouldFetchNextPageForColumn(idx) {
		return nil
	}
	col := &v.columns[idx]
	cursor := col.nextCursor
	col.fetching = true
	return loadIssuesPageCmd(
		s.loadCtx, s.tabID, s.provider, s.projectCfg, s.cwd,
		col.spec.Query, IssuePagination{Cursor: cursor, PerPage: githubDefaultPerPage},
		s.queryGen,
	)
}

// shouldFetchNextPageForColumn reports whether the focused column
// should dispatch a next-chunk fetch: cursor crossed 50% of the
// loaded rows AND HasMore is true AND nextCursor is non-empty
// (we never round-trip an empty cursor mid-chain — that's the
// "first chunk" sentinel) AND no fetch is in flight.
func (v *kanbanIssueView) shouldFetchNextPageForColumn(idx int) bool {
	if idx < 0 || idx >= len(v.columns) {
		return false
	}
	col := v.columns[idx]
	if !col.hasMore || col.fetching || len(col.loaded) == 0 {
		return false
	}
	if col.nextCursor == "" {
		return false
	}
	if v.selRowIdx < len(col.loaded)/2 {
		return false
	}
	return true
}

func (v *kanbanIssueView) resize(width, height int) {
	width = max(20, width)
	v.width = width
	v.height = max(4, height)
}

func (v *kanbanIssueView) header(s *issuesState) string {
	total := 0
	for _, c := range v.columns {
		total += len(c.loaded)
	}
	return promptStyle.Render("Issues") +
		dimStyle.Render(fmt.Sprintf("  (%d) — kanban view", total))
}

func (v *kanbanIssueView) hint() string {
	if v.carry.active {
		return dimStyle.Render("space drop · ←/→/tab change column · esc cancel · other keys disabled while carrying")
	}
	return dimStyle.Render(
		"↑/↓ row · ←/→/tab column · enter open · space pick up · f run workflow · / search · ctrl+r reload · ctrl+o back",
	)
}

func (v *kanbanIssueView) updateKey(s *issuesState, msg tea.KeyPressMsg) (issueView, tea.Cmd, bool) {
	if msg.Mod != 0 {
		return v, nil, false
	}
	// Selection is screen-relative on kanban; any nav key drops the
	// drag-select highlight so the new layout doesn't keep painting
	// the old rectangle.
	if isKanbanNav(msg) {
		s.clearSelection()
	}
	if v.carry.active {
		return v.updateKeyCarrying(s, msg)
	}
	switch msg.Code {
	case ' ':
		v.pickupCarry(s)
		return v, nil, true
	case tea.KeyEnter:
		if v.selColIdx >= len(v.columns) {
			return v, nil, true
		}
		col := v.columns[v.selColIdx]
		if v.selRowIdx >= len(col.loaded) {
			return v, nil, true
		}
		return newIssueDetailView(v, col.loaded[v.selRowIdx], v.width, v.height), nil, true
	case tea.KeyUp, 'k':
		if v.selRowIdx > 0 {
			v.selRowIdx--
		}
		return v, nil, true
	case tea.KeyDown, 'j':
		if v.selColIdx < len(v.columns) {
			col := v.columns[v.selColIdx]
			if v.selRowIdx+1 < len(col.loaded) {
				v.selRowIdx++
			}
		}
		// Threshold check: dispatch the next-page cmd for this
		// column when the cursor crosses 50%.
		return v, v.maybeFetchNextPage(s), true
	case tea.KeyLeft, 'h':
		if v.selColIdx > 0 {
			v.selColIdx--
			v.clampSelection()
		}
		return v, nil, true
	case tea.KeyRight, 'l':
		if v.selColIdx+1 < len(v.columns) {
			v.selColIdx++
			v.clampSelection()
		}
		return v, nil, true
	case tea.KeyTab:
		if len(v.columns) > 0 {
			v.selColIdx = (v.selColIdx + 1) % len(v.columns)
			v.clampSelection()
		}
		return v, nil, true
	case 'g':
		v.selRowIdx = 0
		return v, nil, true
	case 'G':
		if v.selColIdx < len(v.columns) {
			v.selRowIdx = max(0, len(v.columns[v.selColIdx].loaded)-1)
		}
		return v, nil, true
	}
	return v, nil, true
}

// updateKeyCarrying owns the keymap while a card is in flight.
// j/k/Up/Down/g/G/Enter are absorbed silently — the carried card is
// the focus, rows under it are decoration. ←/→/Tab cycle the focused
// column so the carry follows the user across tabs. Space drops;
// Esc cancels.
func (v *kanbanIssueView) updateKeyCarrying(s *issuesState, msg tea.KeyPressMsg) (issueView, tea.Cmd, bool) {
	switch msg.Code {
	case ' ':
		cmd, _ := v.dropCarry(s, s.tabID)
		return v, cmd, true
	case tea.KeyEsc:
		v.cancelCarry(s)
		return v, nil, true
	case tea.KeyLeft, 'h':
		if v.selColIdx > 0 {
			v.selColIdx--
		}
		return v, nil, true
	case tea.KeyRight, 'l':
		if v.selColIdx+1 < len(v.columns) {
			v.selColIdx++
		}
		return v, nil, true
	case tea.KeyTab:
		if len(v.columns) > 0 {
			v.selColIdx = (v.selColIdx + 1) % len(v.columns)
		}
		return v, nil, true
	}
	// Every other key (j/k/Up/Down/g/G/Enter/etc.) is absorbed so a
	// stray press can't corrupt the carry.
	return v, nil, true
}

// isKanbanNav reports whether the keypress is going to move the
// kanban cursor (so the screen handler can drop a stale selection
// highlight). Enter opens detail, which also voids the highlight.
// Space and Esc participate too once carry mode lands — both flip
// the view's focus state in a way that supersedes any active text
// selection rectangle.
func isKanbanNav(msg tea.KeyPressMsg) bool {
	switch msg.Code {
	case tea.KeyUp, tea.KeyDown, tea.KeyLeft, tea.KeyRight,
		tea.KeyTab, tea.KeyEnter, tea.KeyEsc,
		' ', 'h', 'j', 'k', 'l', 'g', 'G':
		return true
	}
	return false
}

func (v *kanbanIssueView) view(s *issuesState) string {
	if len(v.columns) == 0 {
		v.lastRendered = dimStyle.Render("(no issues)")
		return v.lastRendered
	}
	v.lastRendered = v.renderBody(s)
	return v.lastRendered
}

// renderBody draws a tab strip across the top with the focused
// column highlighted, then that column's cards full-width below.
// The strip is truncated if its joined width exceeds the screen so
// very narrow terminals don't break the layout — the focused tab is
// always visible because it's rendered first in the slice.
//
// Carry mode adds one row above col.loaded[]: the carried card,
// rendered with a warn-background style so the user can tell it
// apart from the normal selection cursor. The destination column's
// loaded[] still renders at index 0 below it (no row is consumed
// from the column's data — the carry is purely visual chrome).
func (v *kanbanIssueView) renderBody(s *issuesState) string {
	if v.selColIdx >= len(v.columns) {
		return ""
	}
	tabs := v.renderNarrowTabs()
	col := v.columns[v.selColIdx]
	cellStyle := lipgloss.NewStyle().
		Foreground(activeTheme.foreground).
		Width(v.width)
	selStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.inverseFG).
		Background(activeTheme.accent).
		Width(v.width)
	carryStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.darkFG).
		Background(activeTheme.warn).
		Width(v.width)

	// Resolve the issue-key prefix once per render so the per-row
	// workflow status lookup doesn't reach for `git remote get-url`
	// every card. ok=false when no provider is available or the
	// repo doesn't resolve — in that case the cards just don't
	// carry status icons (no breakage).
	keyPrefix, hasKeyPrefix := workflowKeyPrefix(s)

	lines := []string{
		tabs,
		dimStyle.Render(strings.Repeat("─", v.width)),
	}
	if v.carry.active {
		it := v.carry.item
		card := fmt.Sprintf("#%d  %s", it.number, it.title)
		card = xansi.Truncate(card, v.width, "…")
		lines = append(lines, carryStyle.Render(card))
	}
	for i, it := range col.loaded {
		card := formatIssueCard(it, keyPrefix, hasKeyPrefix, s, v.width)
		// While carrying, suppress the normal selection highlight —
		// the carry card up top is the focus.
		if !v.carry.active && i == v.selRowIdx {
			lines = append(lines, selStyle.Render(card))
		} else {
			lines = append(lines, cellStyle.Render(card))
		}
	}
	for len(lines) < v.height {
		lines = append(lines, strings.Repeat(" ", v.width))
	}
	if len(lines) > v.height {
		lines = lines[:v.height]
	}
	return strings.Join(lines, "\n")
}

// renderNarrowTabs builds the tab strip. The active tab is
// highlighted; if the joined width overflows the screen, later tabs
// are dropped (with a trailing "…" marker) so the active one is
// always visible.
func (v *kanbanIssueView) renderNarrowTabs() string {
	activeStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(activeTheme.inverseFG).
		Background(activeTheme.accent).
		Padding(0, 1)
	idleStyle := lipgloss.NewStyle().
		Foreground(activeTheme.dim).
		Padding(0, 1)

	tabs := make([]string, 0, len(v.columns))
	for i, c := range v.columns {
		label := fmt.Sprintf("%s (%d)", c.spec.Label, len(c.loaded))
		if i == v.selColIdx {
			tabs = append(tabs, activeStyle.Render(label))
		} else {
			tabs = append(tabs, idleStyle.Render(label))
		}
	}
	joined := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
	if lipgloss.Width(joined) <= v.width {
		return lipgloss.NewStyle().Width(v.width).Render(joined)
	}
	// Overflow: keep dropping tail tabs until it fits, ending with
	// an ellipsis indicator so the user knows there's more.
	ellipsis := dimStyle.Render(" …")
	for len(tabs) > 1 {
		tabs = tabs[:len(tabs)-1]
		candidate := lipgloss.JoinHorizontal(lipgloss.Top, tabs...) + ellipsis
		if lipgloss.Width(candidate) <= v.width {
			return lipgloss.NewStyle().Width(v.width).Render(candidate)
		}
	}
	return lipgloss.NewStyle().Width(v.width).Render(joined)
}

// scroll on kanban: no global vertical scroll, so report 0/0/0 and
// the screen-level scrollbar stays hidden. Per-column scrolling for
// dense backlogs is a follow-up — for the mock no column has more
// rows than fit.
func (v *kanbanIssueView) scroll() (int, int, int) { return 0, 0, v.height }

func (v *kanbanIssueView) setYOffset(int) {}

// wheel on kanban moves the selected card row inside the focused
// column. List-style scroll-by-row feels right because the rows are
// the unit of navigation, not pixel-style continuous scroll.
func (v *kanbanIssueView) wheel(delta int) {
	if v.selColIdx >= len(v.columns) {
		return
	}
	col := v.columns[v.selColIdx]
	switch {
	case delta > 0:
		if v.selRowIdx+1 < len(col.loaded) {
			v.selRowIdx++
		}
	case delta < 0:
		if v.selRowIdx > 0 {
			v.selRowIdx--
		}
	}
}

func (v *kanbanIssueView) selectableBody() string { return v.lastRendered }

// selectionYOffset returns 0 because kanban has no global vertical
// scroll today — selection coordinates are screen-relative. If
// per-column scroll lands later (each column gets its own yOffset),
// this needs to switch to whichever offset the focused column has,
// AND selectableBody must return the *full* rendered body (not just
// the visible slice) so the copy path can slice past scrolled-off
// rows. selectionYOffset and selectableBody are coupled by that
// invariant — change them together.
func (v *kanbanIssueView) selectionYOffset() int { return 0 }

// mockIssues is the seed data for the issues screen until real
// backends are wired. Numbers are deliberately non-contiguous so the
// default-sort assertion (ascending by number) is meaningful.
func mockIssues() []issue {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	return []issue{
		{
			number: 12, title: "Wire ask to GitHub Issues backend",
			assignee: "antonio", status: "open",
			createdAt: now.AddDate(0, 0, -14),
			description: `# GitHub Issues backend

We want ` + "`ask`" + ` to talk to GitHub Issues for real, not just the mock list.

## Surface

- ` + "`ask issues`" + ` should pull from the active repo's GitHub project.
- Authenticate with the user's existing ` + "`gh`" + ` CLI token when present.
- Cache issue snapshots to ` + "`~/.config/ask/issues/<repo>.json`" + ` so a cold start has *something* to render before the network round-trip lands.

## Open questions

1. Multi-repo support (` + "`/add-repo`" + `?) — out of scope for v1.
2. Pagination — every repo I checked has < 500 open issues; cursor-based seems safe.
`,
			comments: []issueComment{
				{author: "fritz", createdAt: now.AddDate(0, 0, -10),
					body: "Hot take: do we need OAuth, or are we fine piggybacking on `gh`'s token? The latter is way less scope."},
				{author: "antonio", createdAt: now.AddDate(0, 0, -9),
					body: "Piggyback for v1, full OAuth when we add ClickUp / Linear. Same path."},
				{author: "fritz", createdAt: now.AddDate(0, 0, -2),
					body: "+1. I'll prototype the read path against my own repo this week."},
			},
		},
		{
			number: 7, title: "Pick palette for issue status badges",
			assignee: "antonio", status: "planned",
			createdAt: now.AddDate(0, 0, -21),
			description: `Status badges (open / in-progress / blocked / done / planned) need consistent colours that play with **all** themes, not just the default dark one.

Proposed mapping:

- ` + "`open`" + ` → accent
- ` + "`in-progress`" + ` → warn
- ` + "`blocked`" + ` → error
- ` + "`done`" + ` → success
- ` + "`planned`" + ` → dim

We probably want a small ` + "`statusStyles`" + ` table keyed on status string so theme swaps just work.
`,
			comments: []issueComment{
				{author: "antonio", createdAt: now.AddDate(0, 0, -19),
					body: "Going with theme tokens (accent/warn/error/etc.) so high-contrast themes don't look broken."},
			},
		},
		{
			number: 23, title: "Kanban view skeleton (collapsible columns)",
			assignee: "unassigned", status: "planned",
			createdAt: now.AddDate(0, 0, -3),
			description: `## Goal

A second sub-view inside the Issues screen that lays issues out in vertical columns by status.

## Mechanics

- Same ` + "`issueView`" + ` interface as the list — drop-in.
- Each column is collapsible (chevron + count when collapsed).
- Tab cycles focus between columns; arrow keys move within a column.

The architecture in ` + "`screens.go`" + ` is already shaped for this; the heavy lift is layout math, not state.
`,
		},
		{
			number: 4, title: "Add ClickUp provider",
			assignee: "fritz", status: "open",
			createdAt: now.AddDate(0, -1, -2),
			description: `ClickUp is the second target backend after GitHub. The provider-neutral ` + "`issue`" + ` shape covers most of what ClickUp returns; the gaps:

- **Custom fields** — ClickUp tasks have arbitrary user-defined fields. Stash them in a sidecar map (` + "`extras map[string]any`" + `) keyed by field id; the list view ignores them, the detail view shows them under a "Custom fields" section.
- **Subtasks** — model later. For v1, flatten and treat each subtask as its own issue.
`,
			comments: []issueComment{
				{author: "fritz", createdAt: now.AddDate(0, 0, -28),
					body: "I have an API token and a sandbox space. Will draft the read-only shape this week."},
			},
		},
		{
			number: 31, title: "Sort by status, then number, in flat list",
			assignee: "antonio", status: "in-progress",
			createdAt: now.AddDate(0, 0, -1),
			description: `Right now the list is sorted ascending by issue number, full stop. That's fine for triage, but day-to-day I want **status grouping** first (open at top, done at bottom) with number as the tie-breaker inside each status.

Wire it as a new ` + "`issueSort`" + ` constant; ` + "`applySort`" + ` already has the switch.
`,
		},
		{
			number: 18, title: "Render assignee avatars (kitty graphics)",
			assignee: "fritz", status: "blocked",
			createdAt: now.AddDate(0, 0, -8),
			description: `Bring up GitHub avatars in the assignee column using the Kitty graphics protocol we already use for clipboard image previews.

Blocked on a perf concern: a 50-row list would emit 50 image transmits per resize, which Kitty handles fine in steady state but hammers the terminal during typing-fast resize sequences.

Fix idea: transmit each unique avatar **once** at startup, then reference by image id from list rows. Kitty's placeholder protocol already supports this (we use it for clipboard).
`,
			comments: []issueComment{
				{author: "antonio", createdAt: now.AddDate(0, 0, -6),
					body: "If we cache by login (most assignees repeat across issues) the unique-image count is tiny."},
				{author: "fritz", createdAt: now.AddDate(0, 0, -5),
					body: "Right. I'll wire a `map[login]imageID` and only transmit on cache miss."},
			},
		},
		{
			number: 2, title: "Spec out provider-neutral issue model",
			assignee: "antonio", status: "done",
			createdAt: now.AddDate(0, -2, -5),
			description: `# Provider-neutral issue model

The goal: one ` + "`issue`" + ` shape that GitHub, ClickUp, Linear, and
GitLab can all map onto cleanly, with backend-specific extras held in
a sidecar so the list view doesn't grow per-provider knowledge.

## Required fields

| Field        | Type        | Notes                                    |
|--------------|-------------|------------------------------------------|
| ` + "`number`" + `     | ` + "`int`" + `       | Provider's canonical id (or sequence).   |
| ` + "`title`" + `      | ` + "`string`" + `    | Short summary; one line in the list.     |
| ` + "`assignee`" + `   | ` + "`string`" + `    | Login or display name; ` + "`unassigned`" + ` allowed. |
| ` + "`status`" + `     | ` + "`string`" + `    | Lowercase token: ` + "`open`/`done`/`blocked`" + `…   |
| ` + "`createdAt`" + `  | ` + "`time.Time`" + ` | UTC; the list renders ` + "`YYYY-MM-DD`" + `.        |

## Detail-only fields

` + "`description`" + ` and ` + "`comments`" + ` ride along on every
issue but are only consumed by the **detail** sub-view. Both are raw
markdown — the renderer wraps to the live width.

## Go struct (current shape)

` + "```go" + `
type issue struct {
    number      int
    title       string
    assignee    string
    status      string
    createdAt   time.Time
    description string
    comments    []issueComment
}

type issueComment struct {
    author    string
    createdAt time.Time
    body      string
}
` + "```" + `

## Sidecar for backend extras

ClickUp's custom fields and GitHub's labels don't fit the neutral
shape. They live in a separate ` + "`extras`" + ` map keyed by field
id, attached at fetch time:

` + "```go" + `
type providerExtras struct {
    backend string         // "github" | "clickup" | "linear"
    labels  []string       // GitHub-style flat label list
    fields  map[string]any // ClickUp custom fields keyed by id
}
` + "```" + `

## Status taxonomy

- ` + "`open`" + ` — needs triage or work
- ` + "`planned`" + ` — committed, not yet started
- ` + "`in-progress`" + ` — actively being worked
- ` + "`blocked`" + ` — waiting on something external
- ` + "`done`" + ` — closed, merged, shipped

> The taxonomy is **provider-neutral on input**. Backends with
> different vocabularies (GitHub: open/closed; ClickUp: 8 default
> states) translate via a per-backend mapping table at fetch time.

## Sort order (default)

Ascending by ` + "`number`" + `. Other comparators (status, createdAt
desc) plug in via ` + "`issueSort`" + `; see issue #31.

` + "```bash" + `
# quick sanity check from the repo
$ grep -nE 'type issue|type issueComment' issues.go
20:type issue struct {
40:type issueComment struct {
` + "```" + `

## See also

- Issue #4 — ClickUp provider (validates the sidecar shape).
- Issue #12 — GitHub backend (first real consumer of this struct).
- Issue #31 — sort-by-status (exercises ` + "`applySort`" + `).
`,
			comments: []issueComment{
				{author: "antonio", createdAt: now.AddDate(0, -1, -28),
					body: `Closing — see issues #4, #12 for the integration work that depends on this.

The ` + "`extras`" + ` sidecar is the only thing I'm still unsure about; if it grows
beyond a flat map we'll want a typed wrapper per backend.`},
				{author: "fritz", createdAt: now.AddDate(0, -1, -25),
					body: `+1 on closing. One follow-up: should ` + "`status`" + ` be a typed
` + "`enum`" + ` instead of a free-form string?

` + "```go" + `
type issueStatus int
const (
    statusOpen issueStatus = iota
    statusPlanned
    statusInProgress
    statusBlocked
    statusDone
)
` + "```" + `

Trade-off: typed enum catches typos at compile time but loses the
provider-side label (e.g. ClickUp's "needs review"). Probably defer
until we hit a concrete pain point.`},
			},
		},
		{
			number: 9, title: "Background poll with cooperative cancel",
			assignee: "unassigned", status: "open",
			createdAt: now.AddDate(0, 0, -19),
			description: `When the user is on the issues screen, a background goroutine should refresh the cache every N minutes (default 5). The refresh **must** be cooperative-cancellable so:

1. Closing the tab kills the goroutine cleanly.
2. ` + "`Ctrl+B`" + ` (provider switch) doesn't leak workers.
3. The next refresh after a successful one resets the timer; we never stack two refreshes.

` + "`context.Context`" + ` from the bridge is the obvious answer.
`,
		},
		{
			number: 27, title: "Inline issue search (/) like fzf",
			assignee: "fritz", status: "planned",
			createdAt: now.AddDate(0, 0, -2),
			description: `Press ` + "`/`" + ` from the list to open a fuzzy filter input pinned to the bottom of the screen. Match against title + assignee + status. Esc dismisses; Enter confirms the filter (it stays applied until Esc or empty input).

Should reuse the ` + "`bubbles/textinput`" + ` widget for input, and a small fuzzy match library — leaning toward ` + "`charmbracelet/x/exp/strings`" + ` since we already pull other ` + "`x/`" + ` utils.
`,
		},
		{
			number: 15, title: "Per-issue detail screen on enter",
			assignee: "antonio", status: "open",
			createdAt: now.AddDate(0, 0, -10),
			description: `Hitting Enter on a list row should open a **detail view** for that issue showing:

1. Glamour-rendered markdown body.
2. The comment thread below it, oldest-first, each rendered through glamour too.
3. ` + "`Esc`" + ` / ` + "`Backspace`" + ` returns to the list with the cursor preserved.

Live in the same ` + "`issueView`" + ` interface so the list ↔ detail swap is one line in ` + "`updateKey`" + `.
`,
			comments: []issueComment{
				{author: "antonio", createdAt: now.AddDate(0, 0, -10),
					body: "Self-assigning. This is the next thing after the architecture ships."},
				{author: "fritz", createdAt: now.AddDate(0, 0, -9),
					body: "Make sure scrollback works for long issue bodies — `bubbles/viewport` with j/k/pgup/pgdn is plenty."},
			},
		},
	}
}
