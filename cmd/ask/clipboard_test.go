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

	tea "charm.land/bubbletea/v2"
)

// withClipboardStubs swaps the package-level clipboard seams for the
// duration of the test so we never spawn a real subprocess and never
// read the test host's TMUX / SSH environment. Callers pass the GOOS to
// simulate, the set of binaries that should "exist" on PATH, and a
// recorder that captures every write. The environment reads as empty;
// tests that need tmux or SSH replace clipboardGetenv after calling
// this helper.
func withClipboardStubs(t *testing.T, goos string, present map[string]bool, run func(name, stdin string, args ...string) error) {
	t.Helper()
	prevGOOS, prevLook, prevRun, prevGetenv := clipboardGOOS, clipboardLookPath, clipboardRun, clipboardGetenv
	t.Cleanup(func() {
		clipboardGOOS, clipboardLookPath, clipboardRun, clipboardGetenv = prevGOOS, prevLook, prevRun, prevGetenv
	})
	clipboardGOOS = goos
	clipboardLookPath = func(name string) (string, error) {
		if present[name] {
			return "/fake/" + name, nil
		}
		return "", errors.New("not found")
	}
	clipboardRun = run
	clipboardGetenv = func(string) string { return "" }
}

// clipboardRunRecord is one clipboardRun invocation as seen by the
// recorder that recordClipboardRuns installs.
type clipboardRunRecord struct {
	name  string
	stdin string
	args  string
}

// recordClipboardRuns returns a clipboardRun stub that appends every
// call to the returned slice (args joined by single spaces so tests can
// compare against the literal command line).
func recordClipboardRuns() (*[]clipboardRunRecord, func(name, stdin string, args ...string) error) {
	var runs []clipboardRunRecord
	return &runs, func(name, stdin string, args ...string) error {
		runs = append(runs, clipboardRunRecord{name: name, stdin: stdin, args: strings.Join(args, " ")})
		return nil
	}
}

func TestClipboardCopyText_DarwinUsesPbcopyOnce(t *testing.T) {
	// macOS has a single pasteboard: exactly one pbcopy run, no
	// PRIMARY-selection follow-up.
	runs, rec := recordClipboardRuns()
	withClipboardStubs(t, "darwin", map[string]bool{"pbcopy": true}, rec)
	if err := clipboardCopyText("hello mac"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []clipboardRunRecord{{name: "pbcopy", stdin: "hello mac"}}
	if len(*runs) != 1 || (*runs)[0] != want[0] {
		t.Errorf("runs=%+v, want %+v", *runs, want)
	}
}

func TestClipboardCopyText_LinuxWlCopyWritesClipboardThenPrimary(t *testing.T) {
	// wl-copy is the highest-priority Linux writer. Shift+Insert and
	// middle-click paste the PRIMARY selection (kitty, foot, xterm, …),
	// and the terminal never fills it while ask owns the mouse, so the
	// clipboard write must be followed by a --primary write of the same
	// payload.
	runs, rec := recordClipboardRuns()
	withClipboardStubs(t, "linux", map[string]bool{"wl-copy": true, "xclip": true, "xsel": true}, rec)
	if err := clipboardCopyText("hi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []clipboardRunRecord{
		{name: "wl-copy", stdin: "hi", args: ""},
		{name: "wl-copy", stdin: "hi", args: "--primary"},
	}
	if len(*runs) != len(want) {
		t.Fatalf("runs=%+v, want %+v", *runs, want)
	}
	for i := range want {
		if (*runs)[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, (*runs)[i], want[i])
		}
	}
}

func TestClipboardCopyText_LinuxFallsBackToXclipForBothSelections(t *testing.T) {
	runs, rec := recordClipboardRuns()
	withClipboardStubs(t, "linux", map[string]bool{"xclip": true}, rec)
	if err := clipboardCopyText("hi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []clipboardRunRecord{
		{name: "xclip", stdin: "hi", args: "-selection clipboard"},
		{name: "xclip", stdin: "hi", args: "-selection primary"},
	}
	if len(*runs) != len(want) {
		t.Fatalf("runs=%+v, want %+v", *runs, want)
	}
	for i := range want {
		if (*runs)[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, (*runs)[i], want[i])
		}
	}
}

func TestClipboardCopyText_LinuxFallsBackToXselForBothSelections(t *testing.T) {
	runs, rec := recordClipboardRuns()
	withClipboardStubs(t, "linux", map[string]bool{"xsel": true}, rec)
	if err := clipboardCopyText("hi"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []clipboardRunRecord{
		{name: "xsel", stdin: "hi", args: "--clipboard --input"},
		{name: "xsel", stdin: "hi", args: "--primary --input"},
	}
	if len(*runs) != len(want) {
		t.Fatalf("runs=%+v, want %+v", *runs, want)
	}
	for i := range want {
		if (*runs)[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, (*runs)[i], want[i])
		}
	}
}

