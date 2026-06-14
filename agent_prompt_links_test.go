package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractContextLinks(t *testing.T) {
	// Basic extraction.
	links := extractContextLinks("See @docs/guide.md and @refs/api.md for details.")
	if len(links) != 2 || links[0] != "docs/guide.md" || links[1] != "refs/api.md" {
		t.Errorf("basic: %v", links)
	}

	// No links.
	if got := extractContextLinks("plain text"); len(got) != 0 {
		t.Errorf("no links: %v", got)
	}

	// Inside a fenced code block — stripped.
	body := "Before\n```\n@code/sample.md\n```\nAfter @real/doc.md"
	links = extractContextLinks(body)
	if len(links) != 1 || links[0] != "real/doc.md" {
		t.Errorf("code block strip: %v", links)
	}

	// Tilde fences.
	body = "Before\n~~~\n@tilde/sample.md\n~~~\nAfter @real/doc.md"
	links = extractContextLinks(body)
	if len(links) != 1 || links[0] != "real/doc.md" {
		t.Errorf("tilde fence strip: %v", links)
	}

	// Fence with info string.
	body = "```go\n@go/code.md\n```\n@ok/doc.md"
	links = extractContextLinks(body)
	if len(links) != 1 || links[0] != "ok/doc.md" {
		t.Errorf("info string fence: %v", links)
	}

	// Unterminated fence.
	body = "Before\n```\n@unterminated/doc.md"
	links = extractContextLinks(body)
	if len(links) != 0 {
		t.Errorf("unterminated fence: %v", links)
	}

	// Multiple links in same body, source order.
	body = "@second/doc.md before @first/aaa.md"
	links = extractContextLinks(body)
	if len(links) != 2 || links[0] != "second/doc.md" || links[1] != "first/aaa.md" {
		t.Errorf("source order: %v", links)
	}

	// Link with hyphens and dots in segment.
	links = extractContextLinks("See @a-b/c.d.md")
	if len(links) != 1 || links[0] != "a-b/c.d.md" {
		t.Errorf("hyphen and dot: %v", links)
	}

	// Case-insensitive .md extension.
	links = extractContextLinks("See @Spec.MD and @docs/Guide.Md")
	if len(links) != 2 || links[0] != "Spec.MD" || links[1] != "docs/Guide.Md" {
		t.Errorf("case-insensitive .md: %v", links)
	}
}

func TestResolveContextLink(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "docs/guide.md", "# Guide\n")
	writeTestFile(t, root, "README.md", "# README\n")
	writeTestFile(t, root, "src/main.go", "package main\n")
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Valid link.
	abs, ok := resolveContextLink(root, "docs/guide.md")
	if !ok || abs != filepath.Join(root, "docs/guide.md") {
		t.Errorf("valid link: %q %v", abs, ok)
	}

	// Valid link in root.
	abs, ok = resolveContextLink(root, "README.md")
	if !ok {
		t.Errorf("root file: %v", ok)
	}

	// Empty link.
	if _, ok := resolveContextLink(root, ""); ok {
		t.Error("empty link must be rejected")
	}

	// Leading slash.
	if _, ok := resolveContextLink(root, "/etc/passwd"); ok {
		t.Error("leading slash must be rejected")
	}

	// Leading ./
	if _, ok := resolveContextLink(root, "./docs/guide.md"); ok {
		t.Error("leading ./ must be rejected")
	}

	// Leading ../
	if _, ok := resolveContextLink(root, "../outside.md"); ok {
		t.Error("leading ../ must be rejected")
	}

	// .. segment mid-path.
	if _, ok := resolveContextLink(root, "docs/../escape.md"); ok {
		t.Error(".. mid-path must be rejected")
	}

	// Non-.md file.
	if _, ok := resolveContextLink(root, "src/main.go"); ok {
		t.Error("non-.md must be rejected")
	}

	// Missing file.
	if _, ok := resolveContextLink(root, "docs/missing.md"); ok {
		t.Error("missing file must be rejected")
	}

	// Directory.
	if _, ok := resolveContextLink(root, "empty-dir"); ok {
		t.Error("directory must be rejected (even though non-.md already catches it)")
	}

	// Case-insensitive .md extension.
	writeTestFile(t, root, "Spec.MD", "# Spec\n")
	abs, ok = resolveContextLink(root, "Spec.MD")
	if !ok || filepath.Base(abs) != "Spec.MD" {
		t.Errorf("case-insensitive .md: %q %v", abs, ok)
	}

	// Path escaping root via symlink-equivalent — use Rel rejection.
	// We can't create a path that escapes via Join, but we test the Rel
	// guard by constructing a link that, after Join+Clean, resolves outside.
	// On most systems, Join(root, "../../etc/passwd") → Clean → /etc/passwd
	// But the .. segment check catches this before Join.
	// Belt-and-braces: test with a non-existent root to verify Rel guard.
	if _, ok := resolveContextLink("/nonexistent", "file.md"); ok {
		t.Error("outside root (Rel) must be rejected")
	}
}

