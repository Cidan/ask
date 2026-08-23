package engine

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestIsTransientError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"openrouter provider wrapper 400", errors.New(`stealth/ox-alpha: POST "https://openrouter.ai/api/v1/chat/completions": 400 Bad Request {"message":"Provider returned error","code":400,"metadata":{"raw":"ERROR","provider_name":"Stealth","is_byok":false}}`), true},
		{"bare malformed 400", errors.New("model: 400 Bad Request invalid tool schema"), false},
		{"rate limit 429", errors.New("model: 429 Too Many Requests"), true},
		{"service unavailable 503", errors.New("model: 503 Service Unavailable"), true},
		{"overloaded 529", errors.New("model: 529 overloaded_error"), true},
		{"vertex resource exhausted", errors.New("googleapi: Error 429: Resource exhausted, rate limit"), true},
		{"unauthorized 401", errors.New("model: 401 Unauthorized invalid api key"), false},
		{"not found 404", errors.New("model: 404 model not found"), false},
		{"context length", errors.New("model: 400 maximum context length exceeded"), false},
		{"network reset", errors.New("read tcp: connection reset by peer"), true},
		{"context canceled", context.Canceled, false},
		{"unknown", errors.New("something weird happened"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTransientError(c.err); got != c.want {
				t.Fatalf("isTransientError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// scriptedModel yields the scripted result for each successive call.
type scriptedModel struct {
	calls   int
	results []func(yield func(*model.LLMResponse, error) bool)
}

func (m *scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	idx := m.calls
	m.calls++
	return func(yield func(*model.LLMResponse, error) bool) {
		if idx >= len(m.results) {
			yield(okResponse("done"), nil)
			return
		}
		m.results[idx](yield)
	}
}

func okResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}}}
}

func errYield(err error) func(func(*model.LLMResponse, error) bool) {
	return func(yield func(*model.LLMResponse, error) bool) { yield(nil, err) }
}

func drain(seq iter.Seq2[*model.LLMResponse, error]) (texts []string, gotErr error) {
	for resp, err := range seq {
		if err != nil {
			gotErr = err
			break
		}
		if resp != nil && resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
	}
	return
}

func TestRetryingModel_TransientThenSuccess(t *testing.T) {
	transient := errors.New("model: 503 Service Unavailable")
	inner := &scriptedModel{results: []func(func(*model.LLMResponse, error) bool){
		errYield(transient),
		errYield(transient),
		func(yield func(*model.LLMResponse, error) bool) { yield(okResponse("recovered"), nil) },
	}}
	r := newRetryingModel(inner, time.Millisecond, 1.0)

	texts, err := drain(r.GenerateContent(context.Background(), &model.LLMRequest{}, true))
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}
	if len(texts) != 1 || texts[0] != "recovered" {
		t.Fatalf("expected [recovered], got %v", texts)
	}
	if inner.calls != 3 {
		t.Fatalf("expected 3 inner calls (2 fail + 1 ok), got %d", inner.calls)
	}
}

func TestRetryingModel_NonTransientSurfacesImmediately(t *testing.T) {
	fatal := errors.New("model: 401 Unauthorized invalid api key")
	inner := &scriptedModel{results: []func(func(*model.LLMResponse, error) bool){errYield(fatal)}}
	r := newRetryingModel(inner, time.Millisecond, 1.0)

	_, err := drain(r.GenerateContent(context.Background(), &model.LLMRequest{}, true))
	if err == nil {
		t.Fatal("expected the fatal error to surface")
	}
	if inner.calls != 1 {
		t.Fatalf("expected 1 inner call (no retry), got %d", inner.calls)
	}
}

func TestRetryingModel_MidStreamErrorNotRetried(t *testing.T) {
	transient := errors.New("model: 503 Service Unavailable")
	inner := &scriptedModel{results: []func(func(*model.LLMResponse, error) bool){
		func(yield func(*model.LLMResponse, error) bool) {
			if !yield(okResponse("partial"), nil) {
				return
			}
			yield(nil, transient)
		},
	}}
	r := newRetryingModel(inner, time.Millisecond, 1.0)

	texts, err := drain(r.GenerateContent(context.Background(), &model.LLMRequest{}, true))
	if err == nil {
		t.Fatal("expected mid-stream error to surface (content already emitted)")
	}
	if len(texts) != 1 || texts[0] != "partial" {
		t.Fatalf("expected the partial chunk to pass through, got %v", texts)
	}
	if inner.calls != 1 {
		t.Fatalf("expected 1 inner call (no retry after emit), got %d", inner.calls)
	}
}

func TestRetryingModel_ContextCancelBreaksRetry(t *testing.T) {
	transient := errors.New("model: 503 Service Unavailable")
	// Always transient — without cancellation this would loop forever.
	inner := &scriptedModel{results: nil}
	inner.results = make([]func(func(*model.LLMResponse, error) bool), 1000)
	for i := range inner.results {
		inner.results[i] = errYield(transient)
	}
	r := newRetryingModel(inner, 10*time.Millisecond, 2.0)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(25 * time.Millisecond); cancel() }()

	_, err := drain(r.GenerateContent(ctx, &model.LLMRequest{}, true))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled to break the retry loop, got %v", err)
	}
}

func TestRetryBackoff_CapsAtMax(t *testing.T) {
	if d := retryBackoff(2*time.Second, 2.0, 0); d != 2*time.Second {
		t.Fatalf("attempt 0 = %v, want 2s", d)
	}
	if d := retryBackoff(2*time.Second, 2.0, 1); d != 4*time.Second {
		t.Fatalf("attempt 1 = %v, want 4s", d)
	}
	if d := retryBackoff(2*time.Second, 2.0, 20); d != retryMaxDelay {
		t.Fatalf("attempt 20 = %v, want cap %v", d, retryMaxDelay)
	}
}

// closableModel records whether Close was called, verifying the io.Closer
// capability is forwarded through the retry decorator and CloseModel.
type closableModel struct {
	scriptedModel
	closed bool
}

func (m *closableModel) Close() error { m.closed = true; return nil }

func TestCloseModel_ForwardsThroughRetryDecorator(t *testing.T) {
	inner := &closableModel{}
	wrapped := newRetryingModel(inner, time.Millisecond, 2)
	if err := CloseModel(wrapped); err != nil {
		t.Fatalf("CloseModel: %v", err)
	}
	if !inner.closed {
		t.Error("Close must reach the wrapped model through retryingModel")
	}
}

func TestCloseModel_NoOpForPlainModel(t *testing.T) {
	// A model with no Close must not panic and must return nil.
	if err := CloseModel(&scriptedModel{}); err != nil {
		t.Errorf("CloseModel on a non-closer = %v, want nil", err)
	}
}
