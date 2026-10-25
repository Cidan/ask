package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const (
	// historySummaryMaxAttempts is how many failed attempts a chunk gets
	// before it is left unsummarized; its text stays readable either way.
	historySummaryMaxAttempts = 3
	historySummaryTimeout     = 10 * time.Minute

	historyHeadlineCap = 120
	historySummaryCap  = 4000
)

// historySummarizerAgentName names the summarizer's agent and its events.
const historySummarizerAgentName = "history_summarizer"

const historySummarizerInstruction = `You summarize one archived chunk of a coding agent's conversation. Auto-compaction cut this chunk out of the agent's context window. Your summary is what the agent sees in its place, and what it reads later to decide whether to dig into the archive.

The chunk is in the user message: every user message, assistant reply, thinking block, tool call, and tool result, in order. Earlier archived chunks are listed by headline. When continuity with an earlier chunk matters, look it up with history_summary, history_search, or history_read; keep those lookups few and targeted.

Reply with plain text only, no preamble:
- First line: a headline under 100 characters naming what the chunk was about.
- Then a summary of at most 250 words: what was being worked on and why, decisions made and approaches rejected, the files, commands, and errors that mattered, the state things were left in at the end of the chunk, and open questions or next steps. Name concrete identifiers — files, functions, error messages — so they can be searched for. Do not state anything the chunk does not show.`

// ChunkSummary is a chunk's summary: a one-line headline and the body.
type ChunkSummary struct {
	Headline string
	Body     string
}

// ChunkSummarizer writes the summary of one archived chunk. It may read the
// rest of archive for continuity.
type ChunkSummarizer interface {
	SummarizeChunk(ctx context.Context, archive *HistoryArchive, chunk HistoryChunk) (ChunkSummary, error)
}

// ModelChunkSummarizer summarizes a chunk with a sub-agent on the session's
// own provider and model, built fresh through ModelBuilder so a model that
// holds its own history (Claude Code) is never shared. The sub-agent gets
// the history tools over the same archive and runs in a session of its own.
type ModelChunkSummarizer struct {
	Provider providers.Provider
	ModelID  string
	Effort   string
	// LoadConfig supplies the config the model is built with; nil means
	// config.Load.
	LoadConfig func() (config.Config, error)
	// OnUsage receives the usage record of each of the sub-agent's calls.
	OnUsage func(providers.Usage)
}

// SummarizeChunk implements ChunkSummarizer.
func (s ModelChunkSummarizer) SummarizeChunk(ctx context.Context, archive *HistoryArchive, chunk HistoryChunk) (ChunkSummary, error) {
	if s.Provider == nil {
		return ChunkSummary{}, errors.New("summarizer has no provider")
	}
	load := s.LoadConfig
	if load == nil {
		load = config.Load
	}
	cfg, _ := load()
	// The summary runs inside the agent's model callback; its own run must
	// inherit the turn's cancellation but none of the invocation ADK keeps
	// in the context.
	runCtx, cancel := context.WithTimeout(context.Background(), historySummaryTimeout)
	defer cancel()
	defer context.AfterFunc(ctx, cancel)()
	ctx = runCtx

	text, err := archive.chunkText(chunk.ID)
	if err != nil {
		return ChunkSummary{}, err
	}
	chunks, err := archive.Chunks()
	if err != nil {
		return ChunkSummary{}, err
	}

	llm, err := ModelBuilder(ctx, s.Provider, cfg, s.ModelID)
	if err != nil {
		return ChunkSummary{}, err
	}
	defer CloseModel(llm)

	genConfig := &genai.GenerateContentConfig{}
	if callOpts, _ := s.Provider.CallOptions(s.ModelID, s.Effort); callOpts != nil {
		genConfig.ThinkingConfig = callOpts.ThinkingConfig
		genConfig.MaxOutputTokens = callOpts.MaxOutputTokens
	}
	// The chunk is pinned as the first message, so only the sub-agent's own
	// lookups can ever be cut.
	compactor := NewCompactor(CompactOptions{ContextWindow: s.Provider.ContextWindow(s.ModelID), Model: llm})
	ag, err := llmagent.New(llmagent.Config{
		Name:  historySummarizerAgentName,
		Model: llm,
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return historySummarizerInstruction, nil
		},
		Tools:                 HistoryTools(archive),
		GenerateContentConfig: genConfig,
		BeforeModelCallbacks:  []llmagent.BeforeModelCallback{compactor.BeforeModel},
		AfterModelCallbacks:   []llmagent.AfterModelCallback{compactor.AfterModel},
	})
	if err != nil {
		return ChunkSummary{}, err
	}
	r, err := RunnerBuilder(ag, session.InMemoryService())
	if err != nil {
		return ChunkSummary{}, err
	}

	msg := genai.NewContentFromText(historySummaryContent(chunks, chunk, text), genai.RoleUser)
	reply := ""
	for ev, err := range r.Run(ctx, "user", "history-"+uuid.NewString(), msg, agent.RunConfig{}) {
		if err != nil {
			return ChunkSummary{}, err
		}
		if ev == nil || ev.Partial {
			continue
		}
		if u, ok := ResponseUsage(&ev.LLMResponse); ok && s.OnUsage != nil {
			s.OnUsage(u)
		}
		if ev.Content == nil || ev.Content.Role != genai.RoleModel {
			continue
		}
		var b strings.Builder
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		if t := strings.TrimSpace(b.String()); t != "" {
			reply = t
		}
	}
	if err := ctx.Err(); err != nil {
		return ChunkSummary{}, err
	}
	return parseChunkSummary(reply)
}

// historySummaryContent is the summarizer's user message: the earlier
// chunks by headline, then the chunk itself.
func historySummaryContent(chunks []HistoryChunk, chunk HistoryChunk, text string) string {
	var b strings.Builder
	var earlier []HistoryChunk
	for _, c := range chunks {
		if c.ID < chunk.ID {
			earlier = append(earlier, c)
		}
	}
	if len(earlier) == 0 {
		b.WriteString("This is the first archived chunk.\n")
	} else {
		b.WriteString("Earlier archived chunks:\n")
		for _, c := range earlier {
			fmt.Fprintf(&b, "#%d %s\n", c.ID, chunkHeadline(c))
		}
	}
	fmt.Fprintf(&b, "\nChunk #%d (archive lines %d–%d):\n\n%s", chunk.ID, chunk.FirstLine, chunk.LastLine, text)
	return b.String()
}

// parseChunkSummary splits a reply into its headline and body.
func parseChunkSummary(reply string) (ChunkSummary, error) {
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	head := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			head = i
			break
		}
	}
	if head < 0 {
		return ChunkSummary{}, errors.New("the summarizer returned no summary")
	}
	headline := strings.TrimSpace(lines[head])
	headline = strings.TrimLeft(headline, "#*-> \t")
	if len(headline) >= len("headline:") && strings.EqualFold(headline[:len("headline:")], "headline:") {
		headline = strings.TrimSpace(headline[len("headline:"):])
	}
	headline = strings.TrimSpace(strings.Trim(headline, "*_`"))
	if headline == "" {
		return ChunkSummary{}, errors.New("the summarizer returned no headline")
	}
	body := strings.TrimSpace(strings.Join(lines[head+1:], "\n"))
	if body == "" {
		body = headline
	}
	return ChunkSummary{
		Headline: clipRunes(headline, historyHeadlineCap),
		Body:     clipRunes(body, historySummaryCap),
	}, nil
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
