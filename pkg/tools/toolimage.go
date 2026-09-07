package tools

import (
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// PendingImage is an image produced by a tool during a turn, waiting to be
// handed to the model before the next model call.
type PendingImage struct {
	Data    []byte
	MIME    string
	Caption string
}

// ImageSink collects images produced by tools so a request processor can feed
// them to the model as inline image parts. A tool result is text/JSON on every
// provider (ADK puts it in FunctionResponse.Response and OpenRouter in a string
// tool message), so bytes returned from a tool never reach the model as an
// image. The only cross-provider path is an InlineData part in a user content,
// which is exactly what pasted images use. The sink is the shared handoff
// between the tool that renders an image and the hook that injects it.
type ImageSink struct {
	mu      sync.Mutex
	pending []PendingImage
}

// NewImageSink creates an empty per-session image sink.
func NewImageSink() *ImageSink { return &ImageSink{} }

// Add queues an image for injection. A nil sink or empty image is ignored.
func (s *ImageSink) Add(img PendingImage) {
	if s == nil || len(img.Data) == 0 {
		return
	}
	s.mu.Lock()
	s.pending = append(s.pending, img)
	s.mu.Unlock()
}

// Drain returns and clears the queued images.
func (s *ImageSink) Drain() []PendingImage {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	out := s.pending
	s.pending = nil
	return out
}

// ImageInjectionHook is a request processor, not a callable tool: before each
// model call it drains the session's ImageSink and appends any pending images
// as user content parts so the model can see them. It registers no function
// declaration, so it stays invisible to the model — the same technique
// MemoryRecallHook uses to run per request without becoming a tool the model
// can call. supportsImages gates injection: when the model has no vision the
// images are dropped (the producing tool's text result stands in for them).
type ImageInjectionHook struct {
	sink           *ImageSink
	supportsImages func() bool
}

// NewImageInjectionHook builds the injection hook over a shared sink.
func NewImageInjectionHook(sink *ImageSink, supportsImages func() bool) *ImageInjectionHook {
	return &ImageInjectionHook{sink: sink, supportsImages: supportsImages}
}

func (h *ImageInjectionHook) Name() string        { return "inject_tool_images" }
func (h *ImageInjectionHook) Description() string { return "Feeds tool-rendered images to the model." }
func (h *ImageInjectionHook) IsLongRunning() bool { return false }

// ProcessRequest implements ADK's request processor.
func (h *ImageInjectionHook) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if h == nil || h.sink == nil || req == nil {
		return nil
	}
	imgs := h.sink.Drain()
	if len(imgs) == 0 {
		return nil
	}
	if h.supportsImages != nil && !h.supportsImages() {
		return nil
	}
	for _, img := range imgs {
		if len(img.Data) == 0 {
			continue
		}
		parts := make([]*genai.Part, 0, 2)
		if img.Caption != "" {
			parts = append(parts, genai.NewPartFromText(img.Caption))
		}
		parts = append(parts, genai.NewPartFromBytes(img.Data, img.MIME))
		req.Contents = append(req.Contents, &genai.Content{Role: genai.RoleUser, Parts: parts})
	}
	return nil
}
