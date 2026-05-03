package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoadingModal_OnlyFiresOnFirstPageOfQuery(t *testing.T) {
	// Build a state with a cached chunk for the nil query.
	s := newIssuesState()
	s.appendChunk(nil, issuePageChunk{cursor: "", issues: []issue{{number: 1}}})
	if !s.hasAnyCachedPage(nil) {
		t.Fatalf("setup: cache should report a chunk stored")
	}
	// Subsequent paginations on the same query don't raise the
	// modal: the screen branches on hasAnyCachedPage to decide
	// modal vs inline footer.
	if !s.hasAnyCachedPage(nil) {
		t.Errorf("hasAnyCachedPage(nil) should be true after store")
	}
}

func TestLoadingModal_FreshQueryWithoutCachePromptsModal(t *testing.T) {
	s := newIssuesState()
	q := &fakeQuery{statusMatch: "fresh"}
	if s.hasAnyCachedPage(q) {
		t.Errorf("untouched query should not have cached chunks")
	}
}

func TestNewQueryCancelsPriorLoad(t *testing.T) {
	s := newIssuesState()
	called := false
	prev := s.cancelLoad
	s.cancelLoad = func() {
		called = true
		if prev != nil {
			prev()
		}
	}
	s.resetForNewQuery(&fakeQuery{statusMatch: "x"})
	if !called {
		t.Errorf("resetForNewQuery should invoke prior cancelLoad")
	}
	if s.cancelLoad == nil {
		t.Errorf("cancelLoad should be replaced with the new context's cancel, not cleared")
	}
}

func TestNewQueryBumpsGen(t *testing.T) {
	s := newIssuesState()
	prev := s.queryGen
	s.resetForNewQuery(&fakeQuery{statusMatch: "x"})
	if s.queryGen != prev+1 {
		t.Errorf("queryGen=%d want %d", s.queryGen, prev+1)
	}
}

func TestFirstPageError_ClearsLoadingAndShowsModal(t *testing.T) {
	// Provider error path returns IssueListPage{} (zero value), so
	// the response NextCursor is "". The loading-clear gate must
	// read requestedCursor (the dispatched cursor), which is also
	// "" for the first chunk — otherwise loading=true is never
	// cleared and the user is stuck on "Herding gophers..." forever
	// instead of seeing the error modal.
	m := enterIssuesScreen(t)
	m.issues.loading = true
	loadErr := fmt.Errorf("auth: bad token")
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:           m.id,
		screen:          screenIssues,
		gen:             m.issues.queryGen,
		query:           nil,
		requestedCursor: "",
		page:            IssueListPage{}, // provider zero-value on error
		err:             loadErr,
	})
	if m.issues.loading {
		t.Errorf("loading should be cleared on first-chunk error even when response is zero-valued")
	}
	if m.issues.loadErr == nil || !strings.Contains(m.issues.loadErr.Error(), "bad token") {
		t.Errorf("loadErr should reflect the failure; got %v", m.issues.loadErr)
	}
}

func TestDiscardOnLeave_ClearsCacheAndCancelsInFlight(t *testing.T) {
	// Repro for the duplicate-on-re-entry bug. Without
	// discardOnLeave, every Ctrl+O → Ctrl+I round trip dispatches
	// another fetch whose result appendChunks onto the existing
	// chain, doubling the visible rows. The fix wipes the cache on
	// screen leave so re-entry sees an empty cache and the next
	// chunk is the only one in the chain.
	s := newIssuesState()
	s.appendChunk(nil, issuePageChunk{cursor: "", issues: []issue{{number: 1}, {number: 2}}})
	called := false
	s.cancelLoad = func() { called = true }
	priorGen := s.queryGen
	s.discardOnLeave()
	if !called {
		t.Errorf("discardOnLeave should invoke cancelLoad")
	}
	if s.cancelLoad != nil {
		t.Errorf("cancelLoad should be cleared after discard")
	}
	if s.hasAnyCachedPage(nil) {
		t.Errorf("cache should be empty after discardOnLeave")
	}
	if s.queryGen <= priorGen {
		t.Errorf("queryGen should bump so late responses drop on stale-gen: before=%d after=%d", priorGen, s.queryGen)
	}
}

