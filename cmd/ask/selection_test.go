package main

import (
	"errors"
	"fmt"
	"image"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	xansi "github.com/charmbracelet/x/ansi"
)

// runCopyCmd executes a cmd from copyTextCmd / copyTextSilentCmd and
// splits what it produced: the OSC 52 bytes handed to Bubble Tea as a
// tea.RawMsg, and the binary-writer half's message (a toastShowMsg, or
// nil when it stayed silent).
func runCopyCmd(t *testing.T, cmd tea.Cmd) (raw string, result tea.Msg) {
	t.Helper()
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("copy cmd must batch the OSC 52 raw write with the binary write; got %T", cmd())
	}
	var sawRaw bool
	for _, c := range batch {
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case tea.RawMsg:
			if sawRaw {
				t.Fatalf("copy cmd emitted two raw writes")
			}
			sawRaw = true
			raw = msg.Msg.(string)
		default:
			result = msg
		}
	}
	if !sawRaw {
		t.Fatalf("copy cmd emitted no OSC 52 raw write")
	}
	return raw, result
}

// chatHarness drives a tab the way a user does: the production chat style,
// real frames from View, real mouse events, and the clipboard captured.
type chatHarness struct {
	t      *testing.T
	m      model
	copied string
	writes int
}

func newChatHarness(t *testing.T, goos string, width, height int) *chatHarness {
	t.Helper()
	m := newTestModel(t, newFakeProvider())
	m.chat.style = lipgloss.NewStyle().PaddingTop(1)
	m.toast = NewToastModel(40, time.Second)
	h := &chatHarness{t: t}
	withClipboardStubs(t, goos, map[string]bool{"wl-copy": true, "pbcopy": true}, func(name, stdin string, args ...string) error {
		h.copied = stdin
		h.writes++
		return nil
	})
	h.m, _ = runUpdate(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	return h
}

// draw renders the tab until its frame settles and decodes it into cells.
func (h *chatHarness) draw() uv.ScreenBuffer {
	var frame string
	for i := 0; i < 8; i++ {
		next := h.m.View().Content
		if next == frame {
			break
		}
		frame = next
	}
	buf := uv.NewScreenBuffer(h.m.width, h.m.height)
	uv.NewStyledString(frame).Draw(buf, image.Rect(0, 0, h.m.width, h.m.height))
	return buf
}

// textCols is the chat's text area: everything left of the scrollbar.
func (h *chatHarness) textCols() int { return h.m.width - 1 }

// rows is the chat viewport of a frame as plain text, one string per screen
// row, cells left to right (a wide character's trailing cell drops out).
func (h *chatHarness) rows(buf uv.ScreenBuffer) []string {
	out := make([]string, h.m.chat.Height())
	for y := range out {
		var b strings.Builder
		for x := 0; x < h.textCols(); x++ {
			if c := buf.CellAt(x, y); c != nil {
				b.WriteString(c.Content)
			}
		}
		out[y] = b.String()
	}
	return out
}

// find is the screen cell where text starts, in the current frame.
func (h *chatHarness) find(text string) (x, y int) {
	h.t.Helper()
	for y, row := range h.rows(h.draw()) {
		if i := strings.Index(row, text); i >= 0 {
			return xansi.StringWidth(row[:i]), y
		}
	}
	h.t.Fatalf("%q is not on screen:\n%s", text, strings.Join(h.rows(h.draw()), "\n"))
	return 0, 0
}

// drag presses at one cell, moves to another, and releases there — on a
// drawn frame, as the terminal sends it.
func (h *chatHarness) drag(x0, y0, x1, y1 int) {
	h.t.Helper()
	h.draw()
	h.m, _ = runUpdate(h.t, h.m, tea.MouseClickMsg{X: x0, Y: y0, Button: tea.MouseLeft})
	h.draw()
	h.m, _ = runUpdate(h.t, h.m, tea.MouseMotionMsg{X: x1, Y: y1, Button: tea.MouseLeft})
	h.draw()
	h.m, _ = runUpdate(h.t, h.m, tea.MouseReleaseMsg{X: x1, Y: y1, Button: tea.MouseLeft})
}

// sweep drags from the first character of from through the last of to, as
// they show on screen.
func (h *chatHarness) sweep(from, to string) {
	h.t.Helper()
	x0, y0 := h.find(from)
	x1, y1 := h.find(to)
	h.drag(x0, y0, x1+xansi.StringWidth(to)-1, y1)
}

// copy right-clicks and returns what reached the clipboard.
func (h *chatHarness) copy() string {
	h.t.Helper()
	h.copied = ""
	var cmd tea.Cmd
	h.m, cmd = runUpdate(h.t, h.m, tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseRight})
	if cmd != nil {
		runCopyCmd(h.t, cmd)
	}
	return h.copied
}

