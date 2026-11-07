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

func TestToast_ShowArmsOneExpiryThatDismisses(t *testing.T) {
	tm := NewToastModel(40, time.Millisecond)
	tm, expire := tm.Update(tm.show("bye")())
	if !tm.hasActive() {
		t.Fatal("expected toast active right after show")
	}
	msgs := drainBatch(t, expire)
	if len(msgs) != 1 {
		t.Fatalf("show must arm exactly one timer, got %d msgs: %#v", len(msgs), msgs)
	}
	if _, ok := msgs[0].(toastExpireMsg); !ok {
		t.Fatalf("timer msg = %T, want toastExpireMsg", msgs[0])
	}
	tm, cmd := tm.Update(msgs[0])
	if tm.hasActive() || tm.text != "" {
		t.Errorf("expiry should dismiss the toast: active=%v text=%q", tm.hasActive(), tm.text)
	}
	if cmd != nil {
		t.Error("a dismissed toast must not schedule anything")
	}
}

func TestToast_EarlierShowsExpiryDoesNotDismissNewerShow(t *testing.T) {
	tm := NewToastModel(40, time.Millisecond)
	tm, first := tm.Update(tm.show("first")())
	tm, second := tm.Update(tm.show("second")())
	staleMsgs := drainBatch(t, first)
	if len(staleMsgs) != 1 {
		t.Fatalf("first show armed %d timers, want 1", len(staleMsgs))
	}
	tm, cmd := tm.Update(staleMsgs[0])
	if !tm.hasActive() || tm.text != "second" {
		t.Fatalf("stale expiry dismissed the newer toast: active=%v text=%q", tm.hasActive(), tm.text)
	}
	if cmd != nil {
		t.Error("a stale expiry must not re-arm")
	}
	liveMsgs := drainBatch(t, second)
	if len(liveMsgs) != 1 {
		t.Fatalf("second show armed %d timers, want 1", len(liveMsgs))
	}
	tm, _ = tm.Update(liveMsgs[0])
	if tm.hasActive() {
		t.Error("the latest show's expiry should dismiss the toast")
	}
}

func TestToast_IgnoresAnotherToastsMessages(t *testing.T) {
	tm := NewToastModel(40, time.Millisecond)
	other := NewToastModel(40, time.Millisecond)
	if tm.id == other.id {
		t.Fatalf("toasts share id %d", tm.id)
	}
	tm, cmd := tm.Update(other.show("not mine")())
	if tm.hasActive() || cmd != nil {
		t.Fatalf("foreign show activated the toast: active=%v cmd=%v", tm.hasActive(), cmd != nil)
	}
	tm, _ = tm.Update(tm.show("mine")())
	tm, cmd = tm.Update(toastExpireMsg{id: other.id, tag: tm.tag})
	if !tm.hasActive() || cmd != nil {
		t.Fatalf("foreign expiry touched the toast: active=%v cmd=%v", tm.hasActive(), cmd != nil)
	}
}

func TestApp_ToastRoutesOnlyToOwningTab(t *testing.T) {
	a := newSidebarTestApp(t, 3)
	for _, tab := range a.tabs {
		tab.toast = NewToastModel(80, time.Millisecond)
	}
	owner := a.tabs[1]

	m1, cmd := a.Update(owner.toast.show("copied to clipboard")())
	a = asApp(t, m1)
	for i, tab := range a.tabs {
		if got, want := tab.toast.hasActive(), tab.id == owner.id; got != want {
			t.Errorf("tab %d toast active = %v, want %v", i, got, want)
		}
	}
	timers := drainBatch(t, cmd)
	if len(timers) != 1 {
		t.Fatalf("one show armed %d timers across the app, want 1: %#v", len(timers), timers)
	}

	m2, cmd := a.Update(timers[0])
	a = asApp(t, m2)
	if owner.toast.hasActive() {
		t.Error("expiry routed through the app should dismiss the owner's toast")
	}
	if cmd != nil {
		if msgs := drainBatch(t, cmd); len(msgs) != 0 {
			t.Errorf("expiry scheduled follow-up msgs: %#v", msgs)
		}
	}
}

func TestApp_ToastFromClosedTabIsDropped(t *testing.T) {
	a := newSidebarTestApp(t, 2)
	for _, tab := range a.tabs {
		tab.toast = NewToastModel(80, time.Millisecond)
	}
	gone := NewToastModel(80, time.Millisecond)

	m1, cmd := a.Update(gone.show("late")())
	a = asApp(t, m1)
	for i, tab := range a.tabs {
		if tab.toast.hasActive() {
			t.Errorf("tab %d showed a toast it does not own", i)
		}
	}
	if cmd != nil {
		t.Error("a toast with no owning tab must not arm a timer")
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
