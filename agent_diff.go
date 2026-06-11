package main

import (
	"fmt"
	"strings"
)

// unifiedDiff produces a standard unified diff (3 context lines) of two
// file bodies for the agent harness's edit/write tools. The output
// round-trips through parseUnifiedDiff so the existing renderDiffBlock
// pipeline renders agent edits exactly like claude/codex diffs. Returns
// "" when the bodies are identical. No ---/+++ file header is emitted:
// toolDiffMsg carries the path separately and parseUnifiedDiff drops
// pre-@@ lines anyway.
func unifiedDiff(oldBody, newBody string) string {
	if oldBody == newBody {
		return ""
	}
	a, aNoEOL := splitDiffLines(oldBody)
	b, bNoEOL := splitDiffLines(newBody)
	ops := diffOps(diffKeys(a, aNoEOL), diffKeys(b, bNoEOL), myersBudget)

	const ctx = 3
	var out strings.Builder
	var hunk []string
	hunkOldStart, hunkNewStart := 0, 0
	hunkOldLines, hunkNewLines := 0, 0
	pendingCtx := 0 // equal lines seen since last change, not yet emitted

	aLine := func(i int) string { return a[i] }
	noEOLMarker := `\ No newline at end of file`

	flush := func() {
		if hunkOldLines == 0 && hunkNewLines == 0 {
			return
		}
		// Unified convention: a zero-length side anchors one line above.
		oldStart := hunkOldStart + 1
		if hunkOldLines == 0 {
			oldStart = hunkOldStart
		}
		newStart := hunkNewStart + 1
		if hunkNewLines == 0 {
			newStart = hunkNewStart
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, hunkOldLines, newStart, hunkNewLines)
		for _, l := range hunk {
			out.WriteString(l)
			out.WriteByte('\n')
		}
		hunk = nil
		hunkOldLines, hunkNewLines = 0, 0
	}

	ai, bi := 0, 0
	for opIdx, op := range ops {
		isLast := opIdx == len(ops)-1
		switch op.kind {
		case diffOpEqual:
			n := op.n
			if len(hunk) == 0 {
				// Not inside a hunk: remember up to ctx trailing lines as
				// leading context for the next change.
				ai += n
				bi += n
				pendingCtx = min(n, ctx)
				continue
			}
			if n <= 2*ctx && !isLast {
				// Gap small enough to keep the hunk contiguous with the
				// next change.
				for i := range n {
					hunk = append(hunk, " "+aLine(ai+i))
					if ai+i == len(a)-1 && aNoEOL {
						hunk = append(hunk, noEOLMarker)
					}
				}
				hunkOldLines += n
				hunkNewLines += n
				ai += n
				bi += n
				continue
			}
			// Close the hunk with at most ctx trailing context lines.
			tail := min(n, ctx)
			for i := range tail {
				hunk = append(hunk, " "+aLine(ai+i))
				if ai+i == len(a)-1 && aNoEOL {
					hunk = append(hunk, noEOLMarker)
				}
			}
			hunkOldLines += tail
			hunkNewLines += tail
			flush()
			ai += n
			bi += n
			pendingCtx = ctx
		case diffOpDelete, diffOpInsert:
			if len(hunk) == 0 {
				lead := pendingCtx
				hunkOldStart = ai - lead
				hunkNewStart = bi - lead
				for i := ai - lead; i < ai; i++ {
					hunk = append(hunk, " "+aLine(i))
				}
				hunkOldLines += lead
				hunkNewLines += lead
				pendingCtx = 0
			}
			if op.kind == diffOpDelete {
				for i := range op.n {
					hunk = append(hunk, "-"+a[ai+i])
					if ai+i == len(a)-1 && aNoEOL {
						hunk = append(hunk, noEOLMarker)
					}
				}
				hunkOldLines += op.n
				ai += op.n
			} else {
				for i := range op.n {
					hunk = append(hunk, "+"+b[bi+i])
					if bi+i == len(b)-1 && bNoEOL {
						hunk = append(hunk, noEOLMarker)
					}
				}
				hunkNewLines += op.n
				bi += op.n
			}
		}
	}
	flush()
	// No trailing newline: parseUnifiedDiff keeps hunk body lines
	// verbatim, so a final "\n" would read back as a spurious empty
	// hunk line.
	return strings.TrimSuffix(out.String(), "\n")
}

