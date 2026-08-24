package main

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/tools"
)

// savingsSort is the order base commands are listed in.
type savingsSort int

const (
	savingsSortSaved savingsSort = iota
	savingsSortRuns
	savingsSortName
)

func (s savingsSort) label() string {
	switch s {
	case savingsSortRuns:
		return "runs"
	case savingsSortName:
		return "name"
	default:
		return "saved"
	}
}

// savingsChild is one full command under a base (e.g. "test" under "go").
type savingsChild struct {
	sub   string
	full  string
	count int
	raw   int
	saved int
}

// savingsBase groups every command sharing a program (go, git, make …) and
// aggregates their savings; children are the individual subcommands.
type savingsBase struct {
	name     string
	count    int
	raw      int
	saved    int
	children []savingsChild
	expanded bool
}

// leaf reports whether the base ran as a single command with no distinct
// subcommands to expand into.
func (b *savingsBase) leaf() bool { return len(b.children) <= 1 }

// title is the base's label: the base name when it has subcommands, or the
// single full command when it does not (so a bare `make` reads as "make",
// not "make ▸ make").
func (b *savingsBase) title() string {
	if b.leaf() && len(b.children) == 1 {
		return b.children[0].full
	}
	return b.name
}

// savingsTreeRow is one visible line: a base row, or a child row under it.
type savingsTreeRow struct {
	baseIdx  int
	childIdx int
	isChild  bool
}

type savingsOverlayState struct {
	total  tools.TokenSavings
	bases  []savingsBase
	query  string
	cursor int
	sort   savingsSort
}

func (m model) openSavings() model {
	(&m).clearSelection()
	s := &savingsOverlayState{}
	if loaded, err := tools.LoadSavings(); err == nil {
		s.total = loaded
	}
	s.rebuild()
	m.savings = s
	m.mode = modeSavings
	return m
}

func (m model) closeSavings() model {
	m.mode = modeInput
	m.savings = nil
	return m
}

func splitBaseKey(key string) (base, sub string) {
	base, sub, _ = strings.Cut(key, " ")
	return base, sub
}

// rebuild groups the ledger into base commands + children and sorts them,
// preserving expansion state across a refresh where possible.
func (s *savingsOverlayState) rebuild() {
	expanded := map[string]bool{}
	for i := range s.bases {
		if s.bases[i].expanded {
			expanded[s.bases[i].name] = true
		}
	}

	byBase := map[string]*savingsBase{}
	for key, c := range s.total.ByCommand {
		base, sub := splitBaseKey(key)
		b := byBase[base]
		if b == nil {
			b = &savingsBase{name: base, expanded: expanded[base]}
			byBase[base] = b
		}
		b.count += c.Count
		b.raw += c.RawTokens
		b.saved += c.SavedTokens
		b.children = append(b.children, savingsChild{sub: sub, full: key, count: c.Count, raw: c.RawTokens, saved: c.SavedTokens})
	}

	s.bases = s.bases[:0]
	for _, b := range byBase {
		sort.SliceStable(b.children, func(i, j int) bool {
			if b.children[i].saved != b.children[j].saved {
				return b.children[i].saved > b.children[j].saved
			}
			return b.children[i].full < b.children[j].full
		})
		s.bases = append(s.bases, *b)
	}
	s.applySort()
	s.clampCursor()
}

func (s *savingsOverlayState) applySort() {
	sort.SliceStable(s.bases, func(i, j int) bool {
		a, b := s.bases[i], s.bases[j]
		switch s.sort {
		case savingsSortRuns:
			if a.count != b.count {
				return a.count > b.count
			}
		case savingsSortName:
			return a.name < b.name
		default:
			if a.saved != b.saved {
				return a.saved > b.saved
			}
		}
		return a.name < b.name
	})
}

// visibleRows flattens the tree into the currently-shown lines: every base
// row, plus the children of expanded bases. A non-empty query filters to
// bases (or children) whose name matches and auto-expands them.
func (s *savingsOverlayState) visibleRows() []savingsTreeRow {
	q := strings.ToLower(s.query)
	var out []savingsTreeRow
	for bi := range s.bases {
		b := &s.bases[bi]
		baseMatch := q == "" || strings.Contains(strings.ToLower(b.name), q)
		var kids []int
		for ci := range b.children {
			if q == "" || baseMatch || strings.Contains(strings.ToLower(b.children[ci].full), q) {
				kids = append(kids, ci)
			}
		}
		if q != "" && !baseMatch && len(kids) == 0 {
			continue
		}
		out = append(out, savingsTreeRow{baseIdx: bi})
		show := b.expanded || (q != "" && (baseMatch || len(kids) > 0))
		if !b.leaf() && show {
			for _, ci := range kids {
				out = append(out, savingsTreeRow{baseIdx: bi, childIdx: ci, isChild: true})
			}
		}
	}
	return out
}

