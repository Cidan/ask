package main

import (
	"fmt"
	"strings"
	"testing"
)

// applyUnifiedDiff reconstructs the new body's comparison keys (see
// diffKeys) from old + the generated diff, so tests can assert the
// patch is exact rather than eyeballing hunk text.
func applyUnifiedDiff(t *testing.T, oldBody, diff string) []string {
	t.Helper()
	oldLines, oldNoEOL := splitDiffLines(oldBody)
	oldKeys := diffKeys(oldLines, oldNoEOL)
	hunks := parseUnifiedDiff(diff)
	var out []string
	cursor := 0 // 0-based index into oldKeys
	for _, h := range hunks {
		start := h.oldStart - 1
		if h.oldLines == 0 {
			start = h.oldStart // zero-length side anchors above
		}
		if start < cursor || start > len(oldKeys) {
			t.Fatalf("hunk start %d out of order (cursor %d, old len %d)", start, cursor, len(oldKeys))
		}
		out = append(out, oldKeys[cursor:start]...)
		cursor = start
		for i := 0; i < len(h.lines); i++ {
			line := h.lines[i]
			marker := i+1 < len(h.lines) && strings.HasPrefix(h.lines[i+1], `\`)
			suffix := ""
			if marker {
				suffix = "\x00noeol"
			}
			switch {
			case strings.HasPrefix(line, " "):
				if got := oldKeys[cursor]; got != line[1:]+suffix {
					t.Fatalf("context line mismatch at old line %d: diff has %q, file has %q", cursor+1, line[1:], got)
				}
				out = append(out, line[1:]+suffix)
				cursor++
			case strings.HasPrefix(line, "-"):
				if got := oldKeys[cursor]; got != line[1:]+suffix {
					t.Fatalf("deleted line mismatch at old line %d: diff has %q, file has %q", cursor+1, line[1:], got)
				}
				cursor++
			case strings.HasPrefix(line, "+"):
				out = append(out, line[1:]+suffix)
			case strings.HasPrefix(line, `\`):
				// no-newline marker, consumed via lookahead
			default:
				t.Fatalf("unexpected hunk line %q", line)
			}
		}
	}
	out = append(out, oldKeys[cursor:]...)
	return out
}

func assertDiffApplies(t *testing.T, oldBody, newBody string) string {
	t.Helper()
	diff := unifiedDiff(oldBody, newBody)
	newLines, newNoEOL := splitDiffLines(newBody)
	wantKeys := diffKeys(newLines, newNoEOL)
	gotKeys := applyUnifiedDiff(t, oldBody, diff)
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("patched length %d want %d\ndiff:\n%s", len(gotKeys), len(wantKeys), diff)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("patched line %d = %q want %q\ndiff:\n%s", i+1, gotKeys[i], wantKeys[i], diff)
		}
	}
	return diff
}

func TestUnifiedDiff_IdenticalIsEmpty(t *testing.T) {
	body := "a\nb\nc\n"
	if d := unifiedDiff(body, body); d != "" {
		t.Errorf("identical bodies should produce empty diff, got:\n%s", d)
	}
}

func TestUnifiedDiff_SingleModification(t *testing.T) {
	oldBody := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\n"
	newBody := "l1\nl2\nl3\nl4\nCHANGED\nl6\nl7\nl8\nl9\n"
	diff := assertDiffApplies(t, oldBody, newBody)
	hunks := parseUnifiedDiff(diff)
	if len(hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d:\n%s", len(hunks), diff)
	}
	h := hunks[0]
	if h.oldStart != 2 || h.oldLines != 7 || h.newStart != 2 || h.newLines != 7 {
		t.Errorf("hunk header @@ -%d,%d +%d,%d @@ want @@ -2,7 +2,7 @@", h.oldStart, h.oldLines, h.newStart, h.newLines)
	}
	want := []string{" l2", " l3", " l4", "-l5", "+CHANGED", " l6", " l7", " l8"}
	if len(h.lines) != len(want) {
		t.Fatalf("hunk lines %v want %v", h.lines, want)
	}
	for i := range want {
		if h.lines[i] != want[i] {
			t.Errorf("hunk line %d = %q want %q", i, h.lines[i], want[i])
		}
	}
}

func TestUnifiedDiff_InsertAtTopAndDeleteAtEnd(t *testing.T) {
	oldBody := "a\nb\nc\nd\ne\n"
	newBody := "NEW\na\nb\nc\nd\n"
	diff := assertDiffApplies(t, oldBody, newBody)
	hunks := parseUnifiedDiff(diff)
	if len(hunks) != 1 {
		t.Fatalf("want merged single hunk, got %d:\n%s", len(hunks), diff)
	}
	if hunks[0].oldStart != 1 || hunks[0].newStart != 1 {
		t.Errorf("hunk should start at line 1: %+v", hunks[0])
	}
}

func TestUnifiedDiff_TwoFarChangesProduceTwoHunks(t *testing.T) {
	var oldLines, newLines []string
	for i := 1; i <= 30; i++ {
		oldLines = append(oldLines, fmt.Sprintf("line%02d", i))
		newLines = append(newLines, fmt.Sprintf("line%02d", i))
	}
	newLines[2] = "topchange"
	newLines[26] = "bottomchange"
	oldBody := strings.Join(oldLines, "\n") + "\n"
	newBody := strings.Join(newLines, "\n") + "\n"
	diff := assertDiffApplies(t, oldBody, newBody)
	hunks := parseUnifiedDiff(diff)
	if len(hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d:\n%s", len(hunks), diff)
	}
	// Trailing context of the final hunk must cap at 3 even though the
	// file continues past it.
	last := hunks[1]
	tail := 0
	for i := len(last.lines) - 1; i >= 0 && strings.HasPrefix(last.lines[i], " "); i-- {
		tail++
	}
	if tail != 3 {
		t.Errorf("final hunk trailing context = %d lines, want 3:\n%s", tail, diff)
	}
}

func TestUnifiedDiff_CloseChangesMergeIntoOneHunk(t *testing.T) {
	oldBody := "a\nb\nc\nd\ne\nf\ng\nh\n"
	newBody := "A\nb\nc\nd\ne\nf\ng\nH\n"
	diff := assertDiffApplies(t, oldBody, newBody)
	if hunks := parseUnifiedDiff(diff); len(hunks) != 1 {
		t.Fatalf("changes 6 apart should merge into 1 hunk, got %d:\n%s", len(hunks), diff)
	}
}

func TestUnifiedDiff_CreateAndDelete(t *testing.T) {
	created := assertDiffApplies(t, "", "one\ntwo\n")
	hunks := parseUnifiedDiff(created)
	if len(hunks) != 1 || hunks[0].oldStart != 0 || hunks[0].oldLines != 0 ||
		hunks[0].newStart != 1 || hunks[0].newLines != 2 {
		t.Errorf("create hunk header wrong: %+v\n%s", hunks, created)
	}

	deleted := assertDiffApplies(t, "one\ntwo\n", "")
	hunks = parseUnifiedDiff(deleted)
	if len(hunks) != 1 || hunks[0].oldStart != 1 || hunks[0].oldLines != 2 ||
		hunks[0].newStart != 0 || hunks[0].newLines != 0 {
		t.Errorf("delete hunk header wrong: %+v\n%s", hunks, deleted)
	}
}

func TestUnifiedDiff_NoTrailingNewline(t *testing.T) {
	// Adding a trailing newline must register as a change to the final
	// line, with the no-newline marker on the minus side only.
	diff := assertDiffApplies(t, "a\nb", "a\nb\n")
	if !strings.Contains(diff, `\ No newline at end of file`) {
		t.Errorf("expected no-newline marker:\n%s", diff)
	}
	if !strings.Contains(diff, "-b") || !strings.Contains(diff, "+b") {
		t.Errorf("expected -b/+b pair for newline-ness change:\n%s", diff)
	}

	// Both sides lacking the newline on an unchanged last line keeps it
	// as context (no spurious change).
	diff = assertDiffApplies(t, "x\ny\nz", "x\nCHANGED\nz")
	if strings.Contains(diff, "-z") || strings.Contains(diff, "+z") {
		t.Errorf("unchanged noeol last line must stay context:\n%s", diff)
	}
}

func TestUnifiedDiff_BudgetFallbackStillApplies(t *testing.T) {
	a := []string{"a1", "a2", "a3", "a4"}
	b := []string{"b1", "b2", "b3"}
	ops := myersOps(a, b, 0)
	want := []diffOp{{diffOpDelete, 4}, {diffOpInsert, 3}}
	if len(ops) != 2 || ops[0] != want[0] || ops[1] != want[1] {
		t.Errorf("budget-exhausted ops = %+v want %+v", ops, want)
	}

	// End-to-end the replacement is still a valid, applying diff.
	var oldSB, newSB strings.Builder
	for i := range 50 {
		fmt.Fprintf(&oldSB, "old-%02d\n", i)
		fmt.Fprintf(&newSB, "new-%02d\n", i)
	}
	assertDiffApplies(t, oldSB.String(), newSB.String())
}

func TestUnifiedDiff_InterleavedEditsApply(t *testing.T) {
	oldBody := "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 2\n}\n\nfunc c() {\n\treturn 3\n}\n"
	newBody := "func a() {\n\treturn 10\n}\n\nfunc b2() {\n\treturn 2\n\t// extra\n}\n\nfunc c() {\n\treturn 3\n}\n"
	assertDiffApplies(t, oldBody, newBody)
}
