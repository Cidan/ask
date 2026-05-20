package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
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

// withPasteImageStubs pins clipboardGOOS and saves/restores every
// per-platform paste seam (wl-paste pair + osascript pair) at test
// cleanup. Callers then override only the seams the case exercises;
// the un-overridden ones still point at the saved defaults but won't
// fire because clipboardGOOS gates the switch.
func withPasteImageStubs(t *testing.T, goos string) {
	t.Helper()
	prevGOOS := clipboardGOOS
	prevWlList, prevWlRead := wlPasteListTypesFn, wlPasteReadFn
	prevInfo, prevExtract := darwinClipboardInfoFn, darwinClipboardExtractFn
	t.Cleanup(func() {
		clipboardGOOS = prevGOOS
		wlPasteListTypesFn, wlPasteReadFn = prevWlList, prevWlRead
		darwinClipboardInfoFn, darwinClipboardExtractFn = prevInfo, prevExtract
	})
	clipboardGOOS = goos
}

func TestPasteImageFromClipboard_UnsupportedGOOS(t *testing.T) {
	withPasteImageStubs(t, "plan9")
	_, _, err := pasteImageFromClipboard()
	if err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("expected unsupported-OS error mentioning plan9, got %v", err)
	}
}

func TestPasteImageWayland_PrefersFirstAcceptedMime(t *testing.T) {
	withPasteImageStubs(t, "linux")
	var askedMime string
	payload := []byte("\x89PNG\r\n\x1a\nFAKE")
	wlPasteListTypesFn = func() ([]byte, error) {
		return []byte("text/plain\nimage/png\nimage/jpeg\n"), nil
	}
	wlPasteReadFn = func(mime string) ([]byte, error) {
		askedMime = mime
		return payload, nil
	}
	data, mime, err := pasteImageFromClipboard()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime=%q, want image/png (first accepted in list)", mime)
	}
	if askedMime != "image/png" {
		t.Errorf("wlPasteReadFn called with %q, want image/png", askedMime)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("data mismatch")
	}
}

func TestPasteImageWayland_ListFailurePreservesLegacyErrorString(t *testing.T) {
	withPasteImageStubs(t, "linux")
	wlPasteListTypesFn = func() ([]byte, error) {
		return nil, errors.New("exec: wl-paste not found")
	}
	_, _, err := pasteImageFromClipboard()
	if err == nil {
		t.Fatal("expected error from wl-paste --list-types failure")
	}
	// Users have been seeing this exact phrasing forever — keep it
	// stable so grep / docs / muscle memory still find it.
	want := "wl-paste failed (clipboard empty or wl-paste missing)"
	if err.Error() != want {
		t.Errorf("err=%q, want %q", err.Error(), want)
	}
}

func TestPasteImageWayland_NoImageInClipboard(t *testing.T) {
	withPasteImageStubs(t, "linux")
	wlPasteListTypesFn = func() ([]byte, error) {
		return []byte("text/plain\ntext/html\n"), nil
	}
	wlPasteReadFn = func(mime string) ([]byte, error) {
		t.Fatalf("read should not fire when no accepted mime in list")
		return nil, nil
	}
	_, _, err := pasteImageFromClipboard()
	if err == nil || !strings.Contains(err.Error(), "no image in clipboard") {
		t.Fatalf("expected no-image error, got %v", err)
	}
}

func TestPasteImageWayland_EmptyData(t *testing.T) {
	withPasteImageStubs(t, "linux")
	wlPasteListTypesFn = func() ([]byte, error) {
		return []byte("image/png\n"), nil
	}
	wlPasteReadFn = func(mime string) ([]byte, error) {
		return []byte{}, nil
	}
	_, _, err := pasteImageFromClipboard()
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-image error, got %v", err)
	}
}

func TestPasteImageWayland_ReadFailurePropagates(t *testing.T) {
	withPasteImageStubs(t, "linux")
	wlPasteListTypesFn = func() ([]byte, error) {
		return []byte("image/png\n"), nil
	}
	wlPasteReadFn = func(mime string) ([]byte, error) {
		return nil, errors.New("read failed")
	}
	_, _, err := pasteImageFromClipboard()
	if err == nil || !strings.Contains(err.Error(), "read failed") {
		t.Fatalf("expected read failure to propagate, got %v", err)
	}
}

