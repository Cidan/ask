package main

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// withClipboardStubs swaps the package-level clipboard seams for the
// duration of the test so we never spawn a real subprocess or touch
// /dev/tty. Callers pass the GOOS to simulate, the set of binaries
// that should "exist" on PATH, and a recorder that captures every
// successful write. The OSC 52 emit is stubbed to a no-op; tests that
// need to assert OSC 52 emission can replace clipboardEmitOSC52Fn
// directly after calling this helper.
func withClipboardStubs(t *testing.T, goos string, present map[string]bool, run func(name, stdin string, args ...string) error) {
	t.Helper()
	prevGOOS, prevLook, prevRun, prevEmit := clipboardGOOS, clipboardLookPath, clipboardRun, clipboardEmitOSC52Fn
	t.Cleanup(func() {
		clipboardGOOS, clipboardLookPath, clipboardRun, clipboardEmitOSC52Fn = prevGOOS, prevLook, prevRun, prevEmit
	})
	clipboardGOOS = goos
	clipboardLookPath = func(name string) (string, error) {
		if present[name] {
			return "/fake/" + name, nil
		}
		return "", errors.New("not found")
	}
	clipboardRun = run
	clipboardEmitOSC52Fn = func(string) error { return nil }
}

func TestClipboardCopyText_DarwinUsesPbcopy(t *testing.T) {
	var ranName, ranStdin string
	var ranArgs []string
	withClipboardStubs(t, "darwin",
		map[string]bool{"pbcopy": true},
		func(name, stdin string, args ...string) error {
			ranName, ranStdin, ranArgs = name, stdin, args
			return nil
		})
	if err := clipboardCopyText("hello mac"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ranName != "pbcopy" {
		t.Errorf("ran %q, want pbcopy", ranName)
	}
	if ranStdin != "hello mac" {
		t.Errorf("stdin %q, want hello mac", ranStdin)
	}
	if len(ranArgs) != 0 {
		t.Errorf("pbcopy got args %v, want none", ranArgs)
	}
}

func TestClipboardCopyText_LinuxPrefersWlCopy(t *testing.T) {
	var ranName string
	withClipboardStubs(t, "linux",
		map[string]bool{"wl-copy": true, "xclip": true, "xsel": true},
		func(name, stdin string, args ...string) error {
			ranName = name
			return nil
		})
	if err := clipboardCopyText("hi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ranName != "wl-copy" {
		t.Errorf("ran %q, want wl-copy (highest priority on linux)", ranName)
	}
}

func TestClipboardCopyText_LinuxFallsBackToXclip(t *testing.T) {
	var ranName string
	var ranArgs []string
	withClipboardStubs(t, "linux",
		map[string]bool{"xclip": true},
		func(name, stdin string, args ...string) error {
			ranName, ranArgs = name, args
			return nil
		})
	if err := clipboardCopyText("hi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ranName != "xclip" {
		t.Errorf("ran %q, want xclip fallback", ranName)
	}
	if got := strings.Join(ranArgs, " "); got != "-selection clipboard" {
		t.Errorf("xclip args=%q, want -selection clipboard", got)
	}
}

func TestClipboardCopyText_NoBinaryAvailable(t *testing.T) {
	withClipboardStubs(t, "linux",
		map[string]bool{},
		func(name, stdin string, args ...string) error {
			t.Fatalf("clipboardRun should not be called when no binary present")
			return nil
		})
	err := clipboardCopyText("hi")
	if err == nil {
		t.Fatal("expected error when no clipboard binary is available")
	}
	if !strings.Contains(err.Error(), "wl-copy") {
		t.Errorf("error %q should list the writers tried", err)
	}
}

func TestClipboardCopyText_UnsupportedGOOS(t *testing.T) {
	withClipboardStubs(t, "plan9",
		map[string]bool{},
		func(name, stdin string, args ...string) error {
			t.Fatalf("clipboardRun should not be called on unsupported OS")
			return nil
		})
	err := clipboardCopyText("hi")
	if err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("expected unsupported-OS error mentioning plan9, got %v", err)
	}
}

func TestClipboardCopyText_PropagatesRunError(t *testing.T) {
	withClipboardStubs(t, "darwin",
		map[string]bool{"pbcopy": true},
		func(name, stdin string, args ...string) error {
			return errors.New("boom")
		})
	err := clipboardCopyText("hi")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected pbcopy run error to propagate, got %v", err)
	}
}

