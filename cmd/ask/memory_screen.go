package main

// The /memory browser: every concept the project and global scopes hold,
// weight-ranked, with the body on demand and the same reinforce /
// demote / forget operations the model has.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/memory"
)

type memorySort int

const (
	memorySortWeight memorySort = iota
	memorySortRecent
	memorySortKind
)

func (s memorySort) label() string {
	switch s {
	case memorySortRecent:
		return "recent"
	case memorySortKind:
		return "kind"
	default:
		return "weight"
	}
}

// memoryBrowserMax bounds how many concepts the browser loads.
const memoryBrowserMax = 500

type memoryBrowserState struct {
	cwd      string
	concepts []memory.Concept
	query    string
	cursor   int
	sort     memorySort
	expanded map[int64]bool
	// confirmForget is the id whose forget is armed; a second Ctrl+X
	// deletes it, any other key disarms.
	confirmForget int64
}

func (m model) openMemoryBrowser() (tea.Model, tea.Cmd) {
	if !memory.IsOpen() {
		return m, m.toast.show("memory: service is not running")
	}
	(&m).clearSelection()
	s := &memoryBrowserState{cwd: m.cwd, expanded: map[int64]bool{}}
	if err := s.reload(); err != nil {
		return m, m.toast.show("memory: " + err.Error())
	}
	m.memoryBrowser = s
	m.mode = modeMemory
	return m, nil
}

func (m model) closeMemoryBrowser() model {
	m.mode = modeInput
	m.memoryBrowser = nil
	return m
}

func (s *memoryBrowserState) reload() error {
	ctx, cancel := context.WithTimeout(context.Background(), memory.DefaultHookTimeout)
	defer cancel()
	concepts, err := memory.Default().Top(ctx, s.cwd, memoryBrowserMax)
	if err != nil {
		return err
	}
	s.concepts = concepts
	s.applySort()
	s.clampCursor()
	return nil
}

func (s *memoryBrowserState) applySort() {
	sort.SliceStable(s.concepts, func(i, j int) bool {
		a, b := s.concepts[i], s.concepts[j]
		switch s.sort {
		case memorySortRecent:
			if !a.LastTouched.Equal(b.LastTouched) {
				return a.LastTouched.After(b.LastTouched)
			}
		case memorySortKind:
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			if a.Weight != b.Weight {
				return a.Weight > b.Weight
			}
		default:
			if a.Weight != b.Weight {
				return a.Weight > b.Weight
			}
		}
		return a.ID < b.ID
	})
}

// visibleRows is the indices into concepts that match the filter, which
// looks at title, body, topic, kind, and scope.
func (s *memoryBrowserState) visibleRows() []int {
	q := strings.ToLower(strings.TrimSpace(s.query))
	out := make([]int, 0, len(s.concepts))
	for i, c := range s.concepts {
		if q == "" || strings.Contains(strings.ToLower(c.Title+" "+c.Body+" "+c.Topic+" "+c.Kind+" "+c.Scope), q) {
			out = append(out, i)
		}
	}
	return out
}

func (s *memoryBrowserState) clampCursor() {
	n := len(s.visibleRows())
	if s.cursor >= n {
		s.cursor = n - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
}

// current returns the concept under the cursor.
func (s *memoryBrowserState) current() (memory.Concept, bool) {
	rows := s.visibleRows()
	if s.cursor < 0 || s.cursor >= len(rows) {
		return memory.Concept{}, false
	}
	return s.concepts[rows[s.cursor]], true
}

// refresh replaces one concept with its stored state (after an adjust).
func (s *memoryBrowserState) refresh(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), memory.DefaultHookTimeout)
	defer cancel()
	c, err := memory.Default().Get(ctx, id)
	if err != nil {
		return
	}
	for i := range s.concepts {
		if s.concepts[i].ID == id {
			s.concepts[i] = c
			return
		}
	}
}

const memoryPageStep = 10