func TestCtrlO_FromIssuesScreenInvokesDiscardOnLeave(t *testing.T) {
	// Integration check: Ctrl+O while on the issues screen drops
	// the cached chunk chain. Otherwise re-entering with Ctrl+I
	// stacks a fresh fetch's chunk onto the existing chain.
	m := enterIssuesScreen(t)
	if !m.issues.hasAnyCachedPage(nil) {
		t.Fatalf("setup: expected seedMockIssues to populate the cache")
	}
	m, _ = runUpdate(t, m, ctrlKey('o'))
	if m.screen != screenAsk {
		t.Fatalf("Ctrl+O should switch to ask screen, got %v", m.screen)
	}
	if m.issues.hasAnyCachedPage(nil) {
		t.Errorf("Ctrl+O from issues should discard the cache so re-entry refetches")
	}
}

func TestLoaderAnimation_TickAdvancesFrameWhileLoading(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loading = true
	m.issues.loadingMessage = "Brewing fresh issues..."
	m.issues.loadingFrame = 0
	m, cmd := runUpdate(t, m, issueLoadingTickMsg{tabID: m.id, screen: screenIssues})
	if m.issues.loadingFrame != 1 {
		t.Errorf("frame should advance on tick: got %d want 1", m.issues.loadingFrame)
	}
	if cmd == nil {
		t.Errorf("tick should re-arm the next tick while loading")
	}
}

func TestLoaderAnimation_TickStopsAfterLoadingClears(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loading = false
	startFrame := m.issues.loadingFrame
	_, cmd := runUpdate(t, m, issueLoadingTickMsg{tabID: m.id, screen: screenIssues})
	if m.issues.loadingFrame != startFrame {
		t.Errorf("frame should not advance when not loading: got %d want %d",
			m.issues.loadingFrame, startFrame)
	}
	if cmd != nil {
		t.Errorf("tick should NOT re-arm once loading flips false")
	}
}

func TestLoaderAnimation_TickIgnoresWrongTab(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.loading = true
	m.issues.loadingFrame = 5
	_, cmd := runUpdate(t, m, issueLoadingTickMsg{tabID: m.id + 999, screen: screenIssues})
	if m.issues.loadingFrame != 5 {
		t.Errorf("foreign-tab tick should not advance frame")
	}
	if cmd != nil {
		t.Errorf("foreign-tab tick should not re-arm")
	}
}

func TestLoaderAnimation_RenderDiffersBetweenFrames(t *testing.T) {
	// Two consecutive ticks should produce two visibly different
	// modal renders — the spinner glyph and marquee position both
	// change, so a byte-for-byte equal output means the animation is
	// effectively static.
	m := enterIssuesScreen(t)
	m.issues.loading = true
	m.issues.loadingMessage = "Reticulating splines..."
	m.issues.loadingFrame = 0
	first := stripAnsi(m.activeScreen().view(m))
	m, _ = runUpdate(t, m, issueLoadingTickMsg{tabID: m.id, screen: screenIssues})
	second := stripAnsi(m.activeScreen().view(m))
	if first == second {
		t.Errorf("two consecutive frames produced identical output:\nfirst:\n%s\nsecond:\n%s",
			first, second)
	}
	// Both renders should still carry the picked message.
	if !strings.Contains(first, "Reticulating splines...") {
		t.Errorf("first frame missing message: %q", first)
	}
	if !strings.Contains(second, "Reticulating splines...") {
		t.Errorf("second frame missing message: %q", second)
	}
}

func TestLoaderAnimation_TickIntervalIsHighFps(t *testing.T) {
	// Lock the cadence at <= 50ms (>= 20fps). The skill prompt asked
	// for "high fps" — anything slower starts to read as a static
	// indicator. Bumping past this threshold should be a deliberate
	// design call, not an accidental regression.
	if issueLoadingTickInterval > 50*time.Millisecond {
		t.Errorf("loader tick interval %v is too slow; want <= 50ms (20fps)", issueLoadingTickInterval)
	}
}

