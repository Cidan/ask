package main

import (
	"strings"
	"testing"
)

func TestParseUnifiedDiff_BasicHeader(t *testing.T) {
	diff := `@@ -1,3 +1,4 @@
 ctx
-old
+new
+added
`
	hunks := parseUnifiedDiff(diff)
	if len(hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.oldStart != 1 || h.oldLines != 3 || h.newStart != 1 || h.newLines != 4 {
		t.Errorf("hunk header parsed wrong: %+v", h)
	}
	if len(h.lines) != 5 { // "ctx", "-old", "+new", "+added", ""
		t.Errorf("want 5 lines (inc trailing empty), got %d: %v", len(h.lines), h.lines)
	}
}

func TestParseUnifiedDiff_MultipleHunks(t *testing.T) {
	diff := `@@ -1,2 +1,2 @@
 a
-b
+c
@@ -10,1 +10,1 @@
-x
+y
`
	hunks := parseUnifiedDiff(diff)
	if len(hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d: %+v", len(hunks), hunks)
	}
	if hunks[0].oldStart != 1 || hunks[1].oldStart != 10 {
		t.Errorf("second hunk starts at wrong offset: %+v", hunks)
	}
}

func TestParseUnifiedDiff_AbbreviatedHeader(t *testing.T) {
	// POSIX diff omits ",count" when count==1.
	diff := `@@ -42 +99 @@
-single
+replacement
`
	hunks := parseUnifiedDiff(diff)
	if len(hunks) != 1 {
		t.Fatalf("abbreviated header should still parse: %v", hunks)
	}
	h := hunks[0]
	if h.oldStart != 42 || h.newStart != 99 || h.oldLines != 1 || h.newLines != 1 {
		t.Errorf("abbreviated hunk parsed wrong: %+v", h)
	}
}

func TestParseUnifiedDiff_SkipsPreHeaderLines(t *testing.T) {
	diff := `--- a/f
+++ b/f
@@ -1 +1 @@
-a
+b
`
	hunks := parseUnifiedDiff(diff)
	if len(hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(hunks))
	}
	for _, ln := range hunks[0].lines {
		if strings.HasPrefix(ln, "---") || strings.HasPrefix(ln, "+++") {
			t.Errorf("file-header lines leaked into hunk: %q", ln)
		}
	}
}

func TestParseUnifiedDiff_EmptyIsNoOp(t *testing.T) {
	if h := parseUnifiedDiff(""); len(h) != 0 {
		t.Errorf("empty diff should produce no hunks, got %v", h)
	}
	if h := parseUnifiedDiff("just text without markers"); len(h) != 0 {
		t.Errorf("diff without @@ markers should produce no hunks, got %v", h)
	}
}
