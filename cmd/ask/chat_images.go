package main

import (
	"fmt"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// chatImage is an image a user message carried: its type and size, and —
// on a terminal that draws images (Kitty graphics) — the image id it was
// transmitted under and its thumbnail's size in cells. A message replayed
// from disk arrives with data, which prepareTranscriptImages turns into
// thumb; placeImages transmits thumb and sets imageID. Both are dropped once
// used.
type chatImage struct {
	mime       string
	size       int
	imageID    uint32
	cols, rows int

	data  []byte
	thumb []byte
}

// kittyTransmitFn is the Kitty image upload; a seam so tests never write to
// the controlling terminal.
var kittyTransmitFn = kittyTransmitPNG

// sentImages is what the images pending on the input become once sent: the
// thumbnails already uploaded for the pending area are drawn again in the
// chat, under the message.
func (m *model) sentImages() []chatImage {
	if len(m.pending) == 0 {
		return nil
	}
	out := make([]chatImage, 0, len(m.pending))
	for _, p := range m.pending {
		out = append(out, chatImage{mime: p.mime, size: len(p.data), imageID: p.imageID, cols: p.thumbCols, rows: p.thumbRows})
	}
	return out
}

// prepareTranscriptImages builds the thumbnail of every replayed image, off
// the UI goroutine, when the terminal can draw them. Raw image data is
// dropped either way: the session file keeps it, and the chat only ever
// needs the thumbnail.
func prepareTranscriptImages(items []transcriptItem) {
	kitty := isKitty()
	for i := range items {
		for j := range items[i].images {
			img := &items[i].images[j]
			if img.data == nil {
				continue
			}
			if kitty && img.imageID == 0 {
				if thumb, w, h, err := encodeToPNG(img.data); err == nil {
					img.thumb = thumb
					img.cols, img.rows = thumbnailGrid(w, h)
				}
			}
			img.data = nil
		}
	}
}

// placeTranscriptImages uploads every prepared thumbnail in the transcript to
// the terminal under a fresh image id, so a resumed conversation shows its
// images the way the live one did.
func (m *model) placeTranscriptImages() {
	for i := range m.transcript {
		for j := range m.transcript[i].images {
			img := &m.transcript[i].images[j]
			if img.thumb == nil {
				continue
			}
			id := m.nextKittyImageID()
			if err := kittyTransmitFn(id, img.thumb); err != nil {
				debugLog("kitty transmit err: %v", err)
				img.cols, img.rows = 0, 0
			} else {
				img.imageID = id
			}
			img.thumb = nil
		}
	}
}

// nextKittyImageID hands out image ids, never zero.
func (m *model) nextKittyImageID() uint32 {
	m.nextImageID++
	if m.nextImageID == 0 {
		m.nextImageID = 1
	}
	return m.nextImageID
}

// renderUserEntry is a user message as the chat draws it: the bar holding
// its text, then its images — thumbnails where the terminal draws them,
// else a chip naming each one.
func (m model) renderUserEntry(text string, images []chatImage, width int, queued bool) string {
	var parts []string
	if text != "" || len(images) == 0 {
		parts = append(parts, m.renderUserBarAt(text, width, queued))
	}
	if len(images) > 0 {
		parts = append(parts, renderMessageImages(images, width))
	}
	return strings.Join(parts, "\n")
}

// renderMessageImages lays a message's images out in one row at the chat's
// content column. A thumbnail that would not fit the width is named in a chip
// instead, since cutting its placeholder cells would break the image.
func renderMessageImages(images []chatImage, width int) string {
	pieces := make([]string, 0, len(images))
	widths := make([]int, 0, len(images))
	heights := make([]int, 0, len(images))
	for _, img := range images {
		if img.imageID != 0 && img.cols > 0 && img.rows > 0 && chatLeftMarginCols+img.cols+2 <= width {
			piece, w, h := renderBorderedThumb(img.imageID, img.cols, img.rows)
			pieces, widths, heights = append(pieces, piece), append(widths, w), append(heights, h)
			continue
		}
		piece := chipStyle.UnsetMarginLeft().Render(fmt.Sprintf("[%s  %s]", img.mime, humanBytes(int64(img.size))))
		pieces, widths, heights = append(pieces, piece), append(widths, lipgloss.Width(piece)), append(heights, 1)
	}
	row := joinPiecesH(pieces, widths, heights, 2)
	margin := strings.Repeat(" ", chatLeftMarginCols)
	lines := strings.Split(row, "\n")
	for i := range lines {
		lines[i] = margin + lines[i]
	}
	return strings.Join(lines, "\n")
}