// painted is every viewport cell whose look the selection changed.
func (h *chatHarness) painted(before, after uv.ScreenBuffer) map[[2]int]bool {
	out := map[[2]int]bool{}
	for y := 0; y < h.m.chat.Height(); y++ {
		for x := 0; x < h.textCols(); x++ {
			a, b := before.CellAt(x, y), after.CellAt(x, y)
			if a != nil && b != nil && !reflect.DeepEqual(a.Style, b.Style) {
				out[[2]int{x, y}] = true
			}
		}
	}
	return out
}

// swept is what a terminal highlights for a drag from (x0, y0) to (x1, y1)
// over rows: the press row from the press cell to its text's end, every row
// between whole, the release row up to and including the release cell —
// never the chat's gutter, never past a row's text.
func swept(rows []string, x0, y0, x1, y1 int) map[[2]int]bool {
	if y1 < y0 || (y1 == y0 && x1 < x0) {
		x0, y0, x1, y1 = x1, y1, x0, y0
	}
	out := map[[2]int]bool{}
	for y := max(y0, 0); y <= y1 && y < len(rows); y++ {
		extent := xansi.StringWidth(strings.TrimRight(rows[y], " "))
		from, to := chatLeftMarginCols, extent
		if y == y0 {
			from = max(from, x0)
		}
		if y == y1 {
			to = min(to, x1+1)
		}
		for x := from; x < to; x++ {
			out[[2]int{x, y}] = true
		}
	}
	return out
}

func describeCells(rows []string, cells map[[2]int]bool) string {
	var b strings.Builder
	for y, row := range rows {
		var line strings.Builder
		for x, r := range []rune(row) {
			if cells[[2]int{x, y}] {
				line.WriteRune(r)
			} else if len(line.String()) > 0 {
				line.WriteRune('·')
			}
		}
		if s := strings.Trim(line.String(), "·"); s != "" {
			fmt.Fprintf(&b, "%2d|%s\n", y, s)
		}
	}
	return b.String()
}

const planReply = "## Plan\n\nWe **must** keep `go test` green, and this paragraph is long enough that it wraps across a couple of rows in the chat.\n\n- fix the gate\n- add tests\n\n```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n\nDone."

func planConversation(h *chatHarness) {
	h.m.appendUser("please explain the plan")
	h.m.appendResponse(planReply)
	h.m.appendUser("second question")
	h.m.appendResponse("Short answer here.")
	(&h.m).layout()
	h.m.chat.GotoBottom()
}

// The highlight is exactly what the pointer swept, the way a terminal draws
// a selection — dragging down, dragging up, starting in the gutter or past a
// row's end, ending on a blank row, below the last entry, or below the chat.
func TestSelection_HighlightIsWhatThePointerSwept(t *testing.T) {
	type drag struct{ x0, y0, x1, y1 int }
	h := newChatHarness(t, "linux", 90, 26)
	planConversation(h)
	rows := h.rows(h.draw())
	bottom := h.m.chat.Height() - 1
	_, plan := h.find("## Plan")
	_, fix := h.find("fix the gate")
	_, done := h.find("Done.")
	_, short := h.find("Short answer")
	cases := map[string]drag{
		"down, mid-row to mid-row":            {9, plan + 2, 20, plan + 3},
		"down from a bullet to the bottom":    {5, fix, 40, bottom},
		"up from below the last entry":        {10, bottom, 8, plan},
		"up onto a blank row":                 {12, short, 20, done + 1},
		"inside one row":                      {8, fix, 13, fix},
		"backwards inside one row":            {13, fix, 8, fix},
		"from the gutter":                     {0, plan + 2, 30, plan + 2},
		"from past a row's end":               {80, plan, 9, fix},
		"from the padding row above the text": {40, 0, 9, plan},
	}
	for name, d := range cases {
		before := h.draw()
		h.drag(d.x0, d.y0, d.x1, d.y1)
		after := h.draw()
		got := h.painted(before, after)
		y0 := d.y0
		x0 := d.x0
		if y0 == 0 {
			x0, y0 = 0, 1
		}
		want := swept(rows, x0, y0, d.x1, d.y1)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: painted\n%swant\n%s", name, describeCells(rows, got), describeCells(rows, want))
		}
		h.m.clearSelection()
	}

	before := h.draw()
	x, y := h.find("Short answer")
	h.draw()
	h.m, _ = runUpdate(t, h.m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.draw()
	h.m, _ = runUpdate(t, h.m, tea.MouseMotionMsg{X: 3, Y: h.m.height - 1, Button: tea.MouseLeft})
	got := h.painted(before, h.draw())
	if want := swept(rows, x, y, h.textCols(), bottom); !reflect.DeepEqual(got, want) {
		t.Errorf("dragging below the chat painted\n%swant\n%s", describeCells(rows, got), describeCells(rows, want))
	}
}

