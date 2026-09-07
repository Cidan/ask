package tools

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

func TestRenderDesignTool_RendersAndQueuesImage(t *testing.T) {
	sink := NewImageSink()
	tool := RenderDesignTool(sink)

	resp, err := RunToolWithJSON(testAgentCtx(), tool,
		`{"html":"<div class=box>hello</div>","css":".box{background:#3366ff;color:#fff;padding:20px}","width":300,"height":120,"scale":2,"description":"test"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected error result: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "×") {
		t.Errorf("result should report dimensions, got %q", resp.Content)
	}

	queued := sink.Drain()
	if len(queued) != 1 {
		t.Fatalf("want 1 queued image, got %d", len(queued))
	}
	if queued[0].MIME != "image/png" {
		t.Errorf("mime = %q, want image/png", queued[0].MIME)
	}
	img, err := png.Decode(bytes.NewReader(queued[0].Data))
	if err != nil {
		t.Fatalf("queued bytes are not a valid PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 600 || b.Dy() != 240 {
		t.Errorf("decoded image is %dx%d, want 600x240", b.Dx(), b.Dy())
	}
}

func TestRenderDesignTool_RequiresHTML(t *testing.T) {
	resp, err := RunToolWithJSON(testAgentCtx(), RenderDesignTool(NewImageSink()),
		`{"html":"   ","description":"test"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !resp.IsError {
		t.Errorf("empty html should be an error result, got %q", resp.Content)
	}
}