func (s *savingsOverlayState) clampCursor() {
	n := len(s.visibleRows())
	if s.cursor >= n {
		s.cursor = n - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
}

// maxSaved is the largest base saving, the full-bar reference. Every bar
// (base and child) is drawn on this scale, so a child's length reads as its
// contribution against the whole.
func (s *savingsOverlayState) maxSaved() int {
	m := 0
	for i := range s.bases {
		if s.bases[i].saved > m {
			m = s.bases[i].saved
		}
	}
	return m
}

const savingsPageStep = 10

func (m model) updateSavings(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.savings
	if s == nil {
		return m.closeSavings(), nil
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	rows := s.visibleRows()
	cur := savingsTreeRow{}
	if s.cursor >= 0 && s.cursor < len(rows) {
		cur = rows[s.cursor]
	}
	switch {
	case msg.Code == tea.KeyEsc, msg.Mod == tea.ModCtrl && msg.Code == 'c':
		return m.closeSavings(), nil
	case msg.Mod == tea.ModCtrl && msg.Code == 'r':
		if loaded, err := tools.LoadSavings(); err == nil {
			s.total = loaded
			s.rebuild()
		}
		return m, nil
	case msg.Code == tea.KeyTab:
		s.sort = (s.sort + 1) % 3
		s.applySort()
		s.cursor = 0
		return m, nil
	case msg.Code == tea.KeyEnter:
		s.toggle(cur)
		return m, nil
	case msg.Code == tea.KeyRight:
		if !cur.isChild && !s.bases[cur.baseIdx].leaf() {
			s.bases[cur.baseIdx].expanded = true
		}
		return m, nil
	case msg.Code == tea.KeyLeft:
		return m.savingsCollapse(cur), nil
	case listNavPrev(msg):
		s.cursor--
	case listNavNext(msg):
		s.cursor++
	case msg.Code == tea.KeyPgUp:
		s.cursor -= savingsPageStep
	case msg.Code == tea.KeyPgDown:
		s.cursor += savingsPageStep
	case msg.Code == tea.KeyHome:
		s.cursor = 0
	case msg.Code == tea.KeyEnd:
		s.cursor = len(rows) - 1
	case msg.Code == tea.KeyBackspace:
		if s.query != "" {
			r := []rune(s.query)
			s.query = string(r[:len(r)-1])
			s.cursor = 0
		}
	default:
		if configTextInputKey(msg) {
			s.query += msg.Text
			s.cursor = 0
		}
	}
	s.clampCursor()
	return m, nil
}

// toggle flips a base row's expansion; a child row toggles its parent.
func (s *savingsOverlayState) toggle(cur savingsTreeRow) {
	if cur.baseIdx < 0 || cur.baseIdx >= len(s.bases) {
		return
	}
	b := &s.bases[cur.baseIdx]
	if b.leaf() {
		return
	}
	b.expanded = !b.expanded
}

// savingsCollapse implements Left: on an expanded base it collapses; on a
// child it collapses the parent and moves the cursor onto it.
func (m model) savingsCollapse(cur savingsTreeRow) model {
	s := m.savings
	if cur.baseIdx < 0 || cur.baseIdx >= len(s.bases) {
		return m
	}
	b := &s.bases[cur.baseIdx]
	if cur.isChild {
		b.expanded = false
		for i, r := range s.visibleRows() {
			if !r.isChild && r.baseIdx == cur.baseIdx {
				s.cursor = i
				break
			}
		}
		return m
	}
	b.expanded = false
	s.clampCursor()
	return m
}

func (m model) savingsOverlay(width, height int) string {
	s := m.savings
	if s == nil || m.mode != modeSavings || width < 24 || height < 8 {
		return ""
	}
	mx, my := modelPickerMargins(width, height)
	innerW := max(width-2*mx-modelPickerBoxStyle.GetHorizontalFrameSize(), 24)
	innerH := max(height-2*my-modelPickerBoxStyle.GetVerticalFrameSize(), 6)
	return modelPickerBoxStyle.Render(strings.Join(m.renderSavingsLines(s, innerW, innerH), "\n"))
}

const (
	savingsColRuns  = 5
	savingsColSaved = 8
	savingsColPct   = 5
	savingsColBar   = 16
)

// savingsGeometry sizes the name column and bar for width w: fixed stat and
// bar columns, a name column capped so the table stays a left-aligned block
// (rtk-gain style) rather than stretching the bar to the screen edge.
func savingsGeometry(w int) (nameCol, barW int) {
	barW = savingsColBar
	fixed := savingsColRuns + savingsColSaved + savingsColPct + 8 // 4 two-space gaps
	nameCol = min(26, w-fixed-barW)
	if nameCol < 8 {
		nameCol = 8
		barW = max(w-fixed-nameCol, 4)
	}
	return nameCol, barW
}

// savingsCols lays a row out into the fixed column grid: name, then
// right-aligned runs/saved/%, then the impact bar.
func savingsCols(name string, nameCol int, runs, saved, pct, bar string) string {
	return padRight(name, nameCol) + "  " +
		padLeft(runs, savingsColRuns) + "  " +
		padLeft(saved, savingsColSaved) + "  " +
		padLeft(pct, savingsColPct) + "  " + bar
}

func (m model) renderSavingsLines(s *savingsOverlayState, w, h int) []string {
	nameCol, barW := savingsGeometry(w)
	maxSaved := s.maxSaved()

	lines := make([]string, 0, h)
	totals := fmt.Sprintf("%s of %s (%s)",
		formatTokenCount(s.total.TotalSavedTokens), formatTokenCount(s.total.TotalRawTokens),
		percent(s.total.TotalSavedTokens, s.total.TotalRawTokens))
	lines = append(lines, spread(promptStyle.Render("Token savings"), dimStyle.Render(totals), w))
	lines = append(lines, spread(
		configPromptStyle.Render("> ")+filterPromptLine(s.query, "filter commands"),
		dimStyle.Render("sort: "+s.sort.label()), w))
	lines = append(lines, dimStyle.Render(clipText(
		savingsCols("  COMMAND", nameCol, "RUNS", "SAVED", "%", "IMPACT"), w)))

	rowsH := max(h-len(lines)-1, 1)
	rows := s.visibleRows()
	if len(rows) == 0 {
		lines = append(lines, dimStyle.Render("  (no bash token savings recorded yet)"))
	}
	start, end := modelPickerWindow(len(rows), s.cursor, rowsH)
	for i := start; i < end; i++ {
		lines = append(lines, s.renderSavingsRow(rows[i], nameCol, barW, maxSaved, w, i == s.cursor))
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	if len(lines) > h-1 {
		lines = lines[:h-1]
	}
	help := "↑↓ move · enter/→ expand · ← collapse · tab sort · type to filter · esc"
	if len(rows) > rowsH {
		help = fmt.Sprintf("%d/%d · ", s.cursor+1, len(rows)) + help
	}
	lines = append(lines, themePickerHelpStyle.Render(clipText(help, w)))
	return lines
}

func (s *savingsOverlayState) renderSavingsRow(r savingsTreeRow, nameCol, barW, maxSaved, w int, selected bool) string {
	b := &s.bases[r.baseIdx]

	var prefix, name string
	var count, raw, saved int
	if r.isChild {
		c := b.children[r.childIdx]
		prefix, name, count, raw, saved = "    ", c.sub, c.count, c.raw, c.saved
	} else {
		switch {
		case b.leaf():
			prefix = "  "
		case b.expanded:
			prefix = "▾ "
		default:
			prefix = "▸ "
		}
		name, count, raw, saved = b.title(), b.count, b.raw, b.saved
	}

	label := prefix + clipText(name, nameCol-lipgloss.Width(prefix))
	runs, savedStr, pctStr := fmt.Sprint(count), formatTokenCount(saved), percent(saved, raw)
	frac := 0.0
	if maxSaved > 0 {
		frac = float64(saved) / float64(maxSaved)
	}
	fill := savingsBarFill(frac, barW)

	if selected {
		bar := strings.Repeat("█", fill) + strings.Repeat("░", barW-fill)
		return configSelectedRowStyle.Render(padRight(savingsCols(label, nameCol, runs, savedStr, pctStr, bar), w))
	}
	bar := promptStyle.Render(strings.Repeat("█", fill)) + dimStyle.Render(strings.Repeat("░", barW-fill))
	return padRight(savingsCols(label, nameCol, runs, savedStr, pctStr, bar), w)
}

// savingsBarFill returns how many cells of a width-w bar are filled for a
// fraction, showing at least one cell for any nonzero saving.
func savingsBarFill(frac float64, w int) int {
	if frac <= 0 || w <= 0 {
		return 0
	}
	fill := int(frac*float64(w) + 0.5)
	if fill == 0 {
		fill = 1
	}
	return min(fill, w)
}

// spread places left at the start and right at the end of a w-wide line.
func spread(left, right string, w int) string {
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if lw+1+rw > w {
		left = clipText(left, w-rw-1)
		lw = lipgloss.Width(left)
	}
	return left + strings.Repeat(" ", max(w-lw-rw, 1)) + right
}

func padLeft(s string, w int) string {
	if lipgloss.Width(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-lipgloss.Width(s)) + s
}
