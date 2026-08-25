package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// memoryExtractInstruction is the fixed system instruction of the
// post-turn extraction call. It is deliberately small: the call runs
// after every turn, so its size is the per-turn overhead of memory.
const memoryExtractInstruction = `You maintain a coding agent's long-term memory. From the turn below, extract only durable facts worth recalling in future sessions:
- user: the user's role, expertise, preferences
- feedback: how they want work done, with the why
- project: goals, decisions, constraints not derivable from the code or git history
- reference: where things live in external systems
Skip task progress, code, and anything the repository already records. Prefer updating a listed existing concept over adding a near-duplicate. scope is "global" for facts about the user that hold in every project, else "project".
Reply with JSON only:
{"topic":"1-2 words, reuse a listed topic when one fits","concepts":[{"op":"new|update","id":0,"kind":"user|feedback|project|reference","scope":"project|global","title":"one line","body":"self-contained detail"}]}
Use an empty concepts array when nothing durable was learned.`

const (
	// memoryExtractPromptCap and memoryExtractResponseCap bound the turn
	// text sent to the extraction call (runes): the head of the prompt,
	// the tail of the answer.
	memoryExtractPromptCap   = 2000
	memoryExtractResponseCap = 1500
	memoryExtractNearestK    = 8
	memoryExtractMaxFiles    = 10

	memoryExtractQueueSize = 32
	memoryExtractTimeout   = 90 * time.Second
)

// DebugLog is the engine's debug sink; the TUI points it at its own
// ASK_DEBUG logger. A no-op by default.
var DebugLog = func(format string, args ...any) {}

// MemoryTurn is one finished turn queued for concept extraction.
type MemoryTurn struct {
	Cwd      string
	Prompt   string
	Response string
	// Topic is the tab's current topic, offered to the model as the
	// default.
	Topic string
	// Files are the paths the turn read or edited.
	Files []string
	// Provider is the session's provider, used when the config names
	// none for memory.
	Provider string
	// OnUsage reports the extraction call's own token usage.
	OnUsage func(providerID, modelID string, inputTokens, outputTokens int)
	// OnTopic receives the topic the model settled on for the turn.
	OnTopic func(topic string)
}

// MemoryExtractorOptions configures a MemoryExtractor.
type MemoryExtractorOptions struct {
	// LoadConfig supplies the config each job reads; nil means config.Load.
	LoadConfig func() (config.Config, error)
	QueueSize  int
	Timeout    time.Duration
}

// MemoryExtractor is the background worker that turns finished turns
// into concepts: one goroutine, a bounded queue that drops the oldest
// job when full, and a context that Close cancels.
type MemoryExtractor struct {
	opts    MemoryExtractorOptions
	queue   chan MemoryTurn
	ctx     context.Context
	cancel  context.CancelFunc
	worker  sync.WaitGroup
	pending sync.WaitGroup
	mu      sync.Mutex
	closed  bool
	dropped int
}

var _ memory.Extractor = (*MemoryExtractor)(nil)

// NewMemoryExtractor starts the worker.
func NewMemoryExtractor(opts MemoryExtractorOptions) *MemoryExtractor {
	if opts.LoadConfig == nil {
		opts.LoadConfig = config.Load
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = memoryExtractQueueSize
	}
	if opts.Timeout <= 0 {
		opts.Timeout = memoryExtractTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &MemoryExtractor{
		opts:   opts,
		queue:  make(chan MemoryTurn, opts.QueueSize),
		ctx:    ctx,
		cancel: cancel,
	}
	e.worker.Add(1)
	go e.run()
	return e
}

func (e *MemoryExtractor) run() {
	defer e.worker.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case turn := <-e.queue:
			e.runJob(turn)
		}
	}
}

func (e *MemoryExtractor) runJob(turn MemoryTurn) {
	defer e.pending.Done()
	ctx, cancel := context.WithTimeout(e.ctx, e.opts.Timeout)
	defer cancel()
	if err := e.process(ctx, turn); err != nil && !errors.Is(err, context.Canceled) {
		DebugLog("memory extraction: %v", err)
	}
}

