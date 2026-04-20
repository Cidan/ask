package main

import (
	"errors"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type imagePastedMsg struct {
	data []byte
	mime string
	err  error
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
		return imagePastedMsg{data: data, mime: mime}
	}
}
