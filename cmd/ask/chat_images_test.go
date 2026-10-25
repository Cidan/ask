package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func tinyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 40), G: uint8(y * 40), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stubKitty makes the terminal a Kitty one (or not) and records every image
// upload instead of writing to the controlling terminal.
func stubKitty(t *testing.T, kitty bool) *[]uint32 {
	t.Helper()
	if kitty {
		t.Setenv("TERM", "xterm-kitty")
	} else {
		t.Setenv("TERM", "xterm-256color")
	}
	t.Setenv("KITTY_WINDOW_ID", "")
	var sent []uint32
	prev := kittyTransmitFn
	kittyTransmitFn = func(id uint32, png []byte) error {
		if len(png) == 0 {
			t.Errorf("image %d uploaded with no data", id)
		}
		sent = append(sent, id)
		return nil
	}
	t.Cleanup(func() { kittyTransmitFn = prev })
	return &sent
}

func lastUserEntry(t *testing.T, m model) historyEntry {
	t.Helper()
	for i := len(m.history) - 1; i >= 0; i-- {
		if k := m.history[i].kind; k == histUser || k == histUserQueued {
			return m.history[i]
		}
	}
	t.Fatal("no user entry in history")
	return historyEntry{}
}

func renderedEntry(m *model, kind historyKind, width int) string {
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].kind == kind {
			m.ensureEntryWrapped(i, width)
			return strings.Join(m.history[i].wrapped, "\n")
		}
	}
	return ""
}

// A pasted image stays in the chat once sent: the message keeps its own
// text, and its thumbnail — the Kitty image already uploaded for the
// pending area — is drawn under it instead of an "[image attached]" note.
func TestDispatch_SentImageKeepsItsPreviewInTheChat(t *testing.T) {
	isolateHome(t)
	stubKitty(t, true)
	m := newTestModel(t, newFakeProvider())
	data := tinyPNG(t, 4, 4)
	m.pending = []pendingAttachment{{data: data, mime: "image/png", imageID: 7, thumbCols: 4, thumbRows: 2}}

	next, _ := m.dispatchProviderTurn("what is this?")
	m = next.(model)
	if len(m.pending) != 0 {
		t.Fatal("the attachment stayed pending after send")
	}
	it := m.transcript[len(m.transcript)-1]
	if it.kind != trUser || it.text != "what is this?" {
		t.Fatalf("sent message = %v %q, want the text as typed", it.kind, it.text)
	}
	if len(it.images) != 1 {
		t.Fatalf("message images = %+v", it.images)
	}
	if g := it.images[0]; g.mime != "image/png" || g.size != len(data) || g.imageID != 7 || g.cols != 4 || g.rows != 2 || g.data != nil {
		t.Fatalf("message image = %+v", g)
	}
	if e := lastUserEntry(t, m); len(e.images) != 1 || e.images[0].imageID != 7 {
		t.Fatalf("history entry images = %+v", e.images)
	}

	out := renderedEntry(&m, histUser, 100)
	for _, row := range strings.Split(kittyPlaceholderRows(7, 4, 2), "\n") {
		if !strings.Contains(out, row) {
			t.Fatalf("the chat does not draw image 7's placeholder row %q:\n%s", row, out)
		}
	}
	if strings.Contains(out, "image attached") {
		t.Fatal("the chat still says image attached")
	}
	if !strings.Contains(out, "what is this?") {
		t.Fatal("the message text is missing")
	}
}

// Without Kitty graphics the chat names each image instead.
func TestDispatch_SentImageWithoutKittyIsNamed(t *testing.T) {
	isolateHome(t)
	stubKitty(t, false)
	m := newTestModel(t, newFakeProvider())
	m.pending = []pendingAttachment{{data: make([]byte, 2048), mime: "image/jpeg"}}
	next, _ := m.dispatchProviderTurn("")
	m = next.(model)
	it := m.transcript[len(m.transcript)-1]
	if it.text != "" || len(it.images) != 1 || it.images[0].imageID != 0 {
		t.Fatalf("image-only message = %q %+v", it.text, it.images)
	}
	out := renderedEntry(&m, histUser, 100)
	if !strings.Contains(out, "[image/jpeg  "+humanBytes(2048)+"]") {
		t.Fatalf("the image is not named in the chat:\n%s", out)
	}
}