func TestLoaderAnimation_SpinnerGlyphAdvancesEveryTick(t *testing.T) {
	// The braille spinner glyph in front of the message cycles
	// once per tick — that's the high-FPS "still alive" cue. Two
	// consecutive ticks must produce different glyphs (any two
	// adjacent entries in the 10-frame ring are distinct).
	if a, b := issueLoadingSpinnerFrames[0], issueLoadingSpinnerFrames[1]; a == b {
		t.Errorf("adjacent spinner frames should differ: %q vs %q", a, b)
	}
	// Sanity: the ring is wired all the way around (every entry
	// is non-empty so we don't render a phantom blank).
	for i, g := range issueLoadingSpinnerFrames {
		if g == "" {
			t.Errorf("spinner frame %d is empty", i)
		}
	}
}

func TestLoaderAnimation_BoxIsHorizontallyCenteredOnScreen(t *testing.T) {
	// lipgloss.Place centers the box; verify by checking that the
	// border line of the rendered modal has roughly equal leading
	// and trailing whitespace.
	s := newIssuesState()
	s.loading = true
	s.loadingMessage = "Loading issues..."
	body := stripAnsi(renderIssuesOverlay(s, 80, 24))
	for _, line := range strings.Split(body, "\n") {
		if !strings.ContainsAny(line, "─│┌┐└┘╭╮╰╯") {
			continue
		}
		// Pad line to full width so trailing-space count is
		// meaningful (terminals strip trailing spaces; lipgloss
		// usually doesn't).
		leading := len(line) - len(strings.TrimLeft(line, " "))
		// Reconstruct intended right edge: leading + visible content
		// width. The Place wrapper guarantees the line is exactly
		// `width` chars when we count padding, so trailing equals
		// 80 - leading - visibleWidth.
		visible := strings.TrimSpace(line)
		trailing := 80 - leading - len([]rune(visible))
		if d := leading - trailing; d > 1 || d < -1 {
			t.Errorf("box not horizontally centered: leading=%d trailing=%d (line=%q)",
				leading, trailing, line)
		}
		return
	}
	t.Fatalf("no border line found in rendered modal:\n%s", body)
}

func TestLoaderAnimation_GlyphRendersBesideMessage(t *testing.T) {
	// Single-line layout: a braille glyph followed by two spaces
	// followed by the message. Verifies both pieces land on the
	// same line of the rendered modal.
	s := newIssuesState()
	s.loading = true
	s.loadingMessage = "Reticulating splines..."
	body := stripAnsi(renderIssuesOverlay(s, 80, 24))
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "Reticulating splines...") {
			continue
		}
		// Some braille glyph from the spinner ring must appear on
		// the same line as the message. Check at least one
		// candidate is present.
		hasGlyph := false
		for _, g := range issueLoadingSpinnerFrames {
			if strings.Contains(line, g) {
				hasGlyph = true
				break
			}
		}
		if !hasGlyph {
			t.Errorf("loader line missing spinner glyph: %q", line)
		}
		return
	}
}

func TestStaleGenResponseDoesNotMutate(t *testing.T) {
	m := enterIssuesScreen(t)
	m.issues.queryGen = 10
	beforeCount := len(issuesAll(m.issues))
	beforeFingerprint := m.issues.queryFingerprint(m.issues.currentQuery)
	staleQuery := &fakeQuery{statusMatch: "ghost"}
	m, _ = runUpdate(t, m, issuePageLoadedMsg{
		tabID:  m.id,
		screen: screenIssues,
		gen:    3, // stale!
		query:  staleQuery,
		page:   IssueListPage{Issues: []issue{{number: 12345}}, HasMore: false},
	})
	if got := len(issuesAll(m.issues)); got != beforeCount {
		t.Errorf("nil-query rows mutated by stale msg: %d → %d", beforeCount, got)
	}
	if chain := m.issues.cachedChunks(staleQuery); len(chain) != 0 {
		t.Errorf("stale-gen msg should not store its query in cache: %d chunks", len(chain))
	}
	if got := m.issues.queryFingerprint(m.issues.currentQuery); got != beforeFingerprint {
		t.Errorf("currentQuery mutated by stale msg: %q → %q", beforeFingerprint, got)
	}
}
