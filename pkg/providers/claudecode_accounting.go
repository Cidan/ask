package providers

import (
	"encoding/json"

	"google.golang.org/genai"
)

// ccStep accumulates the API calls the child made during one step — one
// GenerateContent. A step is usually one call; the child's native tools (its
// WebSearch fallback) can make it several.
//
// Calls are read off the stream: message_start carries a call's input and
// cache counts, message_delta its final output count. The usage on assistant
// frames is the message_start snapshot, whose output count is a placeholder
// (1–3 tokens however much the call wrote), so it is only the fallback for a
// stream that carried no message events.
type ccStep struct {
	calls      []ccUsage
	frameUsage *ccUsage
}

// observeEvent records a stream_event frame's message boundaries.
func (s *ccStep) observeEvent(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var ev struct {
		Type    string   `json:"type"`
		Usage   *ccUsage `json:"usage"`
		Message *struct {
			Usage *ccUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			s.calls = append(s.calls, *ev.Message.Usage)
		}
	case "message_delta":
		if n := len(s.calls); n > 0 && ev.Usage != nil && ev.Usage.OutputTokens > 0 {
			s.calls[n-1].OutputTokens = ev.Usage.OutputTokens
			if th := ev.Usage.OutputTokensDetails.ThinkingTokens; th > 0 {
				s.calls[n-1].OutputTokensDetails.ThinkingTokens = th
			}
		}
	}
}

// observeFrame records an assistant frame's usage as the fallback.
func (s *ccStep) observeFrame(u *ccUsage) {
	if u != nil {
		s.frameUsage = u
	}
}

// usage returns the step's accounting and the genai metadata of its last
// call, filled with Gemini's semantics like every adapter's: the prompt
// counts every input bucket, cached reads included, and the total is the
// context the child holds after the call. fallback, when non-nil, stands for
// the only call of a step that reported nothing else (a result frame alone).
func (s *ccStep) usage(fallback *ccUsage) (Usage, *genai.GenerateContentResponseUsageMetadata, bool) {
	calls := s.calls
	if len(calls) == 0 {
		switch {
		case s.frameUsage != nil:
			calls = []ccUsage{*s.frameUsage}
		case fallback != nil:
			calls = []ccUsage{*fallback}
		default:
			return Usage{}, nil, false
		}
	}
	var u Usage
	for _, c := range calls {
		u.InputTokens += c.InputTokens
		u.CacheReadTokens += c.CacheReadInputTokens
		u.CacheWriteTokens += c.CacheCreationInputTokens
		u.OutputTokens += c.OutputTokens
		u.ThinkingTokens += c.OutputTokensDetails.ThinkingTokens
	}
	last := calls[len(calls)-1]
	prompt := last.InputTokens + last.CacheReadInputTokens + last.CacheCreationInputTokens
	u.ContextTokens = prompt + last.OutputTokens
	thinking := last.OutputTokensDetails.ThinkingTokens
	md := &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        int32(prompt),
		CachedContentTokenCount: int32(last.CacheReadInputTokens),
		CandidatesTokenCount:    int32(max(last.OutputTokens-thinking, 0)),
		ThoughtsTokenCount:      int32(thinking),
		TotalTokenCount:         int32(u.ContextTokens),
	}
	return u, md, true
}