// Right-click copies the raw text behind exactly the characters highlighted:
// markup between them is kept, nothing outside them is, and the line breaks
// the window width put in are not.
func TestCopy_IsTheRawTextOfTheSweptCharacters(t *testing.T) {
	cases := []struct {
		from, to string
		want     string
	}{
		{"must", "must", "must"},
		{"must", "keep", "must** keep"},
		{"We", "chat.", "We **must** keep `go test` green, and this paragraph is long enough that it wraps across a couple of rows in the chat."},
		{"• fix", "add tests", "- fix the gate\n- add tests"},
		{"fix", "add tests", "fix the gate\n- add tests"},
		{`fmt.Println("hi")`, `fmt.Println("hi")`, `fmt.Println("hi")`},
		{"func main", "}", "func main() {\n\tfmt.Println(\"hi\")\n}"},
		{"## Plan", "Done.", planReply},
		{"please", "Plan", "please explain the plan\n\n## Plan"},
		{"Done.", "second", "Done.\n\nsecond"},
	}
	for _, tc := range cases {
		h := newChatHarness(t, "linux", 90, 26)
		planConversation(h)
		h.sweep(tc.from, tc.to)
		if got := h.copy(); got != tc.want {
			t.Errorf("sweeping %q..%q copied\n%q\nwant\n%q", tc.from, tc.to, got, tc.want)
		}

		x0, y0 := h.find(tc.from)
		x1, y1 := h.find(tc.to)
		h.drag(x1+xansi.StringWidth(tc.to)-1, y1, x0, y0)
		if got := h.copy(); got != tc.want {
			t.Errorf("sweeping %q..%q upward copied\n%q\nwant\n%q", tc.from, tc.to, got, tc.want)
		}
	}
}

// A selection is of what the screen shows: scrolling moves it with its text;
// a resize re-wraps the chat, so it is gone rather than pointing at other
// words.
func TestSelection_ScrollKeepsItAndResizeDropsIt(t *testing.T) {
	h := newChatHarness(t, "linux", 90, 14)
	for i := 0; i < 12; i++ {
		h.m.appendResponse(fmt.Sprintf("Reply %d with WORD%02d in it.", i, i))
	}
	(&h.m).layout()
	h.m.chat.GotoBottom()
	_, before := h.find("WORD08")
	h.sweep("WORD08", "WORD08")
	h.m, _ = runUpdate(t, h.m, tea.MouseWheelMsg{X: 5, Y: 5, Button: tea.MouseWheelUp})
	selected := h.draw()
	h.m.selActive = false
	plain := h.draw()
	h.m.selActive = true
	rows := h.rows(plain)
	x, y := h.find("WORD08")
	if y == before {
		t.Fatal("the wheel did not scroll the chat")
	}
	if got, want := h.painted(plain, selected), swept(rows, x, y, x+5, y); !reflect.DeepEqual(got, want) {
		t.Fatalf("after scrolling the highlight is\n%swant it on WORD08 at its new row\n%s", describeCells(rows, got), describeCells(rows, want))
	}
	if got := h.copy(); got != "WORD08" {
		t.Fatalf("after scrolling the copy is %q", got)
	}
	writes := h.writes

	h.sweep("WORD07", "WORD07")
	h.m, _ = runUpdate(t, h.m, tea.WindowSizeMsg{Width: 60, Height: 14})
	resized := h.draw()
	h.m.selActive = false
	plain = h.draw()
	h.m.selActive = true
	if got := h.painted(plain, resized); len(got) != 0 {
		t.Fatalf("after a resize %d cells are still highlighted", len(got))
	}
	if got := h.copy(); got != "" || h.writes != writes {
		t.Fatalf("after a resize right-click copied %q (%d clipboard writes)", got, h.writes-writes)
	}
}

