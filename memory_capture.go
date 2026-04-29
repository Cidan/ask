package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// memoryCaptureOutcomeChars is the floor for the assistant-text
// snippet stored alongside each observation. The snippet rounds
// forward from this floor to the next sentence boundary, so observed
// snippets are always ≥ this many chars and complete at sentence
// granularity. 200 is a comfortable opening line + 1-2 sentences;
// noisier turns get more, never less.
const memoryCaptureOutcomeChars = 200

// memoryTurn accumulates the per-turn signal we feed to memmy at
// turn end. It is reset on each new user prompt and flushed on the
// turnCompleteMsg that closes that prompt's run.
//
// The fields stay zero-valued until a user prompt populates them
// (which sets tools / files maps). flushMemoryTurn checks for the
// nil-map case to detect "no turn in flight" — capture is best-
// effort and must not panic when something fires out of order.
type memoryTurn struct {
	prompt   string
	tools    map[string]struct{}
	files    map[string]struct{}
	response strings.Builder
}

// resetMemoryTurn starts a fresh accumulation window for the supplied
// prompt. Called from sendToProvider so every user submission begins
// its own scope.
func (m *model) resetMemoryTurn(prompt string) {
	m.currentTurn = memoryTurn{
		prompt: prompt,
		tools:  map[string]struct{}{},
		files:  map[string]struct{}{},
	}
}

// recordToolCall accumulates a tool name and (when applicable) the
// file path it operated on. No-op when no turn is in flight.
func (m *model) recordToolCall(name string, input map[string]any) {
	if m.currentTurn.tools == nil {
		return
	}
	m.currentTurn.tools[name] = struct{}{}
	if p := toolFilePath(name, input); p != "" {
		m.currentTurn.files[p] = struct{}{}
	}
}

// recordAssistantText appends streamed assistant text to the response
// builder. No-op when no turn is in flight.
func (m *model) recordAssistantText(text string) {
	if m.currentTurn.tools == nil {
		return
	}
	m.currentTurn.response.WriteString(text)
}

// flushMemoryTurn writes the accumulated turn observations to memmy
// and resets state. Best-effort: a closed service quietly no-ops via
// memoryWrite; embedder errors are debug-logged but do not interrupt
// the user-facing flow.
//
// Two kinds of writes go out:
//
//  1. Per-file observations — one Write per file touched, each
//     subjected to its own sentence-rounded snippet of the response
//     so recall on a specific path lands a node about that path.
//  2. Turn summary — one Write that ties prompt + tools + files +
//     outcome. The structural prefix anchors the embedding to
//     concrete nouns rather than verbal filler.
//
// Quality gate: turns that produced no tool calls, touched no files,
// AND yielded < 100 chars of response are skipped entirely. Catches
// "what's 2+2" / "thanks!" turns without also catching genuine
// conceptual conversation, which usually crosses 100 chars.
func (m *model) flushMemoryTurn() {
	t := m.currentTurn
	// Always reset first — a slow Write must not double-fire if a
	// second turnCompleteMsg arrives before this one returns.
	m.currentTurn = memoryTurn{}
	if t.tools == nil {
		return
	}
	prompt := strings.TrimSpace(t.prompt)
	if prompt == "" {
		return
	}
	response := strings.TrimSpace(t.response.String())
	fileList := sortedSetKeys(t.files)
	toolList := sortedSetKeys(t.tools)
	// Quality gate: ignore turns with no tools, no files, and a tiny
	// response. Conversational pings ("ok", "thanks") deserve no
	// permanent residue in the corpus.
	if len(toolList) == 0 && len(fileList) == 0 && len(response) < 100 {
		return
	}
	cwd := m.cwd
	if cwd == "" {
		return
	}

	ctx := context.Background()
	// One Write per file touched. Each per-file snippet first tries
	// to extract sentences that mention the file by path or basename,
	// then falls back to the response's leading content — so the
	// node's text concentrates around that specific subject when the
	// assistant talked about it explicitly, and stays generic
	// otherwise.
	for _, file := range fileList {
		snippet := outcomeSnippet(perFileSnippet(response, file), memoryCaptureOutcomeChars)
		obs := formatPerFileObservation(file, prompt, snippet)
		if err := memoryWrite(ctx, cwd, obs); err != nil {
			debugLog("memory write per-file %s: %v", file, err)
		}
	}
	// One Write for the turn-level summary. memmy's chunker may
	// further split this into 1-2 nodes; the leading prompt: line
	// keeps the embedding anchored even when chunked.
	summary := formatTurnSummary(prompt, toolList, fileList, outcomeSnippet(response, memoryCaptureOutcomeChars))
	if err := memoryWrite(ctx, cwd, summary); err != nil {
		debugLog("memory write turn-summary: %v", err)
	}
}