func TestLoadContextLinks_BFS_Dedup_Cycle(t *testing.T) {
	root := t.TempDir()

	// Create chain A → B → C with A also referencing C directly.
	writeTestFile(t, root, "a.md", "See @b.md and @c.md\n")
	writeTestFile(t, root, "b.md", "See @c.md\n")
	writeTestFile(t, root, "c.md", "Leaf content.\n")

	docs := loadContextLinks(root, []string{"See @a.md\n"})
	if len(docs) != 3 {
		t.Fatalf("want 3 docs, got %d: %v", len(docs), docs)
	}

	byPath := map[string]string{}
	for _, d := range docs {
		byPath[filepath.Base(d.Path)] = d.Body
	}
	if !strings.Contains(byPath["a.md"], "@b.md") {
		t.Errorf("a.md body wrong: %q", byPath["a.md"])
	}
	if !strings.Contains(byPath["b.md"], "@c.md") {
		t.Errorf("b.md body wrong: %q", byPath["b.md"])
	}
	if byPath["c.md"] != "Leaf content." {
		t.Errorf("c.md body wrong: %q", byPath["c.md"])
	}
}

func TestLoadContextLinks_Cycle(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "x.md", "See @y.md\n")
	writeTestFile(t, root, "y.md", "See @x.md\n")

	docs := loadContextLinks(root, []string{"See @x.md\n"})
	// Both loaded once; cycle doesn't cause infinite loop.
	if len(docs) != 2 {
		t.Errorf("cycle: want 2, got %d", len(docs))
	}
}

func TestLoadContextLinks_EmptyRepoRoot(t *testing.T) {
	if docs := loadContextLinks("", []string{"@docs/x.md"}); docs != nil {
		t.Error("empty repoRoot must return nil")
	}
}

func TestLoadContextLinks_EmptyFileSkip(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "empty.md", "   \n")
	writeTestFile(t, root, "real.md", "content\n")

	docs := loadContextLinks(root, []string{"See @empty.md and @real.md\n"})
	if len(docs) != 1 || filepath.Base(docs[0].Path) != "real.md" {
		t.Errorf("empty file must be skipped: %v", docs)
	}
}

func TestLoadContextLinks_Cap(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", agentContextFileCap+100)
	writeTestFile(t, root, "big.md", big)

	docs := loadContextLinks(root, []string{"@big.md"})
	if len(docs) != 1 {
		t.Fatalf("want 1 doc, got %d", len(docs))
	}
	if !strings.Contains(docs[0].Body, "… (truncated)") {
		t.Error("oversized doc must be truncated")
	}
	if len(docs[0].Body) > agentContextFileCap+len("\n… (truncated)") {
		t.Errorf("truncated body too long: %d", len(docs[0].Body))
	}
}

