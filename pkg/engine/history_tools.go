package engine

import (
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

const (
	historyReadDefaultLimit   = 200
	historyReadMaxLimit       = 1000
	historySearchDefaultLimit = 20
	historySearchMaxLimit     = 100
)

// The history tool names, which the elision notice and the summarizer's
// instruction refer to.
const (
	HistorySummaryToolName = "history_summary"
	HistoryReadToolName    = "history_read"
	HistorySearchToolName  = "history_search"
)

const historySummaryDescription = `Summaries of this conversation's archived history: the earlier turns auto-compaction cut out of your context window, kept as numbered chunks. Pass chunk for that chunk's full summary and line range; omit it to list every chunk with its headline.`

const historyReadDescription = `Read this conversation's archived history verbatim — user messages, replies, thinking, tool calls, and full tool results — by archive line number, forward or backward. Start from a line (from history_search or an earlier read) or from a chunk; with neither, start at the most recent chunk. Returns numbered lines and next_line to keep scanning the same way.`

const historySearchDescription = `Search this conversation's archived history by keyword. query takes words that must all appear, "exact phrases", prefix*, OR, and NOT; set literal to match an exact substring such as a path, symbol, or error message. Returns matching lines newest first with their chunk and line number; read around one with history_read.`

// HistorySummaryParams are history_summary's parameters.
type HistorySummaryParams struct {
	Chunk       int    `json:"chunk,omitempty" jsonschema:"archived chunk number; omit to list every chunk with its headline"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// HistorySummaryResult is history_summary's response.
type HistorySummaryResult struct {
	Summary string `json:"summary" jsonschema:"the chunk's summary, or the list of archived chunks"`
	Chunks  int    `json:"chunks" jsonschema:"how many chunks the archive holds"`
}

// HistoryReadParams are history_read's parameters.
type HistoryReadParams struct {
	Line        int    `json:"line,omitempty" jsonschema:"archive line to start at, from history_search or an earlier read; takes precedence over chunk"`
	Chunk       int    `json:"chunk,omitempty" jsonschema:"start at this chunk's first line, or its last line reading backward; with neither line nor chunk, the most recent chunk"`
	Direction   string `json:"direction,omitempty" jsonschema:"forward (default) or backward"`
	Limit       int    `json:"limit,omitempty" jsonschema:"maximum lines to return (default 200, at most 1000)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// HistoryReadResult is history_read's response.
type HistoryReadResult struct {
	Content    string `json:"content" jsonschema:"archive lines in order, each prefixed with its line number; a === chunk N === line marks each chunk the window touches"`
	FirstLine  int    `json:"first_line" jsonschema:"first line returned"`
	LastLine   int    `json:"last_line" jsonschema:"last line returned"`
	NextLine   int    `json:"next_line,omitempty" jsonschema:"line to pass to continue in the same direction; absent at the archive's edge"`
	TotalLines int    `json:"total_lines" jsonschema:"lines in the whole archive"`
}

// HistorySearchParams are history_search's parameters.
type HistorySearchParams struct {
	Query       string `json:"query" jsonschema:"keywords that must all appear; \"exact phrase\", prefix*, OR, and NOT also work"`
	Literal     bool   `json:"literal,omitempty" jsonschema:"match query as one exact, case-insensitive substring: paths, symbols, error messages"`
	Chunk       int    `json:"chunk,omitempty" jsonschema:"search only this chunk"`
	Limit       int    `json:"limit,omitempty" jsonschema:"maximum matching lines (default 20, at most 100)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// HistorySearchResult is history_search's response.
type HistorySearchResult struct {
	Results string `json:"results" jsonschema:"one matching line per row: chunk, line number, what the entry was, and the text"`
	Matches int    `json:"matches" jsonschema:"how many lines are listed"`
	More    bool   `json:"more,omitempty" jsonschema:"true when more lines matched than were listed"`
}

// HistoryTools returns history_summary, history_read, and history_search
// over archive. The compactor that owns archive puts them on the agent's
// requests once it holds a chunk.
func HistoryTools(archive *HistoryArchive) []tool.Tool {
	return []tool.Tool{
		mustFunctionTool(HistorySummaryToolName, historySummaryDescription,
			func(_ agent.Context, p HistorySummaryParams) (HistorySummaryResult, error) {
				return historySummary(archive, p)
			}),
		mustFunctionTool(HistoryReadToolName, historyReadDescription,
			func(_ agent.Context, p HistoryReadParams) (HistoryReadResult, error) {
				return historyReadTool(archive, p)
			}),
		mustFunctionTool(HistorySearchToolName, historySearchDescription,
			func(_ agent.Context, p HistorySearchParams) (HistorySearchResult, error) {
				return historySearchTool(archive, p)
			}),
	}
}

func mustFunctionTool[T, R any](name, description string, handler func(agent.Context, T) (R, error)) tool.Tool {
	t, err := functiontool.New[T, R](functiontool.Config{Name: name, Description: description}, handler)
	if err != nil {
		panic("failed to create tool " + name + ": " + err.Error())
	}
	return t
}

func historySummary(archive *HistoryArchive, p HistorySummaryParams) (HistorySummaryResult, error) {
	chunks, err := archive.Chunks()
	if err != nil {
		return HistorySummaryResult{}, err
	}
	if len(chunks) == 0 {
		return HistorySummaryResult{}, errHistoryEmpty
	}
	if p.Chunk == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d archived chunk(s), lines 1–%d, oldest first:\n", len(chunks), chunks[len(chunks)-1].LastLine)
		for _, c := range chunks {
			fmt.Fprintf(&b, "#%d lines %d–%d: %s\n", c.ID, c.FirstLine, c.LastLine, chunkHeadline(c))
		}
		return HistorySummaryResult{Summary: strings.TrimRight(b.String(), "\n"), Chunks: len(chunks)}, nil
	}
	c, err := archive.Chunk(p.Chunk)
	if err != nil {
		return HistorySummaryResult{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chunk #%d of %d, archived %s: lines %d–%d, entries %d–%d, about %d tokens.\n",
		c.ID, len(chunks), c.CreatedAt.Format("2006-01-02 15:04"), c.FirstLine, c.LastLine, c.FirstEntry, c.LastEntry, c.Tokens)
	if c.Summarized() {
		fmt.Fprintf(&b, "%s\n\n%s", c.Headline, c.Summary)
	} else {
		b.WriteString("No summary is available")
		if c.SummaryError != "" {
			b.WriteString(" (" + c.SummaryError + ")")
		}
		fmt.Fprintf(&b, "; read the chunk itself with %s chunk=%d.", HistoryReadToolName, c.ID)
	}
	return HistorySummaryResult{Summary: b.String(), Chunks: len(chunks)}, nil
}

// chunkHeadline is how a chunk is named in a list.
func chunkHeadline(c HistoryChunk) string {
	if c.Summarized() {
		return c.Headline
	}
	return "(no summary yet; read it with " + HistoryReadToolName + ")"
}

func historyReadTool(archive *HistoryArchive, p HistoryReadParams) (HistoryReadResult, error) {
	backward := false
	switch strings.ToLower(strings.TrimSpace(p.Direction)) {
	case "", "forward":
	case "backward":
		backward = true
	default:
		return HistoryReadResult{}, fmt.Errorf("direction must be forward or backward, got %q", p.Direction)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = historyReadDefaultLimit
	}
	limit = min(limit, historyReadMaxLimit)
	r, err := archive.read(p.Line, p.Chunk, backward, limit)
	if err != nil {
		return HistoryReadResult{}, err
	}
	out := HistoryReadResult{NextLine: r.next, TotalLines: r.total}
	if len(r.lines) == 0 {
		return out, nil
	}
	chunks, err := archive.Chunks()
	if err != nil {
		return HistoryReadResult{}, err
	}
	var b strings.Builder
	current := 0
	for _, l := range r.lines {
		if l.chunk != current {
			current = l.chunk
			b.WriteString("=== chunk " + fmt.Sprint(current))
			if current <= len(chunks) && chunks[current-1].Summarized() {
				b.WriteString(": " + chunks[current-1].Headline)
			}
			b.WriteString(" ===\n")
		}
		fmt.Fprintf(&b, "%6d\t%s\n", l.n, l.text)
	}
	out.Content = b.String()
	out.FirstLine = r.lines[0].n
	out.LastLine = r.lines[len(r.lines)-1].n
	return out, nil
}

func historySearchTool(archive *HistoryArchive, p HistorySearchParams) (HistorySearchResult, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = historySearchDefaultLimit
	}
	limit = min(limit, historySearchMaxLimit)
	hits, more, err := archive.search(p.Query, p.Literal, p.Chunk, limit)
	if err != nil {
		return HistorySearchResult{}, err
	}
	if len(hits) == 0 {
		how := "the keywords"
		if p.Literal {
			how = "the text"
		}
		return HistorySearchResult{Results: fmt.Sprintf("no archived line matches %s %q", how, p.Query)}, nil
	}
	var b strings.Builder
	for _, h := range hits {
		what := h.kind
		if h.name != "" {
			what += " " + h.name
		}
		fmt.Fprintf(&b, "chunk %d, line %d (%s): %s\n", h.chunk, h.line, what, h.snippet)
	}
	return HistorySearchResult{Results: strings.TrimRight(b.String(), "\n"), Matches: len(hits), More: more}, nil
}