// After a resize the chat clamps its scroll offset while drawing, on a copy
// of the model; the mouse must still land on the rows that were drawn.
func TestCopy_AfterAResizeLandsOnWhatIsDrawn(t *testing.T) {
	h := newChatHarness(t, "linux", 120, 30)
	for i := 0; i < 25; i++ {
		h.m.appendUser(fmt.Sprintf("question %d %s Q%02d", i, strings.Repeat("filler words ", 14), i))
		h.m.appendResponse(fmt.Sprintf("Answer %d: %s MARK%02d end.\n\nSecond %s TAIL%02d", i, strings.Repeat("lorem ipsum dolor ", 9), i, strings.Repeat("sit amet ", 6), i))
	}
	(&h.m).layout()
	h.m.chat.GotoBottom()
	h.draw()
	h.m, _ = runUpdate(t, h.m, tea.WindowSizeMsg{Width: 70, Height: 30})
	tested := 0
	for _, row := range h.rows(h.draw()) {
		for _, f := range strings.Fields(row) {
			if len(f) == 6 && (strings.HasPrefix(f, "MARK") || strings.HasPrefix(f, "TAIL")) {
				h.sweep(f, f)
				if got := h.copy(); got != f {
					t.Fatalf("sweeping %s copied %q", f, got)
				}
				tested++
			}
		}
	}
	if tested < 2 {
		t.Fatalf("only %d tokens on screen", tested)
	}
}

// Images are not text: a selection across one copies the words around it.
func TestCopy_SkipsImages(t *testing.T) {
	h := newChatHarness(t, "linux", 90, 30)
	h.m.appendUser("caption words", chatImage{mime: "image/png", size: 10, imageID: 4, cols: 6, rows: 2})
	h.m.appendUser("", chatImage{mime: "image/png", size: 10})
	h.m.appendResponse("after the images")
	h.sweep("caption", "after")
	if got := h.copy(); got != "caption words\n\nafter" {
		t.Fatalf("copied %q", got)
	}
}

// Inside a tool group — a call and its result, drawn with no blank row
// between — the copy keeps them on adjacent lines.
func TestCopy_AToolGroupIsJoinedByOneLine(t *testing.T) {
	h := newChatHarness(t, "linux", 90, 30)
	h.m.history = []historyEntry{
		{kind: histToolCall, text: "     ▸ bash — CALLX list files"},
		{kind: histToolResult, text: "     ⎿ RESULTY a.go b.go"},
	}
	h.sweep("CALLX", "RESULTY")
	if got := h.copy(); got != "CALLX list files\n⎿ RESULTY" {
		t.Fatalf("copied %q", got)
	}
}