// A thumbnail too wide for the chat is named instead of drawn, so its
// placeholder cells are never cut.
func TestRenderMessageImages_NarrowChatNamesTheImage(t *testing.T) {
	img := chatImage{mime: "image/png", size: 10, imageID: 3, cols: 20, rows: 4}
	if out := renderMessageImages([]chatImage{img}, 24); strings.Contains(out, kittyPlaceholderRows(3, 20, 4)[:4]) || !strings.Contains(out, "[image/png") {
		t.Fatalf("narrow render:\n%s", out)
	}
	if out := renderMessageImages([]chatImage{img}, 80); !strings.Contains(out, strings.Split(kittyPlaceholderRows(3, 20, 4), "\n")[0]) {
		t.Fatalf("wide render lost the thumbnail:\n%s", out)
	}
}

func userImageEvent(text string, img []byte) *session.Event {
	parts := []*genai.Part{}
	if text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}
	parts = append(parts, genai.NewPartFromBytes(img, "image/png"))
	return &session.Event{Author: "user", Timestamp: time.Now(), LLMResponse: adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleUser, Parts: parts}}}
}

// A resumed conversation shows its images the way the live one did: the
// replay keeps each user message's images — an image-only message included
// — the load builds their thumbnails off the UI goroutine, and the tab
// uploads each under a fresh id.
func TestResume_ShowsTheConversationsImages(t *testing.T) {
	isolateHome(t)
	sent := stubKitty(t, true)
	img := tinyPNG(t, 30, 15)
	items, err := loadTranscriptFromEvents([]*session.Event{
		userImageEvent("compare these", img),
		userImageEvent("", img),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].text != "compare these" || items[1].kind != trUser || items[1].text != "" {
		t.Fatalf("replayed items = %+v", items)
	}
	for _, it := range items {
		if len(it.images) != 1 || it.images[0].mime != "image/png" || it.images[0].size != len(img) || it.images[0].data == nil {
			t.Fatalf("replayed images = %+v", it.images)
		}
	}

	prepareTranscriptImages(items)
	wantCols, wantRows := thumbnailGrid(30, 15)
	for _, it := range items {
		g := it.images[0]
		if g.data != nil || g.thumb == nil || g.cols != wantCols || g.rows != wantRows {
			t.Fatalf("prepared image = cols %d rows %d data %v thumb %v", g.cols, g.rows, g.data != nil, g.thumb != nil)
		}
	}

	m := newTestModel(t, newFakeProvider())
	m.sessionID = "s1"
	m, _ = runUpdate(t, m, historyLoadedMsg{tabID: m.id, sessionID: "s1", silent: true, transcript: items})
	if len(*sent) != 2 || (*sent)[0] == (*sent)[1] {
		t.Fatalf("uploaded ids %v, want one fresh id per image", *sent)
	}
	for i, it := range m.transcript {
		g := it.images[0]
		if g.imageID != (*sent)[i] || g.thumb != nil {
			t.Fatalf("image %d placed as id %d (thumb kept %v), want %d", i, g.imageID, g.thumb != nil, (*sent)[i])
		}
	}
	out := renderedEntry(&m, histUser, 100)
	if !strings.Contains(out, strings.Split(kittyPlaceholderRows((*sent)[1], wantCols, wantRows), "\n")[0]) {
		t.Fatalf("the resumed image is not drawn:\n%s", out)
	}
}

// Replayed without Kitty, images keep their name and size but no bytes.
func TestResume_WithoutKittyDropsTheBytes(t *testing.T) {
	sent := stubKitty(t, false)
	items, _ := loadTranscriptFromEvents([]*session.Event{userImageEvent("x", tinyPNG(t, 2, 2))}, nil)
	prepareTranscriptImages(items)
	g := items[0].images[0]
	if g.data != nil || g.thumb != nil || g.imageID != 0 || g.mime != "image/png" {
		t.Fatalf("image = %+v", g)
	}
	m := newTestModel(t, newFakeProvider())
	m.transcript = items
	m.placeTranscriptImages()
	if len(*sent) != 0 {
		t.Fatal("uploaded an image to a terminal that cannot draw it")
	}
}

// A message queued while a turn runs keeps its images when it is sent.
func TestQueuedMessage_KeepsItsImagesWhenDrained(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.appendUserQueued("later", chatImage{mime: "image/png", size: 5, imageID: 9, cols: 2, rows: 1})
	m, _ = runUpdate(t, m, queuedMessageDrainedMsg{proc: m.proc})
	it := m.transcript[len(m.transcript)-1]
	if it.kind != trUser || len(it.images) != 1 || it.images[0].imageID != 9 {
		t.Fatalf("drained message = %v %+v", it.kind, it.images)
	}
	if e := lastUserEntry(t, m); e.kind != histUser || len(e.images) != 1 {
		t.Fatalf("drained entry = %v %+v", e.kind, e.images)
	}
}
