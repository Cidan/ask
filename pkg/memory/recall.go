package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultRecallK is how many concepts a per-turn recall returns.
	DefaultRecallK = 15
	// DefaultTopK is how many top-weighted concepts open a session.
	DefaultTopK = 15
	// DefaultBodies is how many of the leading concepts carry their body
	// inline; the rest are titles the model can expand with load_memory.
	DefaultBodies = 3
	// DefaultTopicK bounds the topic list shown to the model.
	DefaultTopicK = 20
	// DefaultHookTimeout caps how long recall operations can take.
	DefaultHookTimeout = 8 * time.Second
)

// FormatConcepts renders concepts as a titled list: one line per concept
// as "#id [kind · topic] title", with the first bodies concepts carrying
// their body indented beneath.
func FormatConcepts(concepts []Concept, heading, note string, bodies int) string {
	if len(concepts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", heading)
	if note != "" {
		b.WriteString(note)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	for i, c := range concepts {
		b.WriteString(ConceptLine(c))
		b.WriteByte('\n')
		if i < bodies && strings.TrimSpace(c.Body) != "" && c.Body != c.Title {
			for _, line := range strings.Split(strings.TrimSpace(c.Body), "\n") {
				b.WriteString("  ")
				b.WriteString(line)
				b.WriteByte('\n')
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ConceptLine is the one-line index entry for a concept.
func ConceptLine(c Concept) string {
	tag := c.Kind
	if c.Topic != "" {
		tag += " · " + c.Topic
	}
	if c.Scope == ScopeGlobal {
		tag += " · global"
	}
	return fmt.Sprintf("- #%d [%s] %s", c.ID, tag, c.Title)
}

const memoryNote = "Long-term memory (#id [kind · topic] title). Call load_memory with an id for a full body, memory_reinforce when one proved useful, memory_demote when one misled."

// SystemBlock returns the session-start block: the top-weighted concepts
// for cwd's project and global scopes.
func SystemBlock(ctx context.Context, cwd string) string {
	svc := Default()
	if !svc.IsOpen() {
		return ""
	}
	recallCtx, cancel := context.WithTimeout(ctx, DefaultHookTimeout)
	defer cancel()
	concepts, err := svc.Top(recallCtx, cwd, DefaultTopK)
	if err != nil {
		return ""
	}
	return FormatConcepts(concepts, "Project memory", memoryNote, DefaultBodies)
}

// RecallBlock returns the per-turn block for prompt and the topic the
// hits imply. ids already in the system block are dropped so a turn does
// not repeat what the session opened with.
func RecallBlock(ctx context.Context, cwd, prompt, topic string, exclude map[int64]bool) (string, string) {
	svc := Default()
	if !svc.IsOpen() || strings.TrimSpace(prompt) == "" {
		return "", NormalizeTopic(topic)
	}
	recallCtx, cancel := context.WithTimeout(ctx, DefaultHookTimeout)
	defer cancel()
	res, err := svc.Recall(recallCtx, RecallQuery{Cwd: cwd, Query: prompt, Topic: topic, K: DefaultRecallK})
	if err != nil {
		return "", NormalizeTopic(topic)
	}
	kept := res.Concepts[:0]
	for _, c := range res.Concepts {
		if exclude[c.ID] {
			continue
		}
		kept = append(kept, c)
	}
	return FormatConcepts(kept, "Relevant memory", "", DefaultBodies), res.Topic
}

// TopIDs returns the ids SystemBlock would show, so RecallBlock can skip
// them.
func TopIDs(ctx context.Context, cwd string) map[int64]bool {
	svc := Default()
	if !svc.IsOpen() {
		return nil
	}
	recallCtx, cancel := context.WithTimeout(ctx, DefaultHookTimeout)
	defer cancel()
	concepts, err := svc.Top(recallCtx, cwd, DefaultTopK)
	if err != nil {
		return nil
	}
	ids := make(map[int64]bool, len(concepts))
	for _, c := range concepts {
		ids[c.ID] = true
	}
	return ids
}

// FileBlock returns titles recalled for a touched file path, without a
// weight bump: a path match is weak evidence of relevance.
func FileBlock(ctx context.Context, cwd, path string) string {
	svc := Default()
	if !svc.IsOpen() || strings.TrimSpace(path) == "" {
		return ""
	}
	recallCtx, cancel := context.WithTimeout(ctx, DefaultHookTimeout)
	defer cancel()
	res, err := svc.Recall(recallCtx, RecallQuery{Cwd: cwd, Query: path, K: 5, Silent: true})
	if err != nil {
		return ""
	}
	return FormatConcepts(res.Concepts, "Memory for "+path, "", 0)
}
