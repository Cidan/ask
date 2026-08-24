package main

import (
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/tools"
)

func TestFormatTokenCount(t *testing.T) {
	cases := map[int]string{0: "0", 512: "512", 1000: "1k", 1400: "1.4k", 1_000_000: "1M", 21_800_000: "21.8M"}
	for n, want := range cases {
		if got := formatTokenCount(n); got != want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPercent(t *testing.T) {
	if got := percent(87, 100); got != "87%" {
		t.Errorf("percent(87,100) = %q", got)
	}
	if got := percent(5, 0); got != "-" {
		t.Errorf("percent unknown raw = %q, want -", got)
	}
}

func testSavingsState() *savingsOverlayState {
	s := &savingsOverlayState{total: tools.TokenSavings{
		TotalRawTokens:   2_400_000,
		TotalSavedTokens: 1_940_000,
		ByCommand: map[string]tools.CommandSavings{
			"go test":  {Count: 37, RawTokens: 1_800_000, SavedTokens: 1_700_000},
			"go build": {Count: 8, RawTokens: 200_000, SavedTokens: 100_000},
			"git diff": {Count: 22, RawTokens: 300_000, SavedTokens: 110_000},
			"make":     {Count: 5, RawTokens: 100_000, SavedTokens: 30_000},
		},
	}}
	s.rebuild()
	return s
}

// The ledger groups into base commands that aggregate their children.
func TestSavings_GroupsByBase(t *testing.T) {
	s := testSavingsState()
	if len(s.bases) != 3 { // go, git, make
		t.Fatalf("bases = %d, want 3", len(s.bases))
	}
	// Default sort by saved: go (1.8M) > git (110k) > make (30k).
	if s.bases[0].name != "go" || s.bases[1].name != "git" || s.bases[2].name != "make" {
		t.Fatalf("base order = %v", []string{s.bases[0].name, s.bases[1].name, s.bases[2].name})
	}
	goBase := s.bases[0]
	if goBase.saved != 1_800_000 || goBase.count != 45 || len(goBase.children) != 2 {
		t.Errorf("go aggregate = saved %d count %d kids %d", goBase.saved, goBase.count, len(goBase.children))
	}
	// make ran once → a leaf, titled by its full command.
	if !s.bases[2].leaf() || s.bases[2].title() != "make" {
		t.Errorf("make should be a leaf titled 'make'; leaf=%v title=%q", s.bases[2].leaf(), s.bases[2].title())
	}
}

// Collapsed shows only base rows; expanding a base reveals its children.
func TestSavings_ExpandRevealsChildren(t *testing.T) {
	s := testSavingsState()
	if got := len(s.visibleRows()); got != 3 {
		t.Fatalf("collapsed visible rows = %d, want 3", got)
	}
	s.toggle(savingsTreeRow{baseIdx: 0}) // expand go
	rows := s.visibleRows()
	if len(rows) != 5 { // go + 2 children + git + make
		t.Fatalf("expanded visible rows = %d, want 5", len(rows))
	}
	if !rows[1].isChild || s.bases[rows[1].baseIdx].children[rows[1].childIdx].sub != "test" {
		t.Errorf("first child under go should be 'test': %+v", rows[1])
	}
	// A leaf base does not expand.
	s.toggle(savingsTreeRow{baseIdx: 2})
	if len(s.visibleRows()) != 5 {
		t.Errorf("leaf base wrongly expanded")
	}
}

// Filtering on a child term auto-expands the matching base and narrows the
// children to the match.
func TestSavings_FilterAutoExpands(t *testing.T) {
	s := testSavingsState()
	s.query = "build"
	rows := s.visibleRows()
	// Only the go base plus its 'build' child.
	if len(rows) != 2 {
		t.Fatalf("filtered rows = %d, want 2 (go + build)", len(rows))
	}
	if !rows[0].isChild == false && s.bases[rows[0].baseIdx].name != "go" {
		t.Errorf("first row should be go base")
	}
	if !rows[1].isChild || s.bases[rows[1].baseIdx].children[rows[1].childIdx].sub != "build" {
		t.Errorf("child row should be build")
	}
}

// Bars scale to the largest base saving; the top base fills the bar.
func TestSavings_RenderHasBarsAndTotals(t *testing.T) {
	m := model{savings: testSavingsState(), mode: modeSavings}
	m.savings.bases[0].expanded = true
	lines := m.renderSavingsLines(m.savings, 80, 20)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Token savings") || !strings.Contains(joined, "1.9M of 2.4M (80%)") {
		t.Errorf("header wrong:\n%s", joined)
	}
	if !strings.Contains(joined, "RUNS") || !strings.Contains(joined, "SAVED") || !strings.Contains(joined, "IMPACT") {
		t.Errorf("column header missing:\n%s", joined)
	}
	if !strings.Contains(joined, "█") { // at least one filled bar
		t.Errorf("no bar rendered:\n%s", joined)
	}
	// The impact bar is a fixed column, not full-width: no data row's bar run
	// exceeds savingsColBar cells.
	if strings.Contains(joined, strings.Repeat("█", savingsColBar+1)) {
		t.Errorf("bar exceeded its fixed column width:\n%s", joined)
	}
	// go has children → expanded chevron; git ran once → a leaf titled by
	// its full command, no chevron.
	if !strings.Contains(joined, "▾ go") {
		t.Errorf("expanded chevron missing:\n%s", joined)
	}
	if !strings.Contains(joined, "git diff") || strings.Contains(joined, "▸ git ") {
		t.Errorf("leaf git should show 'git diff' with no chevron:\n%s", joined)
	}
	if len(lines) != 20 {
		t.Errorf("render produced %d lines, want 20", len(lines))
	}
}

func TestSavings_BarFill(t *testing.T) {
	if got := savingsBarFill(1.0, 10); got != 10 {
		t.Errorf("full bar = %d, want 10", got)
	}
	if got := savingsBarFill(0, 10); got != 0 {
		t.Errorf("empty bar = %d, want 0", got)
	}
	if got := savingsBarFill(0.001, 10); got != 1 {
		t.Errorf("tiny nonzero should show 1 cell, got %d", got)
	}
}

func TestSavings_EmptyLedger(t *testing.T) {
	m := model{savings: &savingsOverlayState{total: tools.TokenSavings{ByCommand: map[string]tools.CommandSavings{}}}, mode: modeSavings}
	m.savings.rebuild()
	joined := strings.Join(m.renderSavingsLines(m.savings, 80, 12), "\n")
	if !strings.Contains(joined, "no bash token savings recorded yet") {
		t.Errorf("empty state missing:\n%s", joined)
	}
}