// EnqueueTurn queues a turn. It reports false when the extractor is
// closed or the turn has nothing to extract from. A full queue drops its
// oldest job so a stalled provider cannot pile up work.
func (e *MemoryExtractor) EnqueueTurn(turn MemoryTurn) bool {
	if e == nil || strings.TrimSpace(turn.Prompt) == "" || strings.TrimSpace(turn.Response) == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	e.pending.Add(1)
	for {
		select {
		case e.queue <- turn:
			return true
		default:
		}
		select {
		case <-e.queue:
			e.pending.Done()
			e.dropped++
			DebugLog("memory extraction: queue full, dropped oldest turn (%d dropped)", e.dropped)
		default:
		}
	}
}

// Enqueue adapts a memory.TurnRecord (ADK's AddSessionToMemory path).
func (e *MemoryExtractor) Enqueue(rec memory.TurnRecord) bool {
	return e.EnqueueTurn(MemoryTurn{
		Cwd:      rec.Cwd,
		Prompt:   rec.Prompt,
		Response: rec.Response,
		Topic:    rec.Topic,
		Files:    rec.Files,
	})
}

// Dropped reports how many queued turns were discarded to make room.
func (e *MemoryExtractor) Dropped() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dropped
}

// Drain blocks until every queued job has finished or ctx ends.
func (e *MemoryExtractor) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		e.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting turns, cancels the job in flight, and waits for
// the worker to exit. Queued jobs that never ran are released.
func (e *MemoryExtractor) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.mu.Unlock()
	e.cancel()
	e.worker.Wait()
	for {
		select {
		case <-e.queue:
			e.pending.Done()
		default:
			return
		}
	}
}

// MemoryExtractModel resolves the provider and model for extraction:
// the config's memory block first, then the session's provider with its
// cheapest listed model.
func MemoryExtractModel(cfg config.Config, sessionProvider string) (string, string) {
	providerID := strings.TrimSpace(cfg.Memory.Provider)
	if providerID == "" {
		providerID = strings.TrimSpace(sessionProvider)
	}
	if providerID == "" {
		providerID = strings.TrimSpace(cfg.Provider)
	}
	if providerID == "" {
		providerID = providers.DefaultProviderID()
	}
	p, ok := providers.Get(providerID)
	if !ok {
		return providerID, ""
	}
	modelID := ""
	if strings.TrimSpace(cfg.Memory.Model) != "" && (cfg.Memory.Provider == "" || cfg.Memory.Provider == providerID) {
		modelID = p.CanonicalModelID(cfg.Memory.Model, "")
	}
	if modelID == "" {
		modelID = p.CanonicalModelID(providers.CheapestModel(providerID), p.DefaultModel())
	}
	return providerID, modelID
}

type memoryExtraction struct {
	Topic    string                   `json:"topic"`
	Concepts []memoryExtractedConcept `json:"concepts"`
}

