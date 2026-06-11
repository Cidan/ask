package main

import (
	"fmt"
	"strings"
)

// parseUnifiedDiff turns a diff like:
//
//	@@ -1,3 +1,4 @@
//	 context
//	-old
//	+new
//	+added
//
// into one diffHunk per `@@` header. Lines whose first character is
// neither space, +, -, nor \ are kept verbatim (no prefix re-writing)
// so renderDiffBlock's existing prefix-based styling applies. Lines
// before the first `@@` (file headers like "--- a/f") are dropped —
// we already know the path from the FileUpdateChange record.
func parseUnifiedDiff(s string) []diffHunk {
	var hunks []diffHunk
	var cur *diffHunk
	flush := func() {
		if cur != nil && len(cur.lines) > 0 {
			hunks = append(hunks, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "@@") {
			flush()
			h := parseUnifiedHunkHeader(line)
			cur = &h
			continue
		}
		if cur == nil {
			continue
		}
		cur.lines = append(cur.lines, line)
	}
	flush()
	return hunks
}

// parseUnifiedHunkHeader extracts oldStart/oldLines/newStart/newLines
// from a line like "@@ -12,7 +12,8 @@ optional context". The line
// count defaults to 1 when the comma is omitted (standard POSIX diff
// convention). Malformed headers collapse to a zero-valued hunk —
// renderDiffBlock handles that gracefully.
func parseUnifiedHunkHeader(line string) diffHunk {
	var h diffHunk
	if _, err := fmt.Sscanf(line, "@@ -%d,%d +%d,%d @@",
		&h.oldStart, &h.oldLines, &h.newStart, &h.newLines); err == nil {
		return h
	}
	// Handle the abbreviated forms where one side omits the line
	// count. Try the combinations in order from most to least likely.
	h = diffHunk{}
	if _, err := fmt.Sscanf(line, "@@ -%d +%d,%d @@",
		&h.oldStart, &h.newStart, &h.newLines); err == nil {
		h.oldLines = 1
		return h
	}
	h = diffHunk{}
	if _, err := fmt.Sscanf(line, "@@ -%d,%d +%d @@",
		&h.oldStart, &h.oldLines, &h.newStart); err == nil {
		h.newLines = 1
		return h
	}
	h = diffHunk{}
	if _, err := fmt.Sscanf(line, "@@ -%d +%d @@", &h.oldStart, &h.newStart); err == nil {
		h.oldLines, h.newLines = 1, 1
	}
	return h
}
