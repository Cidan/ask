package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	// CompactTriggerRatio is the fraction of the context window at which the
	// conversation is compacted.
	CompactTriggerRatio = 0.90
	// CompactTargetRatio is the fraction of the context window the retained
	// history is cut down to.
	CompactTargetRatio = 0.50

	// compactMinRetained is the smallest number of contents a cut may leave
	// behind. Below this the history is short enough that the system prompt,
	// not the conversation, is what fills the window.
	compactMinRetained = 2

	// compactImageTokens is the per-image token estimate. Gemini bills 258
	// tokens for a tile and scales with resolution; images are rare enough
	// next to text that one conservative constant beats decoding the bytes.
	compactImageTokens = 1500

	// compactPartOverhead covers the wire framing of a single function call or
	// response (name, role, delimiters) that the character estimate misses.
	compactPartOverhead = 8
)

// compactElisionNotice is spliced in where history was dropped so the model
// knows its view is partial rather than silently inventing continuity.
const compactElisionNotice = "[Earlier turns in this conversation were elided to fit the context window. " +
	"The first message above is the original request; everything between it and this point is gone. " +
	"Ask the user to restate anything you need rather than guessing at it.]"

// CompactOptions configures a Compactor.
type CompactOptions struct {
	// ContextWindow is the model's input window in tokens. A non-positive
	// window disables compaction: without a denominator there is no ratio.
	ContextWindow int64
	// Disabled turns the compactor into a pass-through. Set for providers
	// that manage their own context and when the user has switched the
	// feature off.
	Disabled bool
	// Notify, when set, is called once per compaction with the outcome.
	Notify func(CompactionResult)
}

// CompactionResult describes one compaction.
type CompactionResult struct {
	// DroppedContents is how many leading conversation entries were removed.
	DroppedContents int
	// BeforeTokens is the usage reading that triggered the compaction.
	BeforeTokens int
	// AfterTokens is the estimated usage of the request actually sent.
	AfterTokens int
	// ContextWindow is the denominator both readings are measured against.
	ContextWindow int64
}

// Compactor keeps a conversation inside its model's context window by
// dropping the oldest turns from the request at send time.
//
// It never touches the stored transcript: ADK rebuilds LLMRequest.Contents
// from the append-only event log on every model call, so the cut is a view
// the model sees and /resume does not. The consequence is that the cut has to
// be re-derived per call, which is why the boundary is remembered as a
// watermark — without one the next call would observe the reduced usage, not
// trigger, send the full history again, and oscillate.
type Compactor struct {
	opts CompactOptions

	mu sync.Mutex
	// lastTotal is the most recent total-token reading, the same number the
	// sidebar meter shows. It drives the trigger because this turn's total
	// is next turn's prompt.
	lastTotal int
	// lastPrompt is the most recent prompt-token reading. It calibrates the
	// estimator against lastSentEst, which is also prompt-only.
	lastPrompt int
	// lastSentEst is the local estimate of the request last handed to the
	// model — post-cut, so the calibration divides like against like.
	lastSentEst int
	// watermark identifies the first content retained by the last cut.
	// Everything before it stays dropped for the rest of the session.
	watermark contentMark
}

// contentMark identifies one content across rebuilds of the request. The
// fingerprint alone is not enough: a transcript can repeat a content verbatim
// (the same tool called twice with the same arguments), and matching the
// earliest copy would silently restore history a previous cut removed. The
// ordinal — how many identical contents precede this one — disambiguates, and
// stays valid because the event log only ever grows at the end.
type contentMark struct {
	fingerprint string
	ordinal     int
}

func (m contentMark) isZero() bool { return m.fingerprint == "" }

// NewCompactor returns a Compactor for opts.
func NewCompactor(opts CompactOptions) *Compactor {
	return &Compactor{opts: opts}
}

// ObserveUsage records the token readings of the last model call.
func (c *Compactor) ObserveUsage(promptTokens, totalTokens int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if totalTokens > 0 {
		c.lastTotal = totalTokens
	}
	if promptTokens > 0 {
		c.lastPrompt = promptTokens
	}
}

// BeforeModel is an llmagent.BeforeModelCallback. It rewrites req.Contents in
// place and never short-circuits the model call.
func (c *Compactor) BeforeModel(_ agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	if c == nil || req == nil {
		return nil, nil
	}
	c.Apply(req)
	return nil, nil
}