func TestPasteImageCmd_EmitsImagePastedMsgShape(t *testing.T) {
	withPasteImageStubs(t, "linux")
	var payloadBuf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{A: 255})
	if err := png.Encode(&payloadBuf, img); err != nil {
		t.Fatalf("encode png fixture: %v", err)
	}
	payload := payloadBuf.Bytes()
	wlPasteListTypesFn = func() ([]byte, error) {
		return []byte("image/png\n"), nil
	}
	wlPasteReadFn = func(mime string) ([]byte, error) {
		return payload, nil
	}
	got, ok := pasteImageCmd()().(imagePastedMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want imagePastedMsg", got)
	}
	if got.err != nil {
		t.Fatalf("unexpected err: %v", got.err)
	}
	if got.mime != "image/png" {
		t.Errorf("mime=%q, want image/png", got.mime)
	}
	if !bytes.Equal(got.data, payload) {
		t.Errorf("data mismatch")
	}
	if len(got.pngForKitty) == 0 || got.width != 1 || got.height != 1 {
		t.Errorf("kitty preview shape = bytes:%d %dx%d, want nonempty 1x1", len(got.pngForKitty), got.width, got.height)
	}
}

func TestPasteImageDarwin_DetectsPNGFirst(t *testing.T) {
	withPasteImageStubs(t, "darwin")
	// Real `clipboard info` output advertises many classes for the
	// same image — we want PNG to win regardless of where it lands.
	darwinClipboardInfoFn = func() ([]byte, error) {
		return []byte("«class PNGf», 22272, «class AVIF», 4977, JPEG picture, 4630, TIFF picture, 58646"), nil
	}
	payload := []byte("\x89PNG\r\n\x1a\nfake-bytes")
	var extractedClass, gotDst string
	darwinClipboardExtractFn = func(className, dstPath string) error {
		extractedClass, gotDst = className, dstPath
		return os.WriteFile(dstPath, payload, 0o644)
	}
	data, mime, err := pasteImageFromClipboard()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime=%q, want image/png", mime)
	}
	if extractedClass != "«class PNGf»" {
		t.Errorf("extract class=%q, want «class PNGf»", extractedClass)
	}
	if gotDst == "" {
		t.Errorf("extract called with empty dstPath")
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("data mismatch: got %x want %x", data, payload)
	}
	// Temp file must be cleaned up.
	if _, err := os.Stat(gotDst); !os.IsNotExist(err) {
		t.Errorf("temp file %q still exists after paste; expected cleanup", gotDst)
	}
}

func TestPasteImageDarwin_AcceptsJPEGPictureAlias(t *testing.T) {
	// macOS often advertises JPEG under the human alias rather than
	// «class JPEG». Both must trigger detection; the coercion target
	// is always the four-char-code form.
	withPasteImageStubs(t, "darwin")
	darwinClipboardInfoFn = func() ([]byte, error) {
		return []byte("JPEG picture, 12345, TIFF picture, 8888"), nil
	}
	var extractedClass string
	darwinClipboardExtractFn = func(className, dstPath string) error {
		extractedClass = className
		return os.WriteFile(dstPath, []byte("\xff\xd8\xff\xe0jpeg"), 0o644)
	}
	_, mime, err := pasteImageFromClipboard()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime=%q, want image/jpeg via JPEG-picture alias", mime)
	}
	if extractedClass != "«class JPEG»" {
		t.Errorf("extract class=%q, want «class JPEG»", extractedClass)
	}
}

func TestPasteImageDarwin_AcceptsGIFPictureAlias(t *testing.T) {
	withPasteImageStubs(t, "darwin")
	darwinClipboardInfoFn = func() ([]byte, error) {
		return []byte("GIF picture, 4027"), nil
	}
	var extractedClass string
	darwinClipboardExtractFn = func(className, dstPath string) error {
		extractedClass = className
		return os.WriteFile(dstPath, []byte("GIF89a..."), 0o644)
	}
	_, mime, err := pasteImageFromClipboard()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mime != "image/gif" {
		t.Errorf("mime=%q, want image/gif via GIF-picture alias", mime)
	}
	if extractedClass != "«class GIFf»" {
		t.Errorf("extract class=%q, want «class GIFf»", extractedClass)
	}
}

