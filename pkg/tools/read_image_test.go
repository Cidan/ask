package tools

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 120, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestReadImage_QueuesForVisionModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pic.png")
	writeTestPNG(t, p, 64, 40)

	env := NewToolEnv(dir, 1, true, nil, nil)
	env.ImageSink = NewImageSink()
	env.SupportsImages = func() bool { return true }

	res, err := runTypedTool[ReadResult](t, ReadTool(env), ReadParams{FilePath: "pic.png", Description: "look"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(res.Content, "64×40") {
		t.Errorf("result should report dimensions, got %q", res.Content)
	}
	queued := env.ImageSink.Drain()
	if len(queued) != 1 || queued[0].MIME != "image/png" {
		t.Fatalf("expected 1 png queued, got %d", len(queued))
	}
	if _, err := png.Decode(bytes.NewReader(queued[0].Data)); err != nil {
		t.Errorf("queued bytes are not a valid PNG: %v", err)
	}
}

func TestReadImage_NoVisionDoesNotQueue(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pic.png")
	writeTestPNG(t, p, 20, 20)

	env := NewToolEnv(dir, 1, true, nil, nil)
	env.ImageSink = NewImageSink()
	env.SupportsImages = func() bool { return false }

	res, err := runTypedTool[ReadResult](t, ReadTool(env), ReadParams{FilePath: "pic.png", Description: "look"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(res.Content, "no vision") {
		t.Errorf("no-vision result should say so, got %q", res.Content)
	}
	if got := env.ImageSink.Drain(); got != nil {
		t.Errorf("no image should be queued without vision, got %d", len(got))
	}
}

func TestReadImage_NoSinkStillDescribes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pic.png")
	writeTestPNG(t, p, 12, 8)

	env := NewToolEnv(dir, 1, true, nil, nil) // no ImageSink
	res, err := runTypedTool[ReadResult](t, ReadTool(env), ReadParams{FilePath: "pic.png", Description: "look"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(res.Content, "12×8") || !strings.Contains(res.Content, "unavailable") {
		t.Errorf("no-sink result should describe the image and note unavailability, got %q", res.Content)
	}
}