// Apply performs the compaction on req. It is the testable half of
// BeforeModel.
func (c *Compactor) Apply(req *model.LLMRequest) {
	if c == nil || req == nil || c.opts.Disabled || c.opts.ContextWindow <= 0 {
		return
	}

	c.mu.Lock()
	observed := c.lastTotal
	prompt := c.lastPrompt
	sentEst := c.lastSentEst
	watermark := c.watermark
	c.mu.Unlock()

	window := int(c.opts.ContextWindow)
	contents := req.Contents
	if len(contents) == 0 {
		return
	}

	// The system instruction and tool declarations are not in Contents and
	// cannot be dropped, so they are a floor the target has to clear.
	floor := estimateOverheadTokens(req)

	// Calibrate the estimator against the previous call: real prompt tokens
	// over the estimate of the request that produced them. Both sides are
	// prompt-only and post-cut, so they measure the same thing.
	//
	// Before the first calibrated call — a resumed session that is already
	// over the trigger — fall back to the total against the request in hand.
	// It conflates output with prompt and the content set may have moved on,
	// but it beats assuming the estimator is perfectly scaled.
	scale := 1.0
	switch {
	case prompt > 0 && sentEst > 0:
		scale = float64(prompt) / float64(sentEst)
	case observed > 0:
		if est := estimateContentsTokens(contents) + floor; est > 0 {
			scale = float64(observed) / float64(est)
		}
	}
	if scale < 0.25 {
		scale = 0.25
	} else if scale > 4 {
		scale = 4
	}
	defer func() {
		c.mu.Lock()
		c.lastSentEst = estimateContentsTokens(req.Contents) + floor
		c.mu.Unlock()
	}()

	// Re-apply the standing watermark first: the model must not see history
	// that a previous compaction already cut away. Without this the reduced
	// usage would read as "no need to compact", the full history would go
	// back out, and the session would oscillate across the trigger.
	start, found := locateMark(contents, watermark)
	if !watermark.isZero() && !found {
		c.mu.Lock()
		c.watermark = contentMark{}
		c.mu.Unlock()
	}

	triggered := observed > 0 && float64(observed) >= CompactTriggerRatio*float64(window)
	if triggered {
		budget := int(CompactTargetRatio*float64(window)) - int(float64(floor)*scale)
		if cut := planCut(contents[start:], budget, scale); cut > 0 {
			start += cut
			c.mu.Lock()
			c.watermark = markContent(contents, start)
			c.mu.Unlock()
		}
	}

	dropped := start - 1
	if dropped <= 0 {
		return
	}

	rebuilt := make([]*genai.Content, 0, len(contents)-dropped+1)
	rebuilt = append(rebuilt, contents[0])
	rebuilt = append(rebuilt, genai.NewContentFromText(compactElisionNotice, genai.RoleUser))
	rebuilt = append(rebuilt, contents[start:]...)
	req.Contents = rebuilt

	if triggered && c.opts.Notify != nil {
		c.opts.Notify(CompactionResult{
			DroppedContents: dropped,
			BeforeTokens:    observed,
			AfterTokens:     int(float64(estimateContentsTokens(rebuilt)+floor) * scale),
			ContextWindow:   c.opts.ContextWindow,
		})
	}
}

// locateMark returns the index of the content mark identifies, or false when
// it no longer occurs — a /clear, a resumed transcript, a different session.
func locateMark(contents []*genai.Content, mark contentMark) (int, bool) {
	if mark.isZero() {
		return 0, false
	}
	seen := 0
	for i := range contents {
		if fingerprintContent(contents[i]) != mark.fingerprint {
			continue
		}
		if seen == mark.ordinal {
			return i, true
		}
		seen++
	}
	return 0, false
}

// markContent builds the watermark for contents[i].
func markContent(contents []*genai.Content, i int) contentMark {
	fp := fingerprintContent(contents[i])
	ordinal := 0
	for j := 0; j < i; j++ {
		if fingerprintContent(contents[j]) == fp {
			ordinal++
		}
	}
	return contentMark{fingerprint: fp, ordinal: ordinal}
}

// planCut returns the index contents should be truncated to so the retained
// suffix fits budget, or 0 when no valid cut helps.
//
// Only a genuine user turn is a valid boundary. Cutting anywhere else strands
// a tool call without its result (or the reverse), which no provider in
// pkg/providers repairs and which OpenRouter rejects with a non-retryable
// 400. The earliest qualifying cut wins so the least history is lost.
func planCut(contents []*genai.Content, budget int, scale float64) int {
	if scale <= 0 {
		scale = 1
	}
	cuts := cutIndices(contents)
	if len(cuts) == 0 {
		return 0
	}
	// Suffix sums, so each candidate is a lookup rather than a re-walk of the
	// tail — the estimator marshals every tool payload it touches.
	suffix := make([]int, len(contents)+1)
	for i := len(contents) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + estimateContentTokens(contents[i])
	}
	for _, cut := range cuts {
		if len(contents)-cut < compactMinRetained {
			break
		}
		if int(float64(suffix[cut])*scale) <= budget {
			return cut
		}
	}
	// Nothing fits: the tail alone exceeds the budget. Keep the last valid
	// boundary so the request at least shrinks instead of giving up.
	for i := len(cuts) - 1; i >= 0; i-- {
		if len(contents)-cuts[i] >= compactMinRetained {
			return cuts[i]
		}
	}
	return 0
}