// What the renderer draws in place of markup is paired with that markup; a
// character the renderer hid is skipped over, not borrowed from elsewhere.
func TestTextMap_PairsDrawnCharactersWithTheirSource(t *testing.T) {
	src := "- fix **the** gate\n\n| a | b |\n|---|---|\n| 1 | 2 |"
	tm := buildEntryTextMap([]string{
		"     • fix the gate",
		"",
		"      a  │  b",
		"     ────┼────",
		"      1  │  2",
	}, src, 80)
	pairs := func(line int) string {
		var b strings.Builder
		for _, c := range tm.lines[line] {
			if c.start >= 0 {
				b.WriteString(src[c.start:c.end])
			} else {
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	for line, want := range map[int]string{
		0: "-_fix_the_gate",
		2: "_a__|__b",
		4: "_1__|__2",
	} {
		if got := pairs(line); got != want {
			t.Errorf("line %d pairs %q, want %q", line, got, want)
		}
	}
	if got := pairs(3); !strings.Contains(got, "---|---") {
		t.Errorf("the table rule pairs %q, want its dashes and joint on the source's ---|---", got)
	}
	if c := tm.lines[0][6]; src[c.start:c.end] != "t" || c.start != strings.Index(src, "the") {
		t.Errorf("'t' of the paired with offset %d, want %d", c.start, strings.Index(src, "the"))
	}
}

func TestRowSelection_StreamsLikeATerminal(t *testing.T) {
	start, end := selPos{entry: 1, line: 2, col: 9}, selPos{entry: 2, line: 1, col: 12}
	cases := []struct {
		entry, line, extent int
		from, to            int
		ok                  bool
	}{
		{1, 1, 40, 0, 0, false},
		{1, 2, 40, 9, 40, true},
		{1, 2, 7, 0, 0, false},
		{1, 3, 40, chatLeftMarginCols, 40, true},
		{2, 0, 40, chatLeftMarginCols, 40, true},
		{2, 1, 40, chatLeftMarginCols, 13, true},
		{2, 2, 40, 0, 0, false},
	}
	for _, c := range cases {
		from, to, ok := rowSelection(start, end, c.entry, c.line, c.extent)
		if ok != c.ok || (ok && (from != c.from || to != c.to)) {
			t.Errorf("row %d.%d: %d..%d %v, want %d..%d %v", c.entry, c.line, from, to, ok, c.from, c.to, c.ok)
		}
	}
	sep := selPos{entry: 1, line: 4}
	if _, _, ok := rowSelection(sep, end, 1, 3, 40); ok {
		t.Error("a selection starting on the blank row below an entry covers that entry")
	}
	if from, to, ok := rowSelection(start, sep, 1, 3, 40); !ok || from != chatLeftMarginCols || to != 40 {
		t.Errorf("a selection ending on the blank row below an entry covers its last row %d..%d %v", from, to, ok)
	}
}

func TestSelection_ScrollbarClickDoesNotSelect(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.width = 40
	m.chat.SetWidth(39)
	m.chat.SetHeight(5)
	for i := 0; i < 100; i++ {
		m.appendHistory("line " + strconv.Itoa(i))
	}
	(&m).layout()
	m2, _ := runUpdate(t, m, tea.MouseClickMsg{X: m.width - 1, Y: 2, Button: tea.MouseLeft})
	if !m2.scrollbarDragging || m2.selDragging {
		t.Fatalf("scrollbar click: scrollbarDragging=%v selDragging=%v", m2.scrollbarDragging, m2.selDragging)
	}
}

func TestSelection_RightClickWithoutSelectionIsANoOp(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m2, cmd := runUpdate(t, m, tea.MouseClickMsg{X: 5, Y: 5, Button: tea.MouseRight})
	if cmd != nil || m2.selActive {
		t.Fatal("right-click without a selection did something")
	}
}

// A click that never leaves its cell selects nothing and leaves the
// clipboard alone, on every platform.
func TestSelection_AClickCopiesNothing(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		h := newChatHarness(t, goos, 80, 20)
		h.m.appendResponse("some words here")
		x, y := h.find("words")
		h.drag(x, y, x, y)
		if h.m.selActive || h.m.selDragging || h.copy() != "" || h.writes != 0 {
			t.Fatalf("%s: a click left a selection (%v) or wrote the clipboard (%d)", goos, h.m.selActive, h.writes)
		}
	}
}

// On macOS the terminal keeps right-click and Cmd+C, so releasing a drag
// copies, silently, and clears the highlight; a failure still says so.
func TestSelection_DarwinReleaseCopiesSilently(t *testing.T) {
	h := newChatHarness(t, "darwin", 80, 20)
	h.m.appendResponse("platform **copy** words")
	x, y := h.find("copy")
	h.draw()
	h.m, _ = runUpdate(t, h.m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.draw()
	h.m, _ = runUpdate(t, h.m, tea.MouseMotionMsg{X: x + 3, Y: y, Button: tea.MouseLeft})
	var cmd tea.Cmd
	h.m, cmd = runUpdate(t, h.m, tea.MouseReleaseMsg{X: x + 3, Y: y, Button: tea.MouseLeft})
	if h.m.selDragging || h.m.selActive || cmd == nil {
		t.Fatalf("darwin release: dragging %v active %v cmd %v", h.m.selDragging, h.m.selActive, cmd != nil)
	}
	raw, result := runCopyCmd(t, cmd)
	if result != nil || h.copied != "copy" || raw != clipboardOSC52("copy", false) {
		t.Fatalf("darwin copy: result %v, payload %q, raw %q", result, h.copied, raw)
	}

	withClipboardStubs(t, "darwin", map[string]bool{"pbcopy": true}, func(string, string, ...string) error { return errors.New("clipboard busted") })
	h.draw()
	h.m, _ = runUpdate(t, h.m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.draw()
	h.m, _ = runUpdate(t, h.m, tea.MouseMotionMsg{X: x + 3, Y: y, Button: tea.MouseLeft})
	h.m, cmd = runUpdate(t, h.m, tea.MouseReleaseMsg{X: x + 3, Y: y, Button: tea.MouseLeft})
	if _, result := runCopyCmd(t, cmd); !strings.Contains(result.(toastShowMsg).text, "copy failed") {
		t.Fatalf("failure toast %v", result)
	}
}

// Elsewhere the release keeps the selection on screen, and right-click
// copies it with a toast and clears it.
func TestSelection_LinuxRightClickCopies(t *testing.T) {
	h := newChatHarness(t, "linux", 80, 20)
	h.m.appendResponse("platform copy words")
	h.sweep("copy", "copy")
	if !h.m.selActive || h.writes != 0 {
		t.Fatalf("linux release: active %v, wrote early %d", h.m.selActive, h.writes)
	}
	var cmd tea.Cmd
	h.m, cmd = runUpdate(t, h.m, tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseRight})
	if h.m.selActive || cmd == nil {
		t.Fatal("right-click must copy and clear")
	}
	_, result := runCopyCmd(t, cmd)
	if h.copied != "copy" || !strings.Contains(result.(toastShowMsg).text, "copied") {
		t.Fatalf("right-click copied %q, toast %v", h.copied, result)
	}

	withClipboardStubs(t, "linux", map[string]bool{"wl-copy": true}, func(string, string, ...string) error { return errors.New("offline") })
	h.sweep("copy", "copy")
	_, cmd = h.m.copySelectionAndClear()
	if _, result := runCopyCmd(t, cmd); !strings.Contains(result.(toastShowMsg).text, "copy failed") {
		t.Fatalf("failure toast %v", result)
	}
}

func TestSelection_FingerprintFollowsTheEnds(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	if m.selectionFingerprint() != "" {
		t.Fatal("no selection must have no fingerprint")
	}
	m.selActive = true
	m.selWidth = m.chat.contentWidth()
	m.selAnchor = selPos{entry: 0, line: 1, col: 6}
	m.selFocus = selPos{entry: 1, line: 0, col: 9}
	first := m.selectionFingerprint()
	m.selFocus.col = 10
	if first == "" || m.selectionFingerprint() == first {
		t.Fatal("moving the focus must change the fingerprint")
	}
}

// A re-projection (a view-mode toggle) remakes the entries a selection
// names, so it drops the selection.
func TestSelection_ReprojectionClearsIt(t *testing.T) {
	h := newChatHarness(t, "linux", 80, 20)
	h.m.appendResponse("some words here")
	h.sweep("words", "here")
	if !h.m.selActive {
		t.Fatal("no selection to clear")
	}
	h.m.projectHistory()
	if h.m.selActive || h.m.selDragging {
		t.Fatal("re-projection kept the selection")
	}
}

func TestSelection_CopyOverSSHUsesOSC52(t *testing.T) {
	withClipboardStubs(t, "linux", map[string]bool{}, func(name, stdin string, args ...string) error {
		t.Fatalf("clipboardRun must not fork over SSH; ran %s %v", name, args)
		return nil
	})
	clipboardGetenv = func(key string) string {
		if key == "SSH_TTY" {
			return "/dev/pts/3"
		}
		return ""
	}
	raw, result := runCopyCmd(t, copyTextCmd(NewToastModel(40, time.Second), "remote text"))
	if raw != clipboardOSC52("remote text", false) || !strings.Contains(result.(toastShowMsg).text, "copied") {
		t.Fatalf("ssh copy raw %q toast %v", raw, result)
	}
}
