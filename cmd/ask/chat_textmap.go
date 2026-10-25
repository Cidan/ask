package main

import (
	"strings"
	"unicode"

	"github.com/Cidan/ask/pkg/diff"
	xansi "github.com/charmbracelet/x/ansi"
)

// textCell is one drawn cell of an entry's wrapped line: the columns it
// covers and, when it draws a character of the entry's copy source, that
// character's byte range in the source. start is -1 for whatever the
// renderer added — bullets, borders, padding, line numbers — and for
// whitespace.
type textCell struct {
	col, width int
	start, end int
}

// entryTextMap ties the cells of an entry's wrapped lines, at one width, to
// its copy source. It is what turns a selection of what the chat shows into
// the raw text of exactly that range.
type entryTextMap struct {
	width   int
	source  string
	lines   [][]textCell
	extents []int
}

// kittyPlaceholder is the rune Kitty draws an image over; it is never text.
const kittyPlaceholder = '\U0010EEEE'

// buildEntryTextMap maps the drawn cells of lines onto source. Both sides
// become grapheme tokens with every run of whitespace — a line break, the
// padding the renderer added, the spaces a wrap consumed — folded into one
// space, and a longest common subsequence of the two (diff.Align) pairs
// each drawn character with the source character it shows. What the
// renderer hid (`**`, fences, `#`s) and what it added (`•`, `│`) fall out
// as unpaired.
func buildEntryTextMap(lines []string, source string, width int) *entryTextMap {
	tm := &entryTextMap{width: width, source: source, lines: make([][]textCell, len(lines)), extents: make([]int, len(lines))}

	type ref struct{ line, cell int }
	var shown []string
	var owner []ref
	space := func() {
		if len(shown) > 0 && shown[len(shown)-1] != " " {
			shown = append(shown, " ")
			owner = append(owner, ref{-1, -1})
		}
	}
	for li, line := range lines {
		space()
		tm.extents[li] = lineExtent(line)
		plain := xansi.Strip(line)
		col := 0
		for plain != "" {
			cluster, w := xansi.FirstGraphemeCluster(plain, xansi.GraphemeWidth)
			plain = plain[len(cluster):]
			if col >= chatLeftMarginCols {
				tm.lines[li] = append(tm.lines[li], textCell{col: col, width: max(w, 1), start: -1, end: -1})
				if isBlankCluster(cluster) {
					space()
				} else {
					shown = append(shown, cluster)
					owner = append(owner, ref{li, len(tm.lines[li]) - 1})
				}
			}
			col += w
		}
	}

	var src []string
	var at [][2]int
	for off := 0; off < len(source); {
		cluster, _ := xansi.FirstGraphemeCluster(source[off:], xansi.GraphemeWidth)
		if cluster == "" {
			break
		}
		if isBlankCluster(cluster) {
			if len(src) == 0 || src[len(src)-1] != " " {
				src = append(src, " ")
				at = append(at, [2]int{-1, -1})
			}
		} else {
			src = append(src, cluster)
			at = append(at, [2]int{off, off + len(cluster)})
		}
		off += len(cluster)
	}

	keys := func(tokens []string) []string {
		out := make([]string, len(tokens))
		for i, t := range tokens {
			out[i] = alignKey(t)
		}
		return out
	}
	for i, j := range diff.Align(keys(shown), keys(src)) {
		if j < 0 || owner[i].line < 0 {
			continue
		}
		c := &tm.lines[owner[i].line][owner[i].cell]
		c.start, c.end = at[j][0], at[j][1]
	}
	return tm
}

// alignKey is what a token is compared by: the glyphs the markdown renderer
// draws in place of markup compare equal to that markup — a list bullet to
// `-` `*` `+`, a table rule to `-`, a table or quote bar to `|` `>`, a done
// task's check to `x` — so a selection starting on one copies from the
// markup it stands for.
func alignKey(token string) string {
	switch token {
	case "•", "─", "-", "*", "+":
		return "-"
	case "│", "┼", "├", "┤", "|", ">":
		return "|"
	case "✓":
		return "x"
	}
	return token
}

// isBlankCluster reports whether a cluster draws no text: whitespace, or a
// Kitty image placeholder.
func isBlankCluster(s string) bool {
	for _, r := range s {
		if r == kittyPlaceholder {
			return true
		}
		if !unicode.IsSpace(r) && !unicode.Is(unicode.Mn, r) {
			return false
		}
	}
	return true
}

// lineExtent is the column just past the last character a wrapped line
// draws; its trailing padding is not part of it.
func lineExtent(line string) int {
	return xansi.StringWidth(strings.TrimRightFunc(xansi.Strip(line), unicode.IsSpace))
}