// cutIndices returns, in ascending order, every index at which contents may be
// truncated without orphaning a tool call or result.
//
// Index 0 is never a candidate: it is the pinned original request. A candidate
// is a content authored by the user that carries no function response — role
// alone is not enough, because ADK builds tool-result contents with the user
// role too (base_flow.go, the function-response event).
func cutIndices(contents []*genai.Content) []int {
	var out []int
	for i := 1; i < len(contents); i++ {
		if isUserTurn(contents[i]) {
			out = append(out, i)
		}
	}
	return out
}

func isUserTurn(c *genai.Content) bool {
	if c == nil || len(c.Parts) == 0 {
		return false
	}
	if c.Role != genai.RoleUser && c.Role != "" {
		return false
	}
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		if p.FunctionResponse != nil || p.FunctionCall != nil {
			return false
		}
	}
	return true
}

// fingerprintContent identifies a content across rebuilds of the request.
// Function call IDs are excluded: ADK strips the ones it generated before a
// callback ever sees them, so they are absent on the Gemini path.
func fingerprintContent(c *genai.Content) string {
	if c == nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(c.Role))
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		h.Write([]byte{0})
		switch {
		case p.FunctionCall != nil:
			h.Write([]byte("fc:" + p.FunctionCall.Name))
			if b, err := json.Marshal(p.FunctionCall.Args); err == nil {
				h.Write(b)
			}
		case p.FunctionResponse != nil:
			h.Write([]byte("fr:" + p.FunctionResponse.Name))
			if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
				h.Write(b)
			}
		case p.InlineData != nil:
			h.Write([]byte("img:" + strconv.Itoa(len(p.InlineData.Data))))
		default:
			if p.Thought {
				h.Write([]byte("th:"))
			}
			h.Write([]byte(p.Text))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// estimateContentsTokens estimates the token cost of a slice of contents.
func estimateContentsTokens(contents []*genai.Content) int {
	var total int
	for _, c := range contents {
		total += estimateContentTokens(c)
	}
	return total
}

// estimateContentTokens estimates the token cost of one content. Thoughts,
// function calls, function responses and images all count: they are sent back
// to the model and billed as input, and they are the bulk of an agentic
// transcript.
func estimateContentTokens(c *genai.Content) int {
	if c == nil {
		return 0
	}
	total := 4
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			total += tokensForChars(len(p.FunctionCall.Name)) + compactPartOverhead
			if b, err := json.Marshal(p.FunctionCall.Args); err == nil {
				total += tokensForChars(len(b))
			}
		case p.FunctionResponse != nil:
			total += tokensForChars(len(p.FunctionResponse.Name)) + compactPartOverhead
			if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
				total += tokensForChars(len(b))
			}
		case p.InlineData != nil:
			total += compactImageTokens
		case p.FileData != nil:
			total += compactImageTokens
		default:
			total += tokensForChars(len(p.Text))
		}
	}
	return total
}

// estimateOverheadTokens estimates the untruncatable part of a request: the
// system instruction and the tool declarations.
func estimateOverheadTokens(req *model.LLMRequest) int {
	if req == nil || req.Config == nil {
		return 0
	}
	total := estimateContentTokens(req.Config.SystemInstruction)
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, d := range t.FunctionDeclarations {
			if d == nil {
				continue
			}
			total += tokensForChars(len(d.Name)+len(d.Description)) + compactPartOverhead
			if d.Parameters != nil {
				if b, err := json.Marshal(d.Parameters); err == nil {
					total += tokensForChars(len(b))
				}
			}
		}
	}
	return total
}

func tokensForChars(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + 3) / 4
}

// CompactionSummary renders a result as the one-line notice the UI shows.
func CompactionSummary(r CompactionResult) string {
	pct := 0
	if r.ContextWindow > 0 {
		pct = int(float64(r.AfterTokens) * 100 / float64(r.ContextWindow))
	}
	return fmt.Sprintf("compacted context: dropped %d earlier entries, now ~%d%% of the window", r.DroppedContents, pct)
}

// AutoCompactEnabled reports whether auto-compaction is on. It is on unless
// the user has explicitly turned it off.
func AutoCompactEnabled(cfg config.Config) bool {
	return cfg.AutoCompact == nil || *cfg.AutoCompact
}
