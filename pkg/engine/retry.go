package engine

import (
	"context"
	"errors"
	"iter"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
)

// retryMaxDelay caps the exponential backoff between transient retries.
const retryMaxDelay = 30 * time.Second

// retryingModel wraps a model.LLM and retries the underlying call on
// transient provider failures — rate limits, 5xx, overload, network
// errors, and OpenRouter's "Provider returned error" wrapper. Transient
// retries are UNBOUNDED: the call is retried until it succeeds, the
// context is cancelled, or a non-transient error surfaces. This is the
// stopgap until workflow runs become resumable ("continuables") — an
// overloaded provider should stall the turn, not kill it.
//
// A retry only happens when the failure arrives before any content was
// streamed (emitted == false), so nothing is ever yielded twice.
type retryingModel struct {
	inner        model.LLM
	initialDelay time.Duration
	backoff      float64
}

func newRetryingModel(inner model.LLM, initialDelay time.Duration, backoff float64) *retryingModel {
	if initialDelay <= 0 {
		initialDelay = time.Second
	}
	if backoff < 1 {
		backoff = 2.0
	}
	return &retryingModel{inner: inner, initialDelay: initialDelay, backoff: backoff}
}

func (r *retryingModel) Name() string { return r.inner.Name() }

func (r *retryingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for attempt := 0; ; attempt++ {
			emitted := false
			var callErr error
			for resp, err := range r.inner.GenerateContent(ctx, req, stream) {
				if err != nil {
					callErr = err
					break
				}
				emitted = true
				if !yield(resp, nil) {
					return
				}
			}
			if callErr == nil {
				return
			}
			// Content already streamed, user cancelled, or a hard error:
			// surface it. Only pre-stream transient failures retry.
			if emitted || ctx.Err() != nil || !isTransientError(callErr) {
				yield(nil, callErr)
				return
			}
			select {
			case <-time.After(retryBackoff(r.initialDelay, r.backoff, attempt)):
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
		}
	}
}

// retryBackoff returns initial * backoff^attempt, clamped to retryMaxDelay.
func retryBackoff(initial time.Duration, factor float64, attempt int) time.Duration {
	d := float64(initial)
	for range attempt {
		d *= factor
		if d >= float64(retryMaxDelay) {
			return retryMaxDelay
		}
	}
	if d > float64(retryMaxDelay) {
		return retryMaxDelay
	}
	return time.Duration(d)
}

// isTransientError reports whether a model-call error is worth retrying.
// OpenRouter reports transient upstream failures as an HTTP 400 whose body
// is "Provider returned error", so that wrapper is transient even though a
// bare 400 (a genuinely malformed request) is not. Unknown errors default
// to non-transient so an unclassified failure surfaces instead of hanging.
func isTransientError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	s := strings.ToLower(err.Error())

	// OpenRouter's transient upstream wrapper — checked before the hard-400
	// rule because its body also contains "400".
	if strings.Contains(s, "provider returned error") {
		return true
	}
	// Hard client errors: waiting loops on the same deterministic failure.
	for _, hard := range []string{
		"400 bad request", " 401", "401 unauthorized", " 403", " 404",
		"invalid api key", "invalid_api_key", "unauthorized", "forbidden",
		"not found", "context length", "context_length", "maximum context",
		"invalid_request",
	} {
		if strings.Contains(s, hard) {
			return false
		}
	}
	// Transient: rate limits, server errors, overload, network.
	for _, t := range []string{
		" 429", "429 too many requests", "too many requests", "rate limit",
		" 500", " 502", " 503", " 529", "internal server error",
		"bad gateway", "overloaded", "unavailable", "timeout",
		"deadline exceeded", "connection reset", "connection refused",
		"eof", "temporarily",
	} {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}