func (m model) updateMemoryBrowser(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.memoryBrowser
	if s == nil {
		return m.closeMemoryBrowser(), nil
	}
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	rows := s.visibleRows()
	armed := s.confirmForget
	s.confirmForget = 0
	switch {
	case msg.Code == tea.KeyEsc, msg.Mod == tea.ModCtrl && msg.Code == 'c':
		return m.closeMemoryBrowser(), nil
	case msg.Mod == tea.ModCtrl && msg.Code == 'r':
		if err := s.reload(); err != nil {
			return m, m.toast.show("memory: " + err.Error())
		}
		return m, nil
	case msg.Mod == tea.ModCtrl && msg.Code == 'x':
		return m.memoryForget(armed)
	case msg.Code == tea.KeyTab:
		s.sort = (s.sort + 1) % 3
		s.applySort()
		s.cursor = 0
		return m, nil
	case msg.Code == tea.KeyEnter:
		if c, ok := s.current(); ok {
			s.expanded[c.ID] = !s.expanded[c.ID]
		}
		return m, nil
	case msg.Mod&^tea.ModShift == 0 && msg.Text == "+":
		return m.memoryAdjust(true)
	case msg.Mod&^tea.ModShift == 0 && msg.Text == "-":
		return m.memoryAdjust(false)
	case listNavPrev(msg):
		s.cursor--
	case listNavNext(msg):
		s.cursor++
	case msg.Code == tea.KeyPgUp:
		s.cursor -= memoryPageStep
	case msg.Code == tea.KeyPgDown:
		s.cursor += memoryPageStep
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

// memoryAdjust reinforces or demotes the concept under the cursor.
func (m model) memoryAdjust(up bool) (tea.Model, tea.Cmd) {
	s := m.memoryBrowser
	c, ok := s.current()
	if !ok {
		return m, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), memory.DefaultHookTimeout)
	defer cancel()
	var applied bool
	var err error
	verb := "reinforced"
	if up {
		applied, err = memory.Default().Reinforce(ctx, c.ID)
	} else {
		verb = "demoted"
		applied, err = memory.Default().Demote(ctx, c.ID)
	}
	if err != nil {
		return m, m.toast.show("memory: " + err.Error())
	}
	s.refresh(c.ID)
	if !applied {
		return m, m.toast.show(fmt.Sprintf("memory: #%d adjusted moments ago, no change", c.ID))
	}
	updated, _ := s.current()
	return m, m.toast.show(fmt.Sprintf("memory: #%d %s (weight %.2f)", c.ID, verb, updated.Weight))
}

// memoryForget arms the delete on the first Ctrl+X and runs it when the
// same concept is still under the cursor on the second.
func (m model) memoryForget(armed int64) (tea.Model, tea.Cmd) {
	s := m.memoryBrowser
	c, ok := s.current()
	if !ok {
		return m, nil
	}
	if armed != c.ID {
		s.confirmForget = c.ID
		return m, m.toast.show(fmt.Sprintf("memory: ctrl+x again to forget #%d", c.ID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), memory.DefaultHookTimeout)
	defer cancel()
	if err := memory.Default().Forget(ctx, c.ID); err != nil {
		return m, m.toast.show("memory: " + err.Error())
	}
	delete(s.expanded, c.ID)
	if err := s.reload(); err != nil {
		return m, m.toast.show("memory: " + err.Error())
	}
	return m, m.toast.show(fmt.Sprintf("memory: #%d forgotten", c.ID))
}

func (m model) memoryOverlay(width, height int) string {
	s := m.memoryBrowser
	if s == nil || m.mode != modeMemory || width < 24 || height < 8 {
		return ""
	}
	mx, my := modelPickerMargins(width, height)
	innerW := max(width-2*mx-modelPickerBoxStyle.GetHorizontalFrameSize(), 24)
	innerH := max(height-2*my-modelPickerBoxStyle.GetVerticalFrameSize(), 6)
	return modelPickerBoxStyle.Render(strings.Join(m.renderMemoryLines(s, innerW, innerH), "\n"))
}

const (
	memoryColID     = 6
	memoryColWeight = 5
	memoryColKind   = 9
	memoryColScope  = 6
	memoryColTopic  = 14
	memoryBodyLines = 6
)

// memoryCols lays a row out into the fixed column grid: id, weight,
// kind, scope, topic, then the title in whatever width is left.
func memoryCols(id, weight, kind, scope, topic, title string, w int) string {
	head := padRight(id, memoryColID) + " " +
		padLeft(weight, memoryColWeight) + "  " +
		padRight(kind, memoryColKind) + " " +
		padRight(scope, memoryColScope) + " " +
		padRight(clipText(topic, memoryColTopic), memoryColTopic) + "  "
	return head + clipText(title, max(w-len([]rune(head)), 4))
}

// memoryRenderRow is one visible line: a concept row or one of its body
// lines when expanded.
type memoryRenderRow struct {
	idx      int
	bodyLine string
	isBody   bool
}

func (s *memoryBrowserState) renderRows() []memoryRenderRow {
	var out []memoryRenderRow
	for _, idx := range s.visibleRows() {
		out = append(out, memoryRenderRow{idx: idx})
		c := s.concepts[idx]
		if !s.expanded[c.ID] || c.Body == c.Title {
			continue
		}
		lines := strings.Split(strings.TrimSpace(c.Body), "\n")
		if len(lines) > memoryBodyLines {
			lines = append(lines[:memoryBodyLines], "…")
		}
		for _, line := range lines {
			out = append(out, memoryRenderRow{idx: idx, bodyLine: line, isBody: true})
		}
	}
	return out
}

func (m model) renderMemoryLines(s *memoryBrowserState, w, h int) []string {
	lines := make([]string, 0, h)
	scope := shortCwdOf(s.cwd)
	if scope == "" {
		scope = "?"
	}
	lines = append(lines, spread(promptStyle.Render("Memory · "+scope+" + global"),
		dimStyle.Render(fmt.Sprintf("%d concepts", len(s.concepts))), w))
	lines = append(lines, spread(
		configPromptStyle.Render("> ")+filterPromptLine(s.query, "filter concepts"),
		dimStyle.Render("sort: "+s.sort.label()), w))
	lines = append(lines, dimStyle.Render(clipText(
		memoryCols("  ID", "W", "KIND", "SCOPE", "TOPIC", "TITLE", w), w)))

	rowsH := max(h-len(lines)-1, 1)
	rows := s.renderRows()
	if len(rows) == 0 {
		lines = append(lines, dimStyle.Render("  (no concepts stored for this project yet)"))
	}
	// The cursor addresses concept rows; find its line so the window
	// keeps it in view even with bodies expanded above it.
	cursorLine := 0
	seen := -1
	for i, r := range rows {
		if r.isBody {
			continue
		}
		seen++
		if seen == s.cursor {
			cursorLine = i
			break
		}
	}
	start, end := modelPickerWindow(len(rows), cursorLine, rowsH)
	for i := start; i < end; i++ {
		lines = append(lines, s.renderMemoryRow(rows[i], w, i == cursorLine))
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	if len(lines) > h-1 {
		lines = lines[:h-1]
	}
	help := "↑↓ move · enter body · + reinforce · - demote · ctrl+x forget · tab sort · type to filter · esc"
	visible := len(s.visibleRows())
	if visible > rowsH {
		help = fmt.Sprintf("%d/%d · ", s.cursor+1, visible) + help
	}
	lines = append(lines, themePickerHelpStyle.Render(clipText(help, w)))
	return lines
}

func (s *memoryBrowserState) renderMemoryRow(r memoryRenderRow, w int, selected bool) string {
	if r.isBody {
		return dimStyle.Render(clipText("        "+r.bodyLine, w))
	}
	c := s.concepts[r.idx]
	prefix := "  "
	if c.Body != c.Title {
		prefix = "▸ "
		if s.expanded[c.ID] {
			prefix = "▾ "
		}
	}
	scope := "proj"
	if c.Scope == memory.ScopeGlobal {
		scope = "global"
	}
	row := memoryCols(prefix+fmt.Sprintf("#%d", c.ID), fmt.Sprintf("%.2f", c.Weight), c.Kind, scope, c.Topic, c.Title, w)
	if selected {
		return configSelectedRowStyle.Render(padRight(row, w))
	}
	return padRight(row, w)
}
