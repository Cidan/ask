package main

import (
	"strings"
	"testing"

	"github.com/Cidan/memmy"
)

func TestFormatRecallContext_EmptyHits(t *testing.T) {
	if got := formatRecallContext(nil, "Project memory"); got != "" {
		t.Errorf("empty hits should produce empty context, got %q", got)
	}
}

func TestFormatRecallContext_NumbersHits(t *testing.T) {
	hits := []memmy.RecallHit{
		{NodeID: "1", Text: "first observation"},
		{NodeID: "2", Text: "second observation"},
	}
	got := formatRecallContext(hits, "Project memory")
	if !strings.HasPrefix(got, "## Project memory") {
		t.Errorf("expected heading, got %q", got)
	}
	if !strings.Contains(got, "1. first observation") {
		t.Errorf("expected numbered first hit, got %q", got)
	}
	if !strings.Contains(got, "2. second observation") {
		t.Errorf("expected numbered second hit, got %q", got)
	}
}

func TestFormatRecallContext_FallsBackToSourceText(t *testing.T) {
	// When a chunk-level hit's Text is empty, the renderer falls
	// back to the parent SourceText so we always emit something for
	// the user to see.
	hits := []memmy.RecallHit{{NodeID: "1", Text: "", SourceText: "from-source"}}
	got := formatRecallContext(hits, "Memory")
	if !strings.Contains(got, "from-source") {
		t.Errorf("expected source-text fallback, got %q", got)
	}
}