type memoryExtractedConcept struct {
	Op    string `json:"op"`
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (e *MemoryExtractor) process(ctx context.Context, turn MemoryTurn) error {
	svc := memory.Default()
	if !svc.IsOpen() {
		return nil
	}
	cfg, _ := e.opts.LoadConfig()
	providerID, modelID := MemoryExtractModel(cfg, turn.Provider)
	prov, ok := providers.Get(providerID)
	if !ok {
		return fmt.Errorf("unknown provider %q", providerID)
	}
	llm, err := ModelBuilder(ctx, prov, cfg, modelID)
	if err != nil {
		return err
	}
	defer CloseModel(llm)

	nearest, err := svc.Recall(ctx, memory.RecallQuery{
		Cwd:    turn.Cwd,
		Query:  clipHead(turn.Prompt, memoryExtractPromptCap) + "\n" + clipTail(turn.Response, memoryExtractResponseCap),
		K:      memoryExtractNearestK,
		Silent: true,
	})
	if err != nil {
		return err
	}
	topics := svc.TopicNames(ctx, turn.Cwd, memory.DefaultTopicK)
	content := MemoryExtractContent(turn, nearest.Concepts, topics)

	raw, in, out, err := generateOnce(ctx, llm, modelID, memoryExtractInstruction, content)
	if turn.OnUsage != nil && (in > 0 || out > 0) {
		turn.OnUsage(providerID, modelID, in, out)
	}
	if err != nil {
		return err
	}
	parsed, err := ParseMemoryExtraction(raw)
	if err != nil {
		return err
	}
	return applyMemoryExtraction(ctx, svc, turn, nearest.Concepts, parsed)
}

// MemoryExtractContent renders the user half of the extraction request.
func MemoryExtractContent(turn MemoryTurn, nearest []memory.Concept, topics []string) string {
	var b strings.Builder
	if turn.Topic != "" || len(topics) > 0 {
		b.WriteString("Topics: ")
		names := topics
		if turn.Topic != "" {
			names = append([]string{"current: " + turn.Topic}, topics...)
		}
		b.WriteString(strings.Join(names, ", "))
		b.WriteByte('\n')
	}
	if len(nearest) > 0 {
		b.WriteString("Existing:\n")
		for _, c := range nearest {
			fmt.Fprintf(&b, "#%d %s\n", c.ID, c.Title)
		}
	}
	if len(turn.Files) > 0 {
		files := turn.Files
		if len(files) > memoryExtractMaxFiles {
			files = files[:memoryExtractMaxFiles]
		}
		b.WriteString("Files: ")
		b.WriteString(strings.Join(files, ", "))
		b.WriteByte('\n')
	}
	b.WriteString("User:\n")
	b.WriteString(clipHead(turn.Prompt, memoryExtractPromptCap))
	b.WriteString("\nAssistant:\n")
	b.WriteString(clipTail(turn.Response, memoryExtractResponseCap))
	return b.String()
}

// ParseMemoryExtraction reads the model's JSON reply, tolerating fences
// and prose around the object.
func ParseMemoryExtraction(raw string) (memoryExtraction, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return memoryExtraction{}, fmt.Errorf("no JSON object in extraction reply: %q", clipHead(raw, 200))
	}
	var out memoryExtraction
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return memoryExtraction{}, fmt.Errorf("extraction reply: %w", err)
	}
	return out, nil
}

func applyMemoryExtraction(ctx context.Context, svc *memory.Service, turn MemoryTurn, nearest []memory.Concept, parsed memoryExtraction) error {
	known := make(map[int64]memory.Concept, len(nearest))
	for _, c := range nearest {
		known[c.ID] = c
	}
	projectScope := memory.ScopeFor(turn.Cwd)
	if projectScope == "" {
		projectScope = memory.ScopeGlobal
	}
	topic := memory.NormalizeTopic(parsed.Topic)
	if topic == "" {
		topic = memory.NormalizeTopic(turn.Topic)
	}
	var firstErr error
	for _, ec := range parsed.Concepts {
		c := memory.Concept{
			Kind:  strings.ToLower(strings.TrimSpace(ec.Kind)),
			Title: strings.TrimSpace(ec.Title),
			Body:  strings.TrimSpace(ec.Body),
			Topic: topic,
		}
		if c.Title == "" && c.Body == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(ec.Scope), memory.ScopeGlobal) {
			c.Scope = memory.ScopeGlobal
		} else {
			c.Scope = projectScope
		}
		if strings.EqualFold(ec.Op, "update") {
			if prev, ok := known[ec.ID]; ok {
				c.ID = prev.ID
				c.Scope = prev.Scope
				if c.Kind == "" {
					c.Kind = prev.Kind
				}
			}
		}
		if _, err := svc.Upsert(ctx, c); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if topic != "" {
		if err := svc.TouchTopic(ctx, turn.Cwd, topic); err != nil && firstErr == nil {
			firstErr = err
		}
		if turn.OnTopic != nil {
			turn.OnTopic(topic)
		}
	}
	return firstErr
}

