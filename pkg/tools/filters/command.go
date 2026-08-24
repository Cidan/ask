package filters

import (
	"path/filepath"
	"regexp"
	"strings"
)

// MaxLines caps universal (post-filter) output middle-out. Semantic
// filters do the heavy compression; this is only the safety net for
// commands with no dedicated filter, so it stays generous — the point is
// to bound pathological output, not to drop signal.
const MaxLines = 1000

var ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// StripANSI removes CSI escape sequences (colors, cursor moves). It is the
// first thing every filter sees, so downstream matching is on plain text.
func StripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiEscapeRegex.ReplaceAllString(s, "")
}

// SplitLines splits on newlines without allocating a final empty element
// for a trailing newline — callers re-add the terminator via JoinLines.
func SplitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// JoinLines rejoins lines with newlines, restoring a trailing newline when
// the original output had one.
func JoinLines(lines []string, endedNL bool) string {
	out := strings.Join(lines, "\n")
	if out != "" && endedNL {
		out += "\n"
	}
	return out
}

// TrimTrailingSpace strips trailing spaces/tabs/CR from every line.
func TrimTrailingSpace(lines []string) []string {
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return lines
}

// SqueezeBlanks collapses runs of blank lines into a single blank line.
func SqueezeBlanks(lines []string) []string {
	out := lines[:0]
	prevBlank := false
	for _, l := range lines {
		blank := l == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, l)
		prevBlank = blank
	}
	return out
}

// TrimBlankEdges drops leading and trailing blank lines.
func TrimBlankEdges(lines []string) []string {
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// DedupConsecutive collapses runs of identical lines into one line tagged
// with a `(×N)` multiplier. Only applied to the universal fallback —
// semantic-filter output carries meaningful repeats and is left alone.
func DedupConsecutive(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		run := j - i
		if run > 1 && lines[i] != "" {
			out = append(out, lines[i]+" (x"+itoa(run)+")")
		} else {
			for range run {
				out = append(out, lines[i])
			}
		}
		i = j
	}
	return out
}

// CapLines truncates middle-out to MaxLines, leaving a marker naming how
// many lines were dropped. Head and tail are kept because that is where a
// command's setup and verdict live.
func CapLines(lines []string) []string {
	if len(lines) <= MaxLines {
		return lines
	}
	half := MaxLines / 2
	out := make([]string, 0, MaxLines+1)
	out = append(out, lines[:half]...)
	out = append(out, "... "+itoa(len(lines)-MaxLines)+" lines truncated to save tokens ...")
	out = append(out, lines[len(lines)-half:]...)
	return out
}

// itoa is strconv.Itoa without the import churn for a hot, tiny path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

var pipelineSplitRegex = regexp.MustCompile(`(&&|\|\||;|\|)`)

// BaseCommand tokenizes the final segment of a possibly-chained shell
// command, skipping leading environment assignments (FOO=1 BAR=baz cmd …).
// It is the single source of truth for command identification: both the
// filter registry and the savings ledger (via ExtractBaseCommand) parse
// commands through here so their keys always agree.
func BaseCommand(command string) []string {
	parts := pipelineSplitRegex.Split(command, -1)
	if len(parts) == 0 {
		return nil
	}
	fields := strings.Fields(strings.TrimSpace(parts[len(parts)-1]))
	for i := range fields {
		if !isEnvAssignment(fields[i]) {
			return fields[i:]
		}
	}
	return nil
}

// isEnvAssignment reports whether f is a leading `NAME=value` shell
// assignment rather than the program itself.
func isEnvAssignment(f string) bool {
	idx := strings.IndexByte(f, '=')
	if idx <= 0 || strings.Contains(f[:idx], "/") {
		return false
	}
	for i, c := range f[:idx] {
		alpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i == 0 && !alpha {
			return false
		}
		if i > 0 && !alpha && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// progOf returns the executable name of a field with any directory prefix
// stripped (/usr/bin/git → git).
func progOf(field string) string { return filepath.Base(field) }