// splitDiffLines splits a body into lines without trailing newlines and
// reports whether the final line lacked one. An empty body is zero
// lines (not one empty line).
func splitDiffLines(s string) ([]string, bool) {
	if s == "" {
		return nil, false
	}
	noEOL := !strings.HasSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n"), noEOL
}

// diffKeys returns comparison keys for a line slice: identical text
// with differing end-of-file newline-ness must not compare equal, so
// the final line of a newline-less file gets a sentinel suffix.
func diffKeys(lines []string, noEOL bool) []string {
	if !noEOL || len(lines) == 0 {
		return lines
	}
	keys := make([]string, len(lines))
	copy(keys, lines)
	keys[len(keys)-1] += "\x00noeol"
	return keys
}

type diffOpKind int

const (
	diffOpEqual diffOpKind = iota
	diffOpDelete
	diffOpInsert
)

// diffOp is a run of n lines all sharing one op kind.
type diffOp struct {
	kind diffOpKind
	n    int
}

// myersBudget caps the Myers search depth. Typical edits trim to a tiny
// middle after common prefix/suffix removal; a middle whose edit
// distance exceeds the budget degrades to one delete-all/insert-all
// replacement — still a valid unified diff, just not minimal — instead
// of burning O(D²) memory on a pathological full-file rewrite.
const myersBudget = 2048

// diffOps computes run-length-encoded line ops via Myers O(ND), after
// trimming the common prefix and suffix.
func diffOps(a, b []string, budget int) []diffOp {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	midA := a[prefix : len(a)-suffix]
	midB := b[prefix : len(b)-suffix]

	var ops []diffOp
	appendOp := func(kind diffOpKind, n int) {
		if n == 0 {
			return
		}
		if len(ops) > 0 && ops[len(ops)-1].kind == kind {
			ops[len(ops)-1].n += n
			return
		}
		ops = append(ops, diffOp{kind, n})
	}

	appendOp(diffOpEqual, prefix)
	mid := myersOps(midA, midB, budget)
	for _, op := range mid {
		appendOp(op.kind, op.n)
	}
	appendOp(diffOpEqual, suffix)
	return ops
}

// myersOps runs the forward Myers greedy algorithm, backtracking
// through saved furthest-reaching frontiers. Exceeding the budget (or a
// degenerate one-sided input) yields a whole-slice replacement.
func myersOps(a, b []string, budget int) []diffOp {
	n, m := len(a), len(b)
	switch {
	case n == 0 && m == 0:
		return nil
	case n == 0:
		return []diffOp{{diffOpInsert, m}}
	case m == 0:
		return []diffOp{{diffOpDelete, n}}
	}
	maxD := min(n+m, budget)
	off := maxD
	v := make([]int, 2*maxD+2)
	var trace [][]int

	var endD int
	found := false
search:
	for d := 0; d <= maxD; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				endD = d
				found = true
				break search
			}
		}
	}
	if !found {
		return []diffOp{{diffOpDelete, n}, {diffOpInsert, m}}
	}

	// Backtrack from (n, m) through the saved frontiers, collecting ops
	// in reverse.
	type rev struct {
		kind diffOpKind
		n    int
	}
	var rops []rev
	push := func(kind diffOpKind, cnt int) {
		if cnt == 0 {
			return
		}
		if len(rops) > 0 && rops[len(rops)-1].kind == kind {
			rops[len(rops)-1].n += cnt
			return
		}
		rops = append(rops, rev{kind, cnt})
	}
	x, y := n, m
	for d := endD; d > 0; d-- {
		vPrev := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && vPrev[off+k-1] < vPrev[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := vPrev[off+prevK]
		prevY := prevX - prevK
		// Snake (equal run) between the edit and the current point.
		var snakeEnd int
		if prevK == k+1 {
			snakeEnd = prevX // insertion: x unchanged at edit
		} else {
			snakeEnd = prevX + 1 // deletion advances x by one
		}
		push(diffOpEqual, x-snakeEnd)
		if prevK == k+1 {
			push(diffOpInsert, 1)
		} else {
			push(diffOpDelete, 1)
		}
		x, y = prevX, prevY
	}
	push(diffOpEqual, x) // leading snake at d=0

	ops := make([]diffOp, 0, len(rops))
	for i := len(rops) - 1; i >= 0; i-- {
		ops = append(ops, diffOp{rops[i].kind, rops[i].n})
	}
	return ops
}
