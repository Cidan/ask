package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
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

	// compactScaleMin and compactScaleMax bound the estimator calibration, so
	// one odd reading cannot make the compactor cut everything or nothing.
	compactScaleMin = 0.25
	compactScaleMax = 4
)

// compactElisionNotice is spliced in where history was dropped so the model
// knows its view is partial rather than silently inventing continuity. A cut
// can land mid-task, between two tool calls, so it covers tool output too.
const compactElisionNotice = "[Earlier turns in this conversation were elided to fit the context window. " +
	"The first message above is the original request; everything between it and this point is gone, " +
	"including the output of any tool calls made in that span. Run a tool again if you need its output; " +
	"ask the user to restate anything else rather than guessing at it.]"

var compactNoticeTokens = estimateContentTokens(genai.NewContentFromText(compactElisionNotice, genai.RoleUser))

// CompactOptions configures a Compactor.
type CompactOptions struct {
	// ContextWindow is the model's input window in tokens. A non-positive
	// window disables compaction: without a denominator there is no ratio.
	ContextWindow int64
	// Disabled turns the compactor into a pass-through.
	Disabled bool
	// Model is the model the compactor's agent calls. Whenever the cut moves
	// it is told through providers.RebaseHistory, so a model that holds the
	// conversation itself (Claude Code) holds exactly what ask now sends.
	Model model.LLM
	// Notify, when set, is called once per compaction with the outcome.
	Notify func(CompactionResult)
}

// CompactionResult describes one compaction.
type CompactionResult struct {
	// DroppedContents is how many leading conversation entries were removed.
	DroppedContents int
	// BeforeTokens is the estimated usage of the request before the cut.
	BeforeTokens int
	// AfterTokens is the estimated usage of the request actually sent.
	AfterTokens int
	// ContextWindow is the denominator both readings are measured against.
	ContextWindow int64
}

// Compactor keeps one agent's conversation inside its model's context window
// by dropping the oldest turns from the request at send time. It is the same
// on every provider: a model that holds its own history is rebuilt from the
// cut view rather than exempted.
//
// It never touches the stored transcript: ADK rebuilds LLMRequest.Contents
// from the append-only event log on every model call, so the cut is a view
// the model sees and /resume does not. The consequence is that the cut has to
// be re-derived per call, which is why the boundary is remembered as a
// watermark — without one the next call would observe the reduced usage, not
// trigger, send the full history again, and oscillate.
//
// Register BeforeModel and AfterModel on the same agent; one Compactor serves
// one agent and one model.
type Compactor struct {
	opts CompactOptions

	mu sync.Mutex
	// lastTotal is the latest call's total-token reading: what the model held
	// once its reply was written. Zero until a call reports usage.
	lastTotal int
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

// ObserveUsage records the total-token reading of the latest model call.
// Zero readings (metadata-only stream chunks) are ignored.
func (c *Compactor) ObserveUsage(totalTokens int) {
	if c == nil || totalTokens <= 0 {
		return
	}
	c.mu.Lock()
	c.lastTotal = totalTokens
	c.mu.Unlock()
}

// AfterModel is an llmagent.AfterModelCallback that feeds ObserveUsage from
// every response. It never replaces the response.
func (c *Compactor) AfterModel(_ agent.Context, resp *model.LLMResponse, _ error) (*model.LLMResponse, error) {
	if c == nil || resp == nil || resp.UsageMetadata == nil {
		return nil, nil
	}
	md := resp.UsageMetadata
	total := int(md.TotalTokenCount)
	if total == 0 {
		total = int(md.PromptTokenCount) + int(md.CandidatesTokenCount)
	}
	c.ObserveUsage(total)
	return nil, nil
}

// BeforeModel is an llmagent.BeforeModelCallback. It rewrites req.Contents in
// place and never short-circuits the model call.
func (c *Compactor) BeforeModel(_ agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	c.Apply(req)
	return nil, nil
}

// Reset drops the standing cut, so the next request goes out whole. Sessions
// call it in place of BeforeModel while auto-compaction is switched off.
func (c *Compactor) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	had := !c.watermark.isZero()
	c.watermark = contentMark{}
	c.mu.Unlock()
	if had {
		providers.RebaseHistory(c.opts.Model)
	}
}