// generateOnce runs one non-streaming call and returns the reply text
// with its token usage.
func generateOnce(ctx context.Context, llm model.LLM, modelID, instruction, content string) (string, int, int, error) {
	req := &model.LLMRequest{
		Model:    modelID,
		Contents: []*genai.Content{genai.NewContentFromText(content, genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(instruction, genai.RoleUser),
		},
	}
	var sb strings.Builder
	var in, out int
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", in, out, err
		}
		if resp == nil {
			continue
		}
		if resp.UsageMetadata != nil {
			in = int(resp.UsageMetadata.PromptTokenCount)
			out = int(resp.UsageMetadata.CandidatesTokenCount)
		}
		if resp.Content == nil {
			continue
		}
		for _, part := range resp.Content.Parts {
			if part != nil && part.Text != "" && !part.Thought {
				sb.WriteString(part.Text)
			}
		}
	}
	return sb.String(), in, out, nil
}

// AppendTouchedFile records the file_path of a read/write/edit call,
// once, up to memoryExtractMaxFiles.
func AppendTouchedFile(files []string, toolName string, input map[string]any) []string {
	switch toolName {
	case "read", "write", "edit":
	default:
		return files
	}
	path, _ := input["file_path"].(string)
	path = strings.TrimSpace(path)
	if path == "" || len(files) >= memoryExtractMaxFiles {
		return files
	}
	for _, f := range files {
		if f == path {
			return files
		}
	}
	return append(files, path)
}

func clipHead(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func clipTail(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

var (
	memoryExtractorMu sync.Mutex
	defaultExtractor  *MemoryExtractor
)

// EnsureMemoryExtractor returns the process-wide extractor, starting it
// (and registering it with the memory service) the first time memory is
// open. Nil while memory is closed.
func EnsureMemoryExtractor() *MemoryExtractor {
	memoryExtractorMu.Lock()
	defer memoryExtractorMu.Unlock()
	if defaultExtractor != nil {
		return defaultExtractor
	}
	svc := memory.Default()
	if !svc.IsOpen() {
		return nil
	}
	defaultExtractor = NewMemoryExtractor(MemoryExtractorOptions{})
	svc.SetExtractor(defaultExtractor)
	return defaultExtractor
}

// SetMemoryExtractor replaces the process-wide extractor (tests).
func SetMemoryExtractor(e *MemoryExtractor) {
	memoryExtractorMu.Lock()
	defer memoryExtractorMu.Unlock()
	defaultExtractor = e
	if svc := memory.Default(); svc.IsOpen() {
		if e == nil {
			svc.SetExtractor(nil)
		} else {
			svc.SetExtractor(e)
		}
	}
}

// CloseMemoryExtractor stops the process-wide extractor. Call it before
// closing the memory service so no job races the database going away.
func CloseMemoryExtractor() {
	memoryExtractorMu.Lock()
	e := defaultExtractor
	defaultExtractor = nil
	memoryExtractorMu.Unlock()
	e.Close()
}

// EnqueueMemoryTurn hands a finished turn to the process-wide extractor.
func EnqueueMemoryTurn(turn MemoryTurn) bool {
	return EnsureMemoryExtractor().EnqueueTurn(turn)
}

// IngestWorkflowMemory files a finished workflow run into memory: the
// run's source as the prompt, the model text it produced as the answer.
func IngestWorkflowMemory(ctx context.Context, sessSvc session.Service, sessionID, cwd string) {
	if !memory.IsOpen() || sessSvc == nil {
		return
	}
	resp, err := sessSvc.Get(ctx, &session.GetRequest{
		AppName:   "ask",
		UserID:    "user",
		SessionID: sessionID,
	})
	if err != nil || resp == nil || resp.Session == nil {
		return
	}
	rec := memory.TurnFromSession(resp.Session)
	rec.Cwd = cwd
	EnsureMemoryExtractor().Enqueue(rec)
}