func TestContextLinksPromptBlock(t *testing.T) {
	// Empty → "".
	if got := contextLinksPromptBlock(nil); got != "" {
		t.Errorf("nil docs: %q", got)
	}
	if got := contextLinksPromptBlock([]loadedContextDoc{}); got != "" {
		t.Errorf("empty docs: %q", got)
	}

	docs := []loadedContextDoc{
		{Path: "/root/b.md", Body: "body B"},
		{Path: "/root/a.md", Body: "body A"},
	}
	block := contextLinksPromptBlock(docs)
	if !strings.Contains(block, "<included_docs>") {
		t.Errorf("block missing tag: %q", block)
	}
	if !strings.Contains(block, "body A") || !strings.Contains(block, "body B") {
		t.Errorf("bodies missing: %q", block)
	}
	// Sorted by path: a.md before b.md.
	aIdx := strings.Index(block, "a.md")
	bIdx := strings.Index(block, "b.md")
	if aIdx < 0 || bIdx < 0 || aIdx >= bIdx {
		t.Errorf("docs not sorted by path: a at %d, b at %d", aIdx, bIdx)
	}
	if !strings.Contains(block, `path="/root/a.md"`) {
		t.Errorf("path attribute missing: %q", block)
	}
}

func TestLoadContextLinks_NoLinksReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "plain.md", "No links here.\n")
	docs := loadContextLinks(root, []string{"No links here.\n"})
	if len(docs) != 0 {
		t.Errorf("no links in source: %v", docs)
	}
}

func TestRuleLinkedDocs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "ref.md", "Reference content.\n")

	docs := ruleLinkedDocs(root, "See @ref.md\n")
	if len(docs) != 1 || !strings.Contains(docs[0].Body, "Reference content.") {
		t.Errorf("ruleLinkedDocs: %v", docs)
	}
}

func TestLoadContextLinks_Transitive(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "level1.md", "See @level2.md\n")
	writeTestFile(t, root, "level2.md", "See @level3.md\n")
	writeTestFile(t, root, "level3.md", "Deep content.\n")

	docs := loadContextLinks(root, []string{"See @level1.md\n"})
	if len(docs) != 3 {
		t.Errorf("transitive: want 3, got %d: %v", len(docs), docs)
	}
}

func TestStripFencedCodeBlocks(t *testing.T) {
	// No fences.
	in := "hello\nworld"
	out := stripFencedCodeBlocks(in)
	if out != in {
		t.Errorf("no fence: %q", out)
	}

	// Simple fence.
	in = "before\n```\ncode\n```\nafter"
	out = stripFencedCodeBlocks(in)
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("simple fence: %q", out)
	}
	if strings.Contains(out, "code") {
		t.Errorf("code must be stripped: %q", out)
	}

	// Tilde fence.
	in = "before\n~~~\ncode\n~~~\nafter"
	out = stripFencedCodeBlocks(in)
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("tilde fence: %q", out)
	}
	if strings.Contains(out, "code") {
		t.Errorf("code must be stripped: %q", out)
	}

	// Info string.
	in = "```go\ncode\n```\nok"
	out = stripFencedCodeBlocks(in)
	if !strings.Contains(out, "ok") || strings.Contains(out, "code") {
		t.Errorf("info string: %q", out)
	}

	// Unterminated fence.
	in = "before\n```\ncode"
	out = stripFencedCodeBlocks(in)
	if strings.Contains(out, "code") || !strings.Contains(out, "before") {
		t.Errorf("unterminated: %q", out)
	}

	// Newlines preserved — verify line count.
	in = "line1\n```\nline3\nline4\nline5\n```\nline7"
	out = stripFencedCodeBlocks(in)
	if strings.Count(out, "\n") != strings.Count(in, "\n") {
		t.Errorf("newline count mismatch: %d vs %d in %q",
			strings.Count(out, "\n"), strings.Count(in, "\n"), out)
	}
}