// Apply performs the compaction on req. It is the testable half of
// BeforeModel.
func (c *Compactor) Apply(req *model.LLMRequest) {
	if c == nil || req == nil || c.opts.Disabled || c.opts.ContextWindow <= 0 || len(req.Contents) == 0 {
		return
	}
	result, moved := c.cut(req)
	if moved {
		providers.RebaseHistory(c.opts.Model)
	}
	if result != nil && c.opts.Notify != nil {
		c.opts.Notify(*result)
	}
}

// cut rewrites req.Contents to the compacted view. It returns the outcome of
// a new cut (nil when none was made) and whether the view's start moved — a
// new cut, or a watermark that no longer resolves.
func (c *Compactor) cut(req *model.LLMRequest) (*CompactionResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	contents := req.Contents
	window := float64(c.opts.ContextWindow)
	// The system instruction and tool declarations are not in Contents and
	// cannot be dropped, so they are a floor the target has to clear.
	floor := estimateOverheadTokens(req)

	// Re-apply the standing watermark first: the model must not see history
	// that a previous compaction already cut away. Without this the reduced
	// usage would read as "no need to compact", the full history would go
	// back out, and the session would oscillate across the trigger.
	moved := false
	start := 0
	if !c.watermark.isZero() {
		if i, ok := locateMark(contents, c.watermark); ok {
			start = i
		} else {
			c.watermark = contentMark{}
			moved = true
		}
	}

	scale := c.scaleLocked(contents, start, floor)
	used := int(scale * float64(viewTokens(contents, start, len(contents))+floor))

	var result *CompactionResult
	if float64(used) >= CompactTriggerRatio*window {
		head := estimateContentTokens(contents[0]) + compactNoticeTokens
		budget := int(CompactTargetRatio*window/scale) - floor - head
		// From the uncut history the first useful cut is 2: index 0 is pinned
		// and a cut at 1 would drop nothing.
		minCut := 1
		if start == 0 {
			minCut = 2
		}
		if cut := planCut(contents[start:], budget, minCut); cut > 0 {
			start += cut
			c.watermark = markContent(contents, start)
			moved = true
			result = &CompactionResult{BeforeTokens: used}
		}
	}

	if start > 1 {
		rebuilt := make([]*genai.Content, 0, len(contents)-start+2)
		rebuilt = append(rebuilt, contents[0])
		rebuilt = append(rebuilt, genai.NewContentFromText(compactElisionNotice, genai.RoleUser))
		rebuilt = append(rebuilt, contents[start:]...)
		req.Contents = rebuilt
	}
	if result != nil {
		result.DroppedContents = start - 1
		result.AfterTokens = int(scale * float64(estimateContentsTokens(req.Contents)+floor))
		result.ContextWindow = c.opts.ContextWindow
	}
	return result, moved
}

// scaleLocked calibrates the character estimate against the provider's own
// count. The latest total reading is what the model held after its reply:
// the view as it was sent, up to and including that reply — the request in
// hand up to its last model content. Only the total is used because it means
// the same thing on every provider; a prompt count does not (Claude Code
// reports only the uncached slice of its input there). Before any reading —
// a resumed session — the estimate stands as is. Caller holds c.mu.
func (c *Compactor) scaleLocked(contents []*genai.Content, start, floor int) float64 {
	if c.lastTotal <= 0 {
		return 1
	}
	end := len(contents)
	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i] != nil && contents[i].Role == genai.RoleModel {
			end = i + 1
			break
		}
	}
	est := viewTokens(contents, start, end) + floor
	if est <= 0 {
		return 1
	}
	scale := float64(c.lastTotal) / float64(est)
	if scale < compactScaleMin {
		scale = compactScaleMin
	} else if scale > compactScaleMax {
		scale = compactScaleMax
	}
	return scale
}