func TestClipboardCopyText_PrimaryWriteErrorPropagates(t *testing.T) {
	var calls int
	withClipboardStubs(t, "linux",
		map[string]bool{"wl-copy": true},
		func(name, stdin string, args ...string) error {
			calls++
			if strings.Join(args, " ") == "--primary" {
				return errors.New("primary busted")
			}
			return nil
		})
	err := clipboardCopyText("hi")
	if err == nil || !strings.Contains(err.Error(), "wl-copy") || !strings.Contains(err.Error(), "primary busted") {
		t.Fatalf("expected the primary write error to propagate with the binary name, got %v", err)
	}
	if calls != 2 {
		t.Errorf("clipboardRun called %d times, want 2 (clipboard then primary)", calls)
	}
}

func TestClipboardCopyText_SSHSkipsRemoteBinaries(t *testing.T) {
	// Over SSH the binaries on PATH belong to the remote host; the
	// user's clipboard is on the terminal at the far end and only OSC
	// 52 reaches it. The binary path must stay untouched (no PATH
	// lookup, no fork) and report success so the toast doesn't claim a
	// failure for a copy that worked.
	for _, env := range []string{"SSH_TTY", "SSH_CONNECTION", "SSH_CLIENT"} {
		t.Run(env, func(t *testing.T) {
			withClipboardStubs(t, "linux",
				map[string]bool{"wl-copy": true},
				func(name, stdin string, args ...string) error {
					t.Fatalf("clipboardRun must not fork over SSH; ran %s %v", name, args)
					return nil
				})
			clipboardLookPath = func(name string) (string, error) {
				t.Fatalf("clipboardLookPath must not be consulted over SSH; asked for %s", name)
				return "", nil
			}
			clipboardGetenv = func(key string) string {
				if key == env {
					return "set"
				}
				return ""
			}
			if err := clipboardCopyText("remote"); err != nil {
				t.Fatalf("SSH copy must succeed via OSC 52 alone, got %v", err)
			}
		})
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

// osc52Plain is the byte shape the terminal must receive for a plain
// (non-tmux) copy: one OSC 52 for the CLIPBOARD selection (`c`) and one
// for PRIMARY (`p`), each carrying the same base64 payload. Two
// sequences rather than the multi-target "cp" form because not every
// emulator parses more than one target character.
func osc52Plain(s string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(s))
	return "\x1b]52;c;" + b64 + "\x07" + "\x1b]52;p;" + b64 + "\x07"
}

func TestClipboardOSC52_PlainSetsClipboardAndPrimary(t *testing.T) {
	got := clipboardOSC52("hello", false)
	if want := osc52Plain("hello"); got != want {
		t.Errorf("clipboardOSC52(plain):\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "\x1bPtmux;") {
		t.Errorf("plain sequence must not carry a tmux envelope: %q", got)
	}
}

func TestClipboardOSC52_TmuxSendsPlainThenPassthrough(t *testing.T) {
	// tmux drops an application's OSC 52 unless `set-clipboard` is
	// `on`, and drops a DCS passthrough unless `allow-passthrough` is
	// on. Sending the plain sequences first and the same bytes inside
	// the envelope second covers whichever the user enabled; ESCs
	// inside the envelope are doubled so tmux unwraps to the original.
	got := clipboardOSC52("hi", true)
	plain := osc52Plain("hi")
	want := plain + "\x1bPtmux;" + strings.ReplaceAll(plain, "\x1b", "\x1b\x1b") + "\x1b\\"
	if got != want {
		t.Errorf("clipboardOSC52(tmux):\n got %q\nwant %q", got, want)
	}
}

func TestClipboardOSC52Cmd_YieldsRawWrite(t *testing.T) {
	withClipboardStubs(t, "linux", map[string]bool{}, nil)
	msg, ok := clipboardOSC52Cmd("payload")().(tea.RawMsg)
	if !ok {
		t.Fatalf("clipboardOSC52Cmd must yield a tea.RawMsg so the bytes leave through the program output; got %T", msg)
	}
	if got := msg.Msg; got != osc52Plain("payload") {
		t.Errorf("raw write = %q, want %q", got, osc52Plain("payload"))
	}
}

func TestClipboardOSC52Cmd_WrapsForTmuxFromEnv(t *testing.T) {
	withClipboardStubs(t, "linux", map[string]bool{}, nil)
	clipboardGetenv = func(key string) string {
		if key == "TMUX" {
			return "/tmp/tmux-1000/default,4242,0"
		}
		return ""
	}
	msg, ok := clipboardOSC52Cmd("payload")().(tea.RawMsg)
	if !ok {
		t.Fatalf("expected tea.RawMsg, got %T", msg)
	}
	if got := msg.Msg; got != clipboardOSC52("payload", true) {
		t.Errorf("raw write under TMUX = %q, want plain+passthrough %q", got, clipboardOSC52("payload", true))
	}
}

func TestClipboardCopyText_NoBinaryStillReportsSoUserCanInstallOne(t *testing.T) {
	// Locally (not SSH) a missing binary is worth telling the user
	// about even though the OSC 52 raw write went out in parallel: it
	// is the one hint that wl-clipboard / xclip is not installed.
	withClipboardStubs(t, "linux",
		map[string]bool{},
		func(name, stdin string, args ...string) error {
			t.Fatalf("clipboardRun must not be called when no binary present")
			return nil
		})
	err := clipboardCopyText("payload")
	if err == nil || !strings.Contains(err.Error(), "wl-copy") {
		t.Fatalf("expected the no-binary error listing the writers tried, got %v", err)
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