func TestOsc52Sequence_PlainSystemClipboard(t *testing.T) {
	got := osc52Sequence("hello", false)
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hello")) + "\x07"
	if got != want {
		t.Errorf("osc52Sequence(plain):\n got %q\nwant %q", got, want)
	}
}

func TestOsc52Sequence_TmuxPassthroughDoublesEscape(t *testing.T) {
	// Inside tmux the inner OSC must be wrapped in DCS passthrough and
	// every embedded ESC inside the inner sequence must be doubled so
	// the outer terminal sees the original OSC after tmux strips one
	// layer.
	got := osc52Sequence("hi", true)
	inner := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("hi")) + "\x07"
	want := "\x1bPtmux;\x1b" + strings.ReplaceAll(inner, "\x1b", "\x1b\x1b") + "\x1b\\"
	if got != want {
		t.Errorf("osc52Sequence(tmux):\n got %q\nwant %q", got, want)
	}
	// Sanity: the wrapper must start with DCS (ESC P) and end with ST
	// (ESC backslash) — anything else would not be a valid passthrough.
	if !strings.HasPrefix(got, "\x1bPtmux;") {
		t.Errorf("tmux wrap missing DCS introducer: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b\\") {
		t.Errorf("tmux wrap missing ST terminator: %q", got)
	}
}

func TestClipboardCopyText_EmitsOSC52AlongsideBinary(t *testing.T) {
	// macOS / tmux is the original symptom: pbcopy "succeeds" but never
	// reaches the system pasteboard. The OSC 52 emit is the parallel
	// path that does. Both must fire for every clipboardCopyText call.
	var emitText string
	var emitCount int
	var ranName, ranStdin string
	withClipboardStubs(t, "darwin",
		map[string]bool{"pbcopy": true},
		func(name, stdin string, args ...string) error {
			ranName, ranStdin = name, stdin
			return nil
		})
	clipboardEmitOSC52Fn = func(s string) error {
		emitCount++
		emitText = s
		return nil
	}
	if err := clipboardCopyText("payload"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if emitCount != 1 {
		t.Errorf("OSC 52 emit fired %d times, want 1", emitCount)
	}
	if emitText != "payload" {
		t.Errorf("OSC 52 got %q, want payload", emitText)
	}
	if ranName != "pbcopy" || ranStdin != "payload" {
		t.Errorf("binary writer should still run: name=%q stdin=%q", ranName, ranStdin)
	}
}

func TestClipboardCopyText_OSC52FailureDoesNotBlockBinary(t *testing.T) {
	// /dev/tty not openable in some sandboxes; that path errors but the
	// binary writer is still authoritative for the success toast.
	var ranName string
	withClipboardStubs(t, "linux",
		map[string]bool{"wl-copy": true},
		func(name, stdin string, args ...string) error {
			ranName = name
			return nil
		})
	clipboardEmitOSC52Fn = func(string) error { return errors.New("no tty") }
	if err := clipboardCopyText("x"); err != nil {
		t.Fatalf("OSC 52 emit failure must not surface as a copy error: %v", err)
	}
	if ranName != "wl-copy" {
		t.Errorf("binary writer should still run after OSC 52 failure; ran=%q", ranName)
	}
}

func TestClipboardCopyText_OSC52FiresEvenWhenBinaryMissing(t *testing.T) {
	// On a linux box with no clipboard binaries on PATH the binary
	// path fails — but OSC 52 may still have hit the terminal. We
	// surface the binary error so the user sees the diagnostic, but
	// the OSC 52 emit must have run regardless so that supported
	// terminals still populate the clipboard.
	var emitted bool
	withClipboardStubs(t, "linux",
		map[string]bool{},
		func(name, stdin string, args ...string) error {
			t.Fatalf("clipboardRun must not be called when no binary present")
			return nil
		})
	clipboardEmitOSC52Fn = func(string) error {
		emitted = true
		return nil
	}
	if err := clipboardCopyText("payload"); err == nil {
		t.Fatal("expected the no-binary error to still propagate")
	}
	if !emitted {
		t.Errorf("OSC 52 must fire before the binary lookup loop, regardless of binary availability")
	}
}
