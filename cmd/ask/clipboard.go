package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

// clipboardLookPath / clipboardRun / clipboardGetenv are package-level
// seams so tests can stub the binary-selection, the subprocess write, and
// the TMUX / SSH environment probes without touching real subprocesses or
// the test host's environment.
var (
	clipboardLookPath = exec.LookPath
	clipboardRun      = func(name string, stdin string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdin = strings.NewReader(stdin)
		return cmd.Run()
	}
	clipboardGOOS   = runtime.GOOS
	clipboardGetenv = os.Getenv
)

// clipboardWriter pairs a binary name with the args that target the
// CLIPBOARD selection and, where the platform has one, the args that
// target the PRIMARY selection. Picked at runtime by clipboardCopyText
// based on GOOS and PATH availability.
type clipboardWriter struct {
	name        string
	args        []string
	primaryArgs []string
}

// clipboardWritersFor returns the writer candidates to try, in order, for
// the given GOOS. macOS gets pbcopy; Linux tries the Wayland writer first
// then the X11 fallbacks, each writing PRIMARY as well as CLIPBOARD
// (Shift+Insert and middle-click paste PRIMARY in kitty, foot, xterm, …
// and the terminal never fills it itself while ask owns the mouse);
// everything else is empty (caller surfaces the no-binary error).
func clipboardWritersFor(goos string) []clipboardWriter {
	switch goos {
	case "darwin":
		return []clipboardWriter{{name: "pbcopy"}}
	case "linux":
		return []clipboardWriter{
			{name: "wl-copy", primaryArgs: []string{"--primary"}},
			{name: "xclip", args: []string{"-selection", "clipboard"}, primaryArgs: []string{"-selection", "primary"}},
			{name: "xsel", args: []string{"--clipboard", "--input"}, primaryArgs: []string{"--primary", "--input"}},
		}
	default:
		return nil
	}
}

// clipboardOverSSH reports whether ask runs inside an SSH session, in
// which case the platform binaries live on the wrong host: the user's
// clipboard belongs to the terminal on the far end, and only OSC 52
// reaches it.
func clipboardOverSSH() bool {
	return clipboardGetenv("SSH_TTY") != "" ||
		clipboardGetenv("SSH_CONNECTION") != "" ||
		clipboardGetenv("SSH_CLIENT") != ""
}

// clipboardCopyText writes s to the OS clipboard through a platform
// binary (pbcopy on macOS; wl-copy / xclip / xsel on Linux in that
// order, each writing CLIPBOARD then PRIMARY). It runs alongside the
// OSC 52 path from clipboardOSC52Cmd: a terminal write is fire-and-
// forget, so the binary write is the authoritative success signal the
// toast reflects. Over SSH there is nothing to run — the binaries would
// talk to the remote host's display — and OSC 52 is the copy, so this
// returns nil without touching PATH. Returns a descriptive error when
// no compatible binary is on PATH.
func clipboardCopyText(s string) error {
	if clipboardOverSSH() {
		return nil
	}
	writers := clipboardWritersFor(clipboardGOOS)
	if len(writers) == 0 {
		return fmt.Errorf("clipboard not supported on %s", clipboardGOOS)
	}
	var tried []string
	for _, w := range writers {
		if _, err := clipboardLookPath(w.name); err != nil {
			tried = append(tried, w.name)
			continue
		}
		if err := clipboardRun(w.name, s, w.args...); err != nil {
			return fmt.Errorf("%s: %w", w.name, err)
		}
		if w.primaryArgs != nil {
			if err := clipboardRun(w.name, s, w.primaryArgs...); err != nil {
				return fmt.Errorf("%s: %w", w.name, err)
			}
		}
		return nil
	}
	return fmt.Errorf("no clipboard binary available (tried %s)", strings.Join(tried, ", "))
}

// clipboardOSC52 returns the terminal escape bytes that set both the
// CLIPBOARD and the PRIMARY selection to s: one OSC 52 per target,
// because the multi-target form ("cp") is not honoured by every
// emulator. Inside tmux the plain sequences go out first — tmux
// forwards them to the outer terminal itself when `set-clipboard` is
// `on` — followed by the same bytes in a DCS passthrough envelope,
// which tmux hands through untouched when `allow-passthrough` is on.
// Either tmux setting is enough; tmux drops whichever form it is not
// configured for. Pure function: no env reads, no I/O.
func clipboardOSC52(s string, inTmux bool) string {
	plain := xansi.SetSystemClipboard(s) + xansi.SetPrimaryClipboard(s)
	if !inTmux {
		return plain
	}
	return plain + xansi.TmuxPassthrough(plain)
}

// clipboardOSC52Cmd hands the OSC 52 bytes to Bubble Tea as a raw write
// so they leave through the program's own output — flushed in order with
// the frames, never interleaved with one — and reach the terminal the
// user is looking at even over SSH, where no local binary can. The
// terminal still has to honour OSC 52 (kitty, Ghostty, WezTerm,
// Alacritty, foot, iTerm2 and modern Terminal.app do).
func clipboardOSC52Cmd(s string) tea.Cmd {
	return tea.Raw(clipboardOSC52(s, clipboardGetenv("TMUX") != ""))
}