func TestPasteImageDarwin_NoImageClass(t *testing.T) {
	withPasteImageStubs(t, "darwin")
	darwinClipboardInfoFn = func() ([]byte, error) {
		return []byte("«class utf8», 60, string, 60"), nil
	}
	darwinClipboardExtractFn = func(className, dstPath string) error {
		t.Fatalf("extract should not be called when no image class detected")
		return nil
	}
	_, _, err := pasteImageFromClipboard()
	if err == nil || !strings.Contains(err.Error(), "no image in clipboard") {
		t.Fatalf("expected no-image error, got %v", err)
	}
}

func TestPasteImageDarwin_InfoSubprocessFailure(t *testing.T) {
	withPasteImageStubs(t, "darwin")
	darwinClipboardInfoFn = func() ([]byte, error) {
		return nil, errors.New("exec: osascript not found")
	}
	darwinClipboardExtractFn = func(className, dstPath string) error {
		t.Fatalf("extract should not be called when info call failed")
		return nil
	}
	_, _, err := pasteImageFromClipboard()
	if err == nil || !strings.Contains(err.Error(), "osascript") {
		t.Fatalf("expected osascript error, got %v", err)
	}
}

func TestPasteImageDarwin_CreateTempFailure(t *testing.T) {
	withPasteImageStubs(t, "darwin")
	darwinClipboardInfoFn = func() ([]byte, error) {
		return []byte("«class PNGf», 100"), nil
	}
	darwinClipboardExtractFn = func(className, dstPath string) error {
		t.Fatalf("extract should not be called when temp creation failed")
		return nil
	}
	missingTempDir := filepath.Join(t.TempDir(), "missing")
	t.Setenv("TMPDIR", missingTempDir)
	t.Setenv("TMP", missingTempDir)
	t.Setenv("TEMP", missingTempDir)
	_, _, err := pasteImageFromClipboard()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected temp creation failure, got %v", err)
	}
}

func TestPasteImageDarwin_ExtractFailureCleansTemp(t *testing.T) {
	withPasteImageStubs(t, "darwin")
	darwinClipboardInfoFn = func() ([]byte, error) {
		return []byte("«class PNGf», 100"), nil
	}
	var seenDst string
	darwinClipboardExtractFn = func(className, dstPath string) error {
		seenDst = dstPath
		// The extract command failed — temp file may exist as a
		// zero-byte placeholder (CreateTemp made it), but defer
		// os.Remove must still wipe it.
		return errors.New("applescript: write failed")
	}
	_, _, err := pasteImageFromClipboard()
	if err == nil || !strings.Contains(err.Error(), "osascript extract") {
		t.Fatalf("expected extract error, got %v", err)
	}
	if seenDst == "" {
		t.Fatal("extract was never called")
	}
	if _, statErr := os.Stat(seenDst); !os.IsNotExist(statErr) {
		t.Errorf("temp file %q still exists after extract failure; expected cleanup", seenDst)
	}
}

func TestPasteImageDarwin_ReadFileFailureCleansTemp(t *testing.T) {
	withPasteImageStubs(t, "darwin")
	darwinClipboardInfoFn = func() ([]byte, error) {
		return []byte("«class PNGf», 100"), nil
	}
	var seenDst string
	darwinClipboardExtractFn = func(className, dstPath string) error {
		seenDst = dstPath
		return os.Remove(dstPath)
	}
	_, _, err := pasteImageFromClipboard()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected read-file failure, got %v", err)
	}
	if seenDst == "" {
		t.Fatal("extract was never called")
	}
	if _, statErr := os.Stat(seenDst); !os.IsNotExist(statErr) {
		t.Errorf("temp file %q still exists after read failure; expected cleanup", seenDst)
	}
}

func TestPasteImageDarwin_EmptyData(t *testing.T) {
	withPasteImageStubs(t, "darwin")
	darwinClipboardInfoFn = func() ([]byte, error) {
		return []byte("«class PNGf», 100"), nil
	}
	darwinClipboardExtractFn = func(className, dstPath string) error {
		return os.WriteFile(dstPath, []byte{}, 0o644)
	}
	_, _, err := pasteImageFromClipboard()
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-image error, got %v", err)
	}
}
