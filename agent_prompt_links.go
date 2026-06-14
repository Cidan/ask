package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// contextLinkRe matches @path/to/file.md references in markdown bodies.
// The capture excludes the @ prefix. Each path segment must start with
// an alphanumeric character; segments contain only alphanumeric,
// underscore, dot, and hyphen. The pattern ends in .md.
var contextLinkRe = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9_.-]*(?:/[A-Za-z0-9][A-Za-z0-9_.-]*)*(?i:\.md))`)

// loadedContextDoc is one document that has been loaded for inclusion in
// the system prompt — either a project instruction file or an @-linked
// document resolved during prompt assembly.
type loadedContextDoc struct {
	Path string
	Body string
}

// extractContextLinks finds all @path/to/file.md references in body.
// Fenced code blocks (``` and ~~~, with optional info strings) are
// stripped first — replaced with spaces while preserving newlines — so
// @-references inside code samples are not treated as links.
// Unterminated fences consume to EOF.
func extractContextLinks(body string) []string {
	clean := stripFencedCodeBlocks(body)
	matches := contextLinkRe.FindAllStringSubmatch(clean, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// stripFencedCodeBlocks replaces fenced code block content with spaces,
// preserving newlines. Handles both ``` and ~~~ fences with optional
// info strings. Unterminated fences consume the remainder of the input.
func stripFencedCodeBlocks(s string) string {
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

// resolveContextLink resolves a link (without the @) against repoRoot.
// Returns the cleaned absolute path and true when the link is valid,
// safe, and points to an existing .md file inside repoRoot.
// Rejects: empty, leading /, leading ./, leading ../, .. segment
// anywhere, paths that escape repoRoot, missing files, directories,
// and non-.md files.
func resolveContextLink(repoRoot, link string) (string, bool) {
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

// loadContextLinks walks the @-link graph breadth-first: it starts from
// the links in sourceBodies, resolves each against repoRoot, loads the
// file, and repeats for links found in the loaded files. Deduplication
// by absolute path prevents cycles and double-loads. Files that are
// empty, whitespace-only, or exceed agentContextFileCap are handled
// identically to agentContextFiles (cap truncation, silent skip for
// empty). Returns nil when repoRoot is empty.
func loadContextLinks(repoRoot string, sourceBodies []string) []loadedContextDoc {
	if repoRoot == "" {
		return nil
	}
	seen := map[string]bool{}
	queue := make([]string, 0)
	for _, body := range sourceBodies {
		for _, link := range extractContextLinks(body) {
			if abs, ok := resolveContextLink(repoRoot, link); ok && !seen[abs] {
				seen[abs] = true
				queue = append(queue, abs)
			}
		}
	}
	var docs []loadedContextDoc
	for i := 0; i < len(queue); i++ {
		abs := queue[i]
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		body := string(data)
		if strings.TrimSpace(body) == "" {
			continue
		}
		if len(body) > agentContextFileCap {
			body = body[:agentContextFileCap] + "\n… (truncated)"
		}
		body = strings.TrimRight(body, "\n")
		docs = append(docs, loadedContextDoc{Path: abs, Body: body})
		for _, link := range extractContextLinks(body) {
			if abs2, ok := resolveContextLink(repoRoot, link); ok && !seen[abs2] {
				seen[abs2] = true
				queue = append(queue, abs2)
			}
		}
	}
	return docs
}

// contextLinksPromptBlock renders the loaded @-linked docs into a
// single <included_docs> system-prompt block, sorted by path for
// deterministic output. Returns "" when docs is empty so the caller
// can skip the block entirely.
func contextLinksPromptBlock(docs []loadedContextDoc) string {
	if len(docs) == 0 {
		return ""
	}
	sorted := make([]loadedContextDoc, len(docs))
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

// ruleLinkedDocs resolves @-links found in a rule body (JIT rules,
// skills, subagents) and returns the loaded documents. A thin wrapper
// around loadContextLinks for the lazy-loading surfaces.
func ruleLinkedDocs(repoRoot, body string) []loadedContextDoc {
	return loadContextLinks(repoRoot, []string{body})
}
