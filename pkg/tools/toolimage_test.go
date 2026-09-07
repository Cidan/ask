package tools

import (
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func countImageParts(c *genai.Content) int {
	n := 0
	for _, p := range c.Parts {
		if p != nil && p.InlineData != nil && len(p.InlineData.Data) > 0 {
			n++
		}
	}
	return n
}

func TestImageInjectionHook_InjectsWhenVisionOn(t *testing.T) {
	sink := NewImageSink()
	sink.Add(PendingImage{Data: []byte{1, 2, 3}, MIME: "image/png", Caption: "first"})
	sink.Add(PendingImage{Data: []byte{4, 5, 6}, MIME: "image/png"})

	hook := NewImageInjectionHook(sink, func() bool { return true })
	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}

	if err := hook.ProcessRequest(testAgentCtx(), req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if len(req.Contents) != 3 {
		t.Fatalf("want 3 contents after injecting 2 images, got %d", len(req.Contents))
	}
	if got := countImageParts(req.Contents[1]); got != 1 {
		t.Errorf("first injected content: want 1 image part, got %d", got)
	}
	if req.Contents[1].Role != genai.RoleUser {
		t.Errorf("injected content role = %q, want user", req.Contents[1].Role)
	}
	// The first image carried a caption; the second did not.
	if len(req.Contents[1].Parts) != 2 {
		t.Errorf("captioned image should have caption + image parts, got %d", len(req.Contents[1].Parts))
	}
	if len(req.Contents[2].Parts) != 1 {
		t.Errorf("uncaptioned image should have only the image part, got %d", len(req.Contents[2].Parts))
	}
	// Sink is drained: a second pass injects nothing.
	if err := hook.ProcessRequest(testAgentCtx(), req); err != nil {
		t.Fatalf("second ProcessRequest: %v", err)
	}
	if len(req.Contents) != 3 {
		t.Errorf("sink should be drained; contents grew to %d", len(req.Contents))
	}
}

func TestImageInjectionHook_SkipsWithoutVision(t *testing.T) {
	sink := NewImageSink()
	sink.Add(PendingImage{Data: []byte{1, 2, 3}, MIME: "image/png"})

	hook := NewImageInjectionHook(sink, func() bool { return false })
	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}

	if err := hook.ProcessRequest(testAgentCtx(), req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if len(req.Contents) != 1 {
		t.Errorf("no vision: nothing should be injected, got %d contents", len(req.Contents))
	}
	if got := sink.Drain(); got != nil {
		t.Errorf("no-vision path should still drain the sink, got %d pending", len(got))
	}
}

func TestImageInjectionHook_NilSupportsImagesTreatedCapable(t *testing.T) {
	sink := NewImageSink()
	sink.Add(PendingImage{Data: []byte{9}, MIME: "image/png"})
	hook := NewImageInjectionHook(sink, nil)
	req := &model.LLMRequest{}
	if err := hook.ProcessRequest(testAgentCtx(), req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if len(req.Contents) != 1 {
		t.Errorf("nil supportsImages should inject, got %d contents", len(req.Contents))
	}
}

func TestImageInjectionHook_EmptySinkNoop(t *testing.T) {
	hook := NewImageInjectionHook(NewImageSink(), func() bool { return true })
	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
	if err := hook.ProcessRequest(testAgentCtx(), req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if len(req.Contents) != 1 {
		t.Errorf("empty sink should be a no-op, got %d contents", len(req.Contents))
	}
}

func TestImageSink_IgnoresEmpty(t *testing.T) {
	sink := NewImageSink()
	sink.Add(PendingImage{Data: nil, MIME: "image/png"})
	if got := sink.Drain(); got != nil {
		t.Errorf("empty image should not be queued, got %d", len(got))
	}
}