type imagePastedMsg struct {
	data        []byte
	mime        string
	pngForKitty []byte
	width       int
	height      int
	err         error
}

var acceptedImageMimes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// Image-paste subprocess seams — tests stub these instead of forking
// wl-paste / osascript. The Linux pair maps to wl-paste's two-step
// type listing + typed read; the Darwin pair maps to osascript reading
// `clipboard info` and coercing the clipboard to a four-char-code class
// that writes the raw bytes to a temp file.
var (
	wlPasteListTypesFn = func() ([]byte, error) {
		return exec.Command("wl-paste", "--list-types").Output()
	}
	wlPasteReadFn = func(mime string) ([]byte, error) {
		return exec.Command("wl-paste", "--type", mime, "--no-newline").Output()
	}
	darwinClipboardInfoFn = func() ([]byte, error) {
		return exec.Command("osascript", "-e", "clipboard info").Output()
	}
	darwinClipboardExtractFn = func(className, dstPath string) error {
		script := fmt.Sprintf(
			"set img to (the clipboard as %s)\n"+
				"set fd to open for access POSIX file %q with write permission\n"+
				"set eof of fd to 0\n"+
				"write img to fd\n"+
				"close access fd",
			className, dstPath)
		return exec.Command("osascript", "-e", script).Run()
	}
)

// darwinImageClasses lists the MIME types we accept from the macOS
// pasteboard, paired with the AppleScript four-char-code coercion
// target and the marker substrings `clipboard info` uses to advertise
// the type. Order is preference order: PNG first because macOS
// auto-converts screenshots to PNG and the model accepts it without a
// re-encode. JPEG / GIF appear under either the «class XXXX» literal
// or the human alias ("JPEG picture", "GIF picture") — we accept both.
var darwinImageClasses = []struct {
	className string
	mime      string
	infoTags  []string
}{
	{"«class PNGf»", "image/png", []string{"«class PNGf»"}},
	{"«class JPEG»", "image/jpeg", []string{"«class JPEG»", "JPEG picture"}},
	{"«class GIFf»", "image/gif", []string{"«class GIFf»", "GIF picture"}},
}

func pasteImageCmd() tea.Cmd {
	return func() tea.Msg {
		data, mime, err := pasteImageFromClipboard()
		if err != nil {
			return imagePastedMsg{err: err}
		}
		msg := imagePastedMsg{data: data, mime: mime}
		if png, w, h, derr := encodeToPNG(data); derr == nil {
			msg.pngForKitty = png
			msg.width = w
			msg.height = h
		}
		return msg
	}
}

// pasteImageFromClipboard dispatches by clipboardGOOS to the per-platform
// reader. Linux uses wl-paste (no X11 fallback — see cmd/ask/CLAUDE.md); macOS
// uses osascript to coerce the system pasteboard to a known image class.
func pasteImageFromClipboard() ([]byte, string, error) {
	switch clipboardGOOS {
	case "linux":
		return pasteImageWayland()
	case "darwin":
		return pasteImageDarwin()
	default:
		return nil, "", fmt.Errorf("image paste not supported on %s", clipboardGOOS)
	}
}

func pasteImageWayland() ([]byte, string, error) {
	listOut, err := wlPasteListTypesFn()
	if err != nil {
		return nil, "", errors.New("wl-paste failed (clipboard empty or wl-paste missing)")
	}
	var mime string
	for _, t := range strings.Split(string(listOut), "\n") {
		t = strings.TrimSpace(t)
		if acceptedImageMimes[t] {
			mime = t
			break
		}
	}
	if mime == "" {
		return nil, "", errors.New("no image in clipboard")
	}
	data, err := wlPasteReadFn(mime)
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", errors.New("clipboard image was empty")
	}
	return data, mime, nil
}

// pasteImageDarwin coerces the macOS pasteboard to a known image class
// via osascript. `the clipboard as «class XXXX»` returns the raw bytes
// for that representation; AppleScript writes them to a temp file and
// we read them back. Verified against a /System PNG: output is
// byte-identical to the source (no AppleScript wrapper header).
func pasteImageDarwin() ([]byte, string, error) {
	info, err := darwinClipboardInfoFn()
	if err != nil {
		return nil, "", fmt.Errorf("osascript failed: %w", err)
	}
	infoStr := string(info)
	var className, mime string
	for _, c := range darwinImageClasses {
		for _, tag := range c.infoTags {
			if strings.Contains(infoStr, tag) {
				className, mime = c.className, c.mime
				break
			}
		}
		if className != "" {
			break
		}
	}
	if className == "" {
		return nil, "", errors.New("no image in clipboard")
	}
	tmp, err := os.CreateTemp("", "askclip-*.bin")
	if err != nil {
		return nil, "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, "", err
	}
	defer os.Remove(tmpPath)
	if err := darwinClipboardExtractFn(className, tmpPath); err != nil {
		return nil, "", fmt.Errorf("osascript extract: %w", err)
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", errors.New("clipboard image was empty")
	}
	return data, mime, nil
}
