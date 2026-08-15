package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestDoc(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractContextLinks(t *testing.T) {
	// Basic extraction.
	links := ExtractContextLinks("See @docs/guide.md and @refs/api.md for details.")
	if len(links) != 2 || links[0] != "docs/guide.md" || links[1] != "refs/api.md" {
		t.Errorf("basic: %v", links)
	}

	// No links.
	if got := ExtractContextLinks("plain text"); len(got) != 0 {
		t.Errorf("no links: %v", got)
	}

	// Inside a fenced code block — stripped.
	body := "Before\n```\n@code/sample.md\n```\nAfter @real/doc.md"
	links = ExtractContextLinks(body)
	if len(links) != 1 || links[0] != "real/doc.md" {
		t.Errorf("code block strip: %v", links)
	}

	// Tilde fences.
	body = "Before\n~~~\n@tilde/sample.md\n~~~\nAfter @real/doc.md"
	links = ExtractContextLinks(body)
	if len(links) != 1 || links[0] != "real/doc.md" {
		t.Errorf("tilde fence strip: %v", links)
	}

	// Fence with info string.
	body = "```go\n@go/code.md\n```\n@ok/doc.md"
	links = ExtractContextLinks(body)
	if len(links) != 1 || links[0] != "ok/doc.md" {
		t.Errorf("info string fence: %v", links)
	}

	// Unterminated fence.
	body = "Before\n```\n@unterminated/doc.md"
	links = ExtractContextLinks(body)
	if len(links) != 0 {
		t.Errorf("unterminated fence: %v", links)
	}

	// Multiple links in same body, source order.
	body = "@second/doc.md before @first/aaa.md"
	links = ExtractContextLinks(body)
	if len(links) != 2 || links[0] != "second/doc.md" || links[1] != "first/aaa.md" {
		t.Errorf("source order: %v", links)
	}

	// Link with hyphens and dots in segment.
	links = ExtractContextLinks("See @a-b/c.d.md")
	if len(links) != 1 || links[0] != "a-b/c.d.md" {
		t.Errorf("hyphen and dot: %v", links)
	}

	// Case-insensitive .md extension.
	links = ExtractContextLinks("See @Spec.MD and @docs/Guide.Md")
	if len(links) != 2 || links[0] != "Spec.MD" || links[1] != "docs/Guide.Md" {
		t.Errorf("case-insensitive .md: %v", links)
	}
}

func TestResolveContextLink(t *testing.T) {
	root := t.TempDir()
	writeTestDoc(t, root, "docs/guide.md", "# Guide\n")
	writeTestDoc(t, root, "README.md", "# README\n")
	writeTestDoc(t, root, "src/main.go", "package main\n")
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Valid link.
	abs, ok := ResolveContextLink(root, "docs/guide.md")
	if !ok || abs != filepath.Join(root, "docs/guide.md") {
		t.Errorf("valid link: %q %v", abs, ok)
	}

	// Valid link in root.
	abs, ok = ResolveContextLink(root, "README.md")
	if !ok {
		t.Errorf("root file: %v", ok)
	}

	// Empty link.
	if _, ok := ResolveContextLink(root, ""); ok {
		t.Error("empty link must be rejected")
	}

	// Leading slash.
	if _, ok := ResolveContextLink(root, "/etc/passwd"); ok {
		t.Error("leading slash must be rejected")
	}

	// Leading ./
	if _, ok := ResolveContextLink(root, "./docs/guide.md"); ok {
		t.Error("leading ./ must be rejected")
	}

	// Leading ../
	if _, ok := ResolveContextLink(root, "../outside.md"); ok {
		t.Error("leading ../ must be rejected")
	}

	// .. segment mid-path.
	if _, ok := ResolveContextLink(root, "docs/../escape.md"); ok {
		t.Error(".. mid-path must be rejected")
	}

	// Non-.md file.
	if _, ok := ResolveContextLink(root, "src/main.go"); ok {
		t.Error("non-.md must be rejected")
	}

	// Missing file.
	if _, ok := ResolveContextLink(root, "docs/missing.md"); ok {
		t.Error("missing file must be rejected")
	}

	// Directory.
	if _, ok := ResolveContextLink(root, "empty-dir"); ok {
		t.Error("directory must be rejected")
	}

	// Case-insensitive .md extension.
	writeTestDoc(t, root, "Spec.MD", "# Spec\n")
	abs, ok = ResolveContextLink(root, "Spec.MD")
	if !ok || filepath.Base(abs) != "Spec.MD" {
		t.Errorf("case-insensitive .md: %q %v", abs, ok)
	}

	if _, ok := ResolveContextLink("/nonexistent", "file.md"); ok {
		t.Error("outside root (Rel) must be rejected")
	}
}

func TestLoadContextLinks_BFS_Dedup_Cycle(t *testing.T) {
	root := t.TempDir()

	writeTestDoc(t, root, "a.md", "See @b.md and @c.md\n")
	writeTestDoc(t, root, "b.md", "See @c.md\n")
	writeTestDoc(t, root, "c.md", "Leaf content.\n")

	docs := LoadContextLinks(root, []string{"See @a.md\n"})
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
	writeTestDoc(t, root, "x.md", "See @y.md\n")
	writeTestDoc(t, root, "y.md", "See @x.md\n")

	docs := LoadContextLinks(root, []string{"See @x.md\n"})
	if len(docs) != 2 {
		t.Errorf("cycle: want 2, got %d", len(docs))
	}
}

func TestLoadContextLinks_EmptyRepoRoot(t *testing.T) {
	if docs := LoadContextLinks("", []string{"@docs/x.md"}); docs != nil {
		t.Error("empty repoRoot must return nil")
	}
}

func TestLoadContextLinks_EmptyFileSkip(t *testing.T) {
	root := t.TempDir()
	writeTestDoc(t, root, "empty.md", "   \n")
	writeTestDoc(t, root, "real.md", "content\n")

	docs := LoadContextLinks(root, []string{"See @empty.md and @real.md\n"})
	if len(docs) != 1 || filepath.Base(docs[0].Path) != "real.md" {
		t.Errorf("empty file must be skipped: %v", docs)
	}
}

func TestLoadContextLinks_Cap(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", AgentContextFileCap+100)
	writeTestDoc(t, root, "big.md", big)

	docs := LoadContextLinks(root, []string{"@big.md"})
	if len(docs) != 1 {
		t.Fatalf("want 1 doc, got %d", len(docs))
	}
	if !strings.Contains(docs[0].Body, "… (truncated)") {
		t.Error("oversized doc must be truncated")
	}
	if len(docs[0].Body) > AgentContextFileCap+len("\n… (truncated)") {
		t.Errorf("truncated body too long: %d", len(docs[0].Body))
	}
}

func TestContextLinksPromptBlock(t *testing.T) {
	if got := ContextLinksPromptBlock(nil); got != "" {
		t.Errorf("nil docs: %q", got)
	}
	if got := ContextLinksPromptBlock([]LoadedContextDoc{}); got != "" {
		t.Errorf("empty docs: %q", got)
	}

	docs := []LoadedContextDoc{
		{Path: "/root/b.md", Body: "body B"},
		{Path: "/root/a.md", Body: "body A"},
	}
	block := ContextLinksPromptBlock(docs)
	if !strings.Contains(block, "<included_docs>") {
		t.Errorf("block missing tag: %q", block)
	}
	if !strings.Contains(block, "body A") || !strings.Contains(block, "body B") {
		t.Errorf("bodies missing: %q", block)
	}
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
	writeTestDoc(t, root, "plain.md", "No links here.\n")
	docs := LoadContextLinks(root, []string{"No links here.\n"})
	if len(docs) != 0 {
		t.Errorf("no links in source: %v", docs)
	}
}

func TestRuleLinkedDocs(t *testing.T) {
	root := t.TempDir()
	writeTestDoc(t, root, "ref.md", "Reference content.\n")

	docs := RuleLinkedDocs(root, "See @ref.md\n")
	if len(docs) != 1 || !strings.Contains(docs[0].Body, "Reference content.") {
		t.Errorf("ruleLinkedDocs: %v", docs)
	}
}

func TestLoadContextLinks_Transitive(t *testing.T) {
	root := t.TempDir()
	writeTestDoc(t, root, "level1.md", "See @level2.md\n")
	writeTestDoc(t, root, "level2.md", "See @level3.md\n")
	writeTestDoc(t, root, "level3.md", "Deep content.\n")

	docs := LoadContextLinks(root, []string{"See @level1.md\n"})
	if len(docs) != 3 {
		t.Errorf("transitive: want 3, got %d: %v", len(docs), docs)
	}
}

func TestStripFencedCodeBlocks(t *testing.T) {
	in := "hello\nworld"
	out := StripFencedCodeBlocks(in)
	if out != in {
		t.Errorf("no fence: %q", out)
	}

	in = "before\n```\ncode\n```\nafter"
	out = StripFencedCodeBlocks(in)
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("simple fence: %q", out)
	}
	if strings.Contains(out, "code") {
		t.Errorf("code must be stripped: %q", out)
	}

	in = "before\n~~~\ncode\n~~~\nafter"
	out = StripFencedCodeBlocks(in)
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("tilde fence: %q", out)
	}
	if strings.Contains(out, "code") {
		t.Errorf("code must be stripped: %q", out)
	}

	in = "```go\ncode\n```\nok"
	out = StripFencedCodeBlocks(in)
	if !strings.Contains(out, "ok") || strings.Contains(out, "code") {
		t.Errorf("info string: %q", out)
	}

	in = "before\n```\ncode"
	out = StripFencedCodeBlocks(in)
	if strings.Contains(out, "code") || !strings.Contains(out, "before") {
		t.Errorf("unterminated: %q", out)
	}

	in = "line1\n```\nline3\nline4\nline5\n```\nline7"
	out = StripFencedCodeBlocks(in)
	if strings.Count(out, "\n") != strings.Count(in, "\n") {
		t.Errorf("newline count mismatch: %d vs %d in %q",
			strings.Count(out, "\n"), strings.Count(in, "\n"), out)
	}
}