// viewTokens estimates contents[:end] as the model sees it with the cut at
// start: the pinned first content, the elision notice, then
// contents[start:end]. A start of 0 or 1 is no cut.
func viewTokens(contents []*genai.Content, start, end int) int {
	if start <= 1 {
		return estimateContentsTokens(contents[:end])
	}
	n := estimateContentTokens(contents[0]) + compactNoticeTokens
	if end > start {
		n += estimateContentsTokens(contents[start:end])
	}
	return n
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
// suffix fits budget (in estimator units), or 0 when the history already fits
// or no cut at or after minCut helps. The earliest qualifying cut wins so the
// least history is lost.
func planCut(contents []*genai.Content, budget, minCut int) int {
	cuts := cutIndices(contents, minCut)
	if len(cuts) == 0 {
		return 0
	}
	// Suffix sums, so each candidate is a lookup rather than a re-walk of the
	// tail — the estimator marshals every tool payload it touches.
	suffix := make([]int, len(contents)+1)
	for i := len(contents) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + estimateContentTokens(contents[i])
	}
	// Without a cut, what is kept starts just before the first valid cut.
	if suffix[max(minCut, 1)-1] <= budget {
		return 0
	}
	for _, cut := range cuts {
		if len(contents)-cut < compactMinRetained {
			break
		}
		if suffix[cut] <= budget {
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

// cutIndices returns, in ascending order, every index at or after minCut (and
// never 0, the pinned original request) where contents may be truncated
// without orphaning a tool call or result.
//
// ADK puts a tool's result in the content right after its call, so a cut is
// valid anywhere except on a result: at a user message, or at a model reply —
// between two tool round-trips of one turn, which is the only place a long
// agentic turn can be cut at all. Cutting on a result strands it without its
// call, which no provider repairs and which OpenRouter rejects with a
// non-retryable 400.
func cutIndices(contents []*genai.Content, minCut int) []int {
	var out []int
	for i := max(minCut, 1); i < len(contents); i++ {
		if isCutPoint(contents[i]) {
			out = append(out, i)
		}
	}
	return out
}

// isCutPoint reports whether the retained history may start at c: a
// non-empty content carrying no function response. Role is no guide — ADK
// builds tool-result contents with the user role too (base_flow.go, the
// function-response event).
func isCutPoint(c *genai.Content) bool {
	if c == nil || len(c.Parts) == 0 {
		return false
	}
	for _, p := range c.Parts {
		if p != nil && p.FunctionResponse != nil {
			return false
		}
	}
	return true
}

// fingerprintContent identifies a content across rebuilds of the request by
// its role and first part. Only the first part counts: request hooks append
// parts to the latest user message on every call (the memory recall block,
// which moves on to the next message a turn later), and that must not change
// the message's identity or a watermark on it would stop resolving. Function
// call IDs are excluded: ADK strips the ones it generated before a callback
// ever sees them, so they are absent on the Gemini path.
func fingerprintContent(c *genai.Content) string {
	if c == nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(c.Role))
	var first *genai.Part
	for _, p := range c.Parts {
		if p != nil {
			first = p
			break
		}
	}
	switch {
	case first == nil:
	case first.FunctionCall != nil:
		h.Write([]byte("fc:" + first.FunctionCall.Name))
		if b, err := json.Marshal(first.FunctionCall.Args); err == nil {
			h.Write(b)
		}
	case first.FunctionResponse != nil:
		h.Write([]byte("fr:" + first.FunctionResponse.Name))
		if b, err := json.Marshal(first.FunctionResponse.Response); err == nil {
			h.Write(b)
		}
	case first.InlineData != nil:
		h.Write([]byte("img:" + strconv.Itoa(len(first.InlineData.Data))))
	default:
		if first.Thought {
			h.Write([]byte("th:"))
		}
		h.Write([]byte(first.Text))
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
			if d.ParametersJsonSchema != nil {
				if b, err := json.Marshal(d.ParametersJsonSchema); err == nil {
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
