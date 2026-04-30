package main

import (
	"strings"
	"testing"
	"time"

	uvansi "github.com/charmbracelet/x/ansi"
)

func TestToastRender_NoActiveAlertReturnsInputUnchanged(t *testing.T) {
	tm := NewToastModel(40, time.Second)
	in := "first line\nsecond line\nthird line"
	if got := tm.Render(in); got != in {
		t.Errorf("inactive toast must passthrough; got %q", got)
	}
}

func TestToast_ShowActivatesAndUpdateConsumes(t *testing.T) {
	tm := NewToastModel(40, time.Second)
	cmd := tm.show("hi there")
	if cmd == nil {
		t.Fatal("show should return a cmd")
	}
	msg := cmd()
	tm2, _ := tm.Update(msg)
	if !tm2.hasActive() {
		t.Errorf("toast should be active after consuming show msg")
	}
	if tm2.text != "hi there" {
		t.Errorf("toast text=%q want hi there", tm2.text)
	}
}

func TestToastRender_ActiveOverlaysOnTopRight(t *testing.T) {
	// Toast renders as a bordered chip (3 rows: top border, body,
	// bottom border). Feed it more rows than that so we can assert
	// the lines BELOW the chip are completely untouched.
	tm := NewToastModel(20, time.Second)
	tm.active = true
	tm.text = "ok"
	tm.expires = time.Now().Add(time.Second)
	const rows = 6
	row := strings.Repeat("x", 60)
	in := strings.Repeat(row+"\n", rows-1) + row
	out := tm.Render(in)
	lines := strings.Split(out, "\n")
	if len(lines) != rows {
		t.Fatalf("line count changed by Render: got %d, want %d", len(lines), rows)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("output should contain toast text, got %q", out)
	}
	// The chip is 3 rows tall — rows below row 2 (0-indexed) should
	// be the original line, byte-for-byte.
	for i := 3; i < rows; i++ {
		if lines[i] != row {
			t.Errorf("line %d below the chip was modified: got %q want %q", i, lines[i], row)
		}
	}
	// And the top three lines should each have been modified by the
	// overlay (chip text appended at right).
	for i := 0; i < 3; i++ {
		if lines[i] == row {
			t.Errorf("line %d should carry the toast overlay, but matches the input untouched", i)
		}
	}
}

func TestToast_AutoDismissAfterDuration(t *testing.T) {
	tm := NewToastModel(40, 50*time.Millisecond)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	tm.clock = func() time.Time { return now }
	cmd := tm.show("bye")
	tm2, _ := tm.Update(cmd())
	if !tm2.hasActive() {
		t.Fatal("expected toast active right after show")
	}
	// First tick: still within duration.
	tm3, tickCmd := tm2.Update(toastTickMsg{})
	if !tm3.hasActive() {
		t.Errorf("toast should still be active before duration elapses")
	}
	if tickCmd == nil {
		t.Errorf("active toast should keep ticking")
	}
	// Advance the clock past expiry.
	now = now.Add(time.Second)
	tm3.clock = func() time.Time { return now }
	tm4, doneCmd := tm3.Update(toastTickMsg{})
	if tm4.hasActive() {
		t.Errorf("toast should auto-dismiss once expired")
	}
	if doneCmd != nil {
		t.Errorf("expired toast should stop scheduling ticks")
	}
}

func TestToast_TickWithoutActiveIsHarmless(t *testing.T) {
	tm := NewToastModel(40, time.Second)
	tm2, cmd := tm.Update(toastTickMsg{})
	if tm2.hasActive() {
		t.Errorf("tick should not activate a dormant toast")
	}
	if cmd != nil {
		t.Errorf("tick on dormant toast should not re-arm")
	}
}

func TestToastRender_LongMessageWrapsAcrossLines(t *testing.T) {
	// A wide error message should render across multiple body rows
	// instead of being single-line-truncated. We feed a long message
	// at maxWidth=80 (= innerMax 76) and assert the chip grew taller
	// than 3 rows (top border + 1 body row + bottom border = the
	// truncated baseline).
	tm := NewToastModel(80, time.Second)
	tm.active = true
	tm.text = "memory: create database \"ask_tests\": connectivity: " +
		"could not reach bolt://localhost:7687 within 10s — verify the " +
		"server is running and the credentials are correct"
	tm.expires = time.Now().Add(time.Second)

	chip := tm.renderChip()
	rows := strings.Count(chip, "\n") + 1
	if rows < 4 {
		t.Errorf("expected wrapped chip to span >=4 rows, got %d:\n%s", rows, chip)
	}
	// Every body row must fit within the inner width (80 - 4 = 76)
	// so the chip's bordered box stays at most 80 cells wide.
	for _, line := range strings.Split(chip, "\n") {
		if w := uvansi.StringWidth(line); w > 80 {
			t.Errorf("chip row exceeded maxWidth: width=%d line=%q", w, line)
		}
	}
}

func TestToastRender_BodyHeightCapsWithEllipsis(t *testing.T) {
	// Force a body that wraps past maxHeight rows. The visible last
	// line should end with "…" so the reader sees there is more.
	tm := NewToastModel(20, time.Second)
	tm.maxHeight = 3
	tm.active = true
	// 200 'x' chars at innerMax=16 wraps to 13 rows; we cap to 3.
	tm.text = strings.Repeat("x", 200)
	tm.expires = time.Now().Add(time.Second)

	chip := tm.renderChip()
	bodyLines := strings.Split(chip, "\n")
	// rendered chip = top border + body + bottom border. With
	// maxHeight=3 we expect 3 body rows + 2 borders = 5 rows total.
	if got := len(bodyLines); got != 5 {
		t.Fatalf("expected 5 chip rows (border+body+border), got %d:\n%s", got, chip)
	}
	if !strings.Contains(chip, "…") {
		t.Errorf("body height cap should leave an ellipsis marker, got:\n%s", chip)
	}
}

func TestToastRender_ShortMessageStaysSingleLine(t *testing.T) {
	// A message that fits inside innerMax must not gain any wrapping
	// rows; the bordered chip should remain 3 rows tall (top + body +
	// bottom).
	tm := NewToastModel(40, time.Second)
	tm.active = true
	tm.text = "memory off"
	tm.expires = time.Now().Add(time.Second)

	chip := tm.renderChip()
	if got := strings.Count(chip, "\n") + 1; got != 3 {
		t.Errorf("short message should keep 3-row chip, got %d:\n%s", got, chip)
	}
}

func TestToast_ApplyThemeRebuildsStyle(t *testing.T) {
	tm := NewToastModel(40, time.Second)
	pre := tm.style
	tm.applyTheme(activeTheme)
	if tm.style.GetBold() != true {
		t.Errorf("themed toast should be bold")
	}
	_ = pre // ensure compile when theme is no-op
}
