package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// clipboardLookPath / clipboardRun / clipboardEmitOSC52Fn are package-level
// seams so tests can stub the binary-selection, the subprocess write, and the
// OSC 52 terminal emission without touching real subprocesses or /dev/tty.
var (
	clipboardLookPath = exec.LookPath
	clipboardRun      = func(name string, stdin string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdin = strings.NewReader(stdin)
		return cmd.Run()
	}
	clipboardGOOS        = runtime.GOOS
	clipboardEmitOSC52Fn = clipboardEmitOSC52
)

// clipboardWriter pairs a binary name with the args it needs. Picked at
// runtime by clipboardCopyText based on GOOS and PATH availability.
type clipboardWriter struct {
	name string
	args []string
}

// clipboardWritersFor returns the writer candidates to try, in order, for
// the given GOOS. macOS gets pbcopy; Linux tries the Wayland writer first
// then the X11 fallbacks; everything else is empty (caller surfaces the
// no-binary error).
func clipboardWritersFor(goos string) []clipboardWriter {
	switch goos {
	case "darwin":
		return []clipboardWriter{{name: "pbcopy"}}
	case "linux":
		return []clipboardWriter{
			{name: "wl-copy"},
			{name: "xclip", args: []string{"-selection", "clipboard"}},
			{name: "xsel", args: []string{"--clipboard", "--input"}},
		}
	default:
		return nil
	}
}

// clipboardCopyText writes s to the OS clipboard via two paths in
// parallel: an OSC 52 escape sent directly to /dev/tty (the terminal
// emulator handles the actual write, which survives tmux and SSH where
// child-process writers talk to the wrong session's clipboard), and a
// platform-native binary (pbcopy on macOS; wl-copy / xclip / xsel on
// Linux in that order). The OSC 52 emit is best-effort — a successful
// /dev/tty write only confirms the terminal received the sequence —
// so the binary write is still the authoritative success signal that
// the toast reflects. Returns a descriptive error when no compatible
// binary is on PATH.
func clipboardCopyText(s string) error {
	_ = clipboardEmitOSC52Fn(s)
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
		return nil
	}
	return fmt.Errorf("no clipboard binary available (tried %s)", strings.Join(tried, ", "))
}

// osc52Sequence returns the OSC 52 system-clipboard set sequence for
// s. When inTmux is true the inner OSC is wrapped in a tmux DCS
// passthrough envelope (ESC P tmux ; <inner with each ESC doubled> ESC
// \\) so the outer terminal — not tmux itself — receives the OSC. Pure
// function: no env reads, no I/O, kept separate so tests can pin both
// shapes deterministically.
func osc52Sequence(s string, inTmux bool) string {
	inner := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\x07"
	if !inTmux {
		return inner
	}
	return "\x1bPtmux;\x1b" + strings.ReplaceAll(inner, "\x1b", "\x1b\x1b") + "\x1b\\"
}

// clipboardEmitOSC52 writes the OSC 52 sequence to /dev/tty so the
// terminal emulator performs the clipboard write directly. This is
// what makes copy work in tmux on macOS (where pbcopy "succeeds" but
// talks to the tmux session, not the system pasteboard) and over SSH
// (where the remote pbcopy/wl-copy is the wrong host). Best-effort:
// a successful write here only confirms the terminal received the
// bytes — the terminal still has to honour OSC 52, which iTerm2 /
// WezTerm / Ghostty / kitty / Alacritty / modern Terminal.app do.
// Going through /dev/tty (not stdout) mirrors kitty.go's graphics
// transmit so we don't race the Bubble Tea renderer's frame writes.
func clipboardEmitOSC52(s string) error {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer tty.Close()
	_, err = tty.WriteString(osc52Sequence(s, os.Getenv("TMUX") != ""))
	return err
}

type imagePastedMsg struct {
	data       []byte
	mime       string
	pngForKitty []byte
	width      int
	height     int
	err        error
}

var acceptedImageMimes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

func pasteImageCmd() tea.Cmd {
	return func() tea.Msg {
		listOut, err := exec.Command("wl-paste", "--list-types").Output()
		if err != nil {
			return imagePastedMsg{err: errors.New("wl-paste failed (clipboard empty or wl-paste missing)")}
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
			return imagePastedMsg{err: errors.New("no image in clipboard")}
		}
		data, err := exec.Command("wl-paste", "--type", mime, "--no-newline").Output()
		if err != nil {
			return imagePastedMsg{err: err}
		}
		if len(data) == 0 {
			return imagePastedMsg{err: errors.New("clipboard image was empty")}
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
