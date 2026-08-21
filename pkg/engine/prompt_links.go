package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// contextLinkRe matches @path/to/file.md references in markdown bodies.
var contextLinkRe = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9_.-]*(?:/[A-Za-z0-9][A-Za-z0-9_.-]*)*(?i:\.md))`)

// LoadedContextDoc is one document that has been loaded for inclusion in
// the system prompt — either a project instruction file or an @-linked
// document resolved during prompt assembly.
type LoadedContextDoc struct {
	Path string
	// Body is what goes into the prompt, capped by
	// TruncateInstructionDoc.
	Body string
	// Links are the @-references found in the document's FULL body,
	// before any truncation. Body alone is not a safe source for them:
	// a link past the cap is still a real dependency of the
	// instructions and must still be followed.
	Links []string
}

// ExtractContextLinks finds all @path/to/file.md references in body.
func ExtractContextLinks(body string) []string {
	clean := StripFencedCodeBlocks(body)
	matches := contextLinkRe.FindAllStringSubmatch(clean, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// StripFencedCodeBlocks replaces fenced code block content with spaces,
// preserving newlines. Handles both ``` and ~~~ fences with optional
// info strings. Unterminated fences consume the remainder of the input.
func StripFencedCodeBlocks(s string) string {
	lines := strings.Split(s, "\n")
	var b strings.Builder
	b.Grow(len(s))
	inFence := false
	var fenceChar byte
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if !inFence {
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				inFence = true
				if strings.HasPrefix(trimmed, "```") {
					fenceChar = '`'
				} else {
					fenceChar = '~'
				}
				b.WriteString(strings.Repeat(" ", len(line)))
			} else {
				b.WriteString(line)
			}
		} else {
			stripped := strings.TrimSpace(trimmed)
			prefix := strings.Repeat(string(fenceChar), 3)
			if stripped == prefix ||
				(strings.HasPrefix(stripped, prefix) && strings.TrimSpace(strings.TrimPrefix(stripped, prefix)) == "") {
				inFence = false
				b.WriteString(strings.Repeat(" ", len(line)))
			} else {
				b.WriteString(strings.Repeat(" ", len(line)))
			}
		}
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ResolveContextLink resolves a link (without the @) against repoRoot.
func ResolveContextLink(repoRoot, link string) (string, bool) {
	if link == "" {
		return "", false
	}
	if strings.HasPrefix(link, "/") || strings.HasPrefix(link, "./") || strings.HasPrefix(link, "../") {
		return "", false
	}
	if !strings.EqualFold(filepath.Ext(link), ".md") {
		return "", false
	}
	for _, seg := range strings.Split(link, "/") {
		if seg == ".." || seg == "." {
			return "", false
		}
	}
	abs := filepath.Join(repoRoot, filepath.FromSlash(link))
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", false
	}
	return abs, true
}

// LoadContextLinks walks the @-link graph breadth-first starting from
// the links in sourceBodies. Callers that already hold a document's
// untruncated link list should prefer LoadContextLinksFrom — passing a
// capped Body here would lose every link past the cap.
func LoadContextLinks(repoRoot string, sourceBodies []string) []LoadedContextDoc {
	var links []string
	for _, body := range sourceBodies {
		links = append(links, ExtractContextLinks(body)...)
	}
	return LoadContextLinksFrom(repoRoot, links)
}

// LoadContextLinksFrom walks the @-link graph breadth-first from an
// explicit seed list: it resolves each link against repoRoot, loads the
// file, and repeats for the links found inside it. Each loaded file is
// scanned for further links BEFORE it is truncated, so a deep @-chain
// survives a large intermediate document.
func LoadContextLinksFrom(repoRoot string, links []string) []LoadedContextDoc {
	if repoRoot == "" {
		return nil
	}
	seen := map[string]bool{}
	queue := make([]string, 0, len(links))
	enqueue := func(link string) {
		if abs, ok := ResolveContextLink(repoRoot, link); ok && !seen[abs] {
			seen[abs] = true
			queue = append(queue, abs)
		}
	}
	for _, link := range links {
		enqueue(link)
	}
	var docs []LoadedContextDoc
	for i := 0; i < len(queue); i++ {
		abs := queue[i]
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		full := string(data)
		if strings.TrimSpace(full) == "" {
			continue
		}
		nested := ExtractContextLinks(full)
		body := strings.TrimRight(TruncateInstructionDoc(abs, full, AgentContextFileCap), "\n")
		docs = append(docs, LoadedContextDoc{Path: abs, Body: body, Links: nested})
		for _, link := range nested {
			enqueue(link)
		}
	}
	return docs
}

// ContextLinksPromptBlock renders the loaded @-linked docs into a
// single <included_docs> system-prompt block, sorted by path for
// deterministic output.
func ContextLinksPromptBlock(docs []LoadedContextDoc) string {
	if len(docs) == 0 {
		return ""
	}
	sorted := make([]LoadedContextDoc, len(docs))
	copy(sorted, docs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var b strings.Builder
	b.WriteString("<included_docs>\nThe following markdown files are @-linked from project instructions, rules, skills, or subagents. They are part of this session's direct context.\n")
	for _, d := range sorted {
		fmt.Fprintf(&b, "<file path=%q>\n%s\n</file>\n", d.Path, d.Body)
	}
	b.WriteString("</included_docs>")
	return b.String()
}

// RuleLinkedDocs resolves @-links found in a rule body (JIT rules,
// skills, subagents) and returns the loaded documents.
func RuleLinkedDocs(repoRoot, body string) []LoadedContextDoc {
	return LoadContextLinks(repoRoot, []string{body})
}