// toolFilePath extracts the file path argument from a tool's input
// map, covering the standard file-touching tools (Read/Edit/Write/
// MultiEdit + NotebookEdit). Returns "" when the tool doesn't operate
// on a file.
func toolFilePath(name string, input map[string]any) string {
	if input == nil {
		return ""
	}
	switch name {
	case "Read", "Edit", "Write", "MultiEdit":
		if p, _ := input["file_path"].(string); p != "" {
			return p
		}
	case "NotebookEdit":
		if p, _ := input["notebook_path"].(string); p != "" {
			return p
		}
	}
	return ""
}

func formatPerFileObservation(file, prompt, snippet string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "edited %s\n", file)
	fmt.Fprintf(&b, "prompt: %s\n", prompt)
	if snippet != "" {
		fmt.Fprintf(&b, "outcome: %s\n", snippet)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatTurnSummary(prompt string, tools, files []string, outcome string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "prompt: %s\n", prompt)
	if len(files) > 0 {
		fmt.Fprintf(&b, "files: %s\n", strings.Join(files, ", "))
	}
	if len(tools) > 0 {
		fmt.Fprintf(&b, "tools: %s\n", strings.Join(tools, ", "))
	}
	if outcome != "" {
		fmt.Fprintf(&b, "outcome: %s\n", outcome)
	}
	return strings.TrimRight(b.String(), "\n")
}

// outcomeSnippet returns text trimmed to roughly maxChars but always
// rounded forward to the next sentence boundary so the snippet ends
// at a complete thought. When the rounded boundary would extend more
// than 200 runes past maxChars (a long sentence), we give up the
// rounding and trim hard at maxChars to stay reasonably close to
// budget.
func outcomeSnippet(text string, maxChars int) string {
	text = strings.TrimSpace(text)
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	limit := maxChars + 200
	if limit > len(runes) {
		limit = len(runes)
	}
	for i := maxChars; i < limit; i++ {
		if isSentenceBoundary(runes, i) {
			return strings.TrimSpace(string(runes[:i+1]))
		}
	}
	return strings.TrimSpace(string(runes[:maxChars]))
}

// perFileSnippet pulls sentences from response that mention the file
// — by full path or by basename — so per-file observations get
// pinpointed context rather than the response's generic intro.
// Returns the leading content of the response when nothing matches,
// which still beats an empty snippet.
func perFileSnippet(response, filePath string) string {
	if response == "" {
		return ""
	}
	base := filePath
	if i := strings.LastIndex(filePath, "/"); i >= 0 {
		base = filePath[i+1:]
	}
	sentences := splitSentences(response)
	matched := make([]string, 0, 3)
	for _, s := range sentences {
		if strings.Contains(s, filePath) || (base != filePath && strings.Contains(s, base)) {
			matched = append(matched, strings.TrimSpace(s))
			if len(matched) >= 3 {
				break
			}
		}
	}
	if len(matched) == 0 {
		return response
	}
	return strings.Join(matched, " ")
}

// splitSentences chops text on sentence-ending punctuation. Naive but
// good enough for the perFileSnippet use case; we are scanning
// assistant text, not formal prose, so unicode sentence-segmentation
// would be overkill. The wrinkle this version corrects: a bare
// rune-equality check on '.' would split "auth.go" into two
// "sentences," producing chunks that no longer contain the file
// path and dooming the file-mention match. isSentenceBoundary fixes
// that by requiring whitespace or end-of-input to follow a '.'.
func splitSentences(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	start := 0
	runes := []rune(text)
	for i := range runes {
		if !isSentenceBoundary(runes, i) {
			continue
		}
		seg := strings.TrimSpace(string(runes[start : i+1]))
		if seg != "" {
			out = append(out, seg)
		}
		start = i + 1
	}
	if start < len(runes) {
		seg := strings.TrimSpace(string(runes[start:]))
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// isSentenceBoundary reports whether runes[i] is a sentence-ending
// punctuation that does not also live inside a token. The heuristic:
//   - '!', '?', and '\n' are always boundaries.
//   - '.' is a boundary only when followed by whitespace or end-of-
//     input. Anything else (alphanumerics, '/', etc.) means it is
//     part of a token like "auth.go" or "127.0.0.1".
//
// This is deliberately not Unicode-perfect — assistant prose is the
// only thing we feed it and a one-rune lookahead handles the file-
// extension and dotted-IP cases that matter for memmy capture.
func isSentenceBoundary(runes []rune, i int) bool {
	if i < 0 || i >= len(runes) {
		return false
	}
	r := runes[i]
	switch r {
	case '!', '?', '\n':
		return true
	case '.':
		if i+1 >= len(runes) {
			return true
		}
		switch runes[i+1] {
		case ' ', '\n', '\t', '\r':
			return true
		}
		return false
	}
	return false
}

func sortedSetKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
