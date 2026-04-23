package main

import (
	"encoding/json"
	"strings"
)

// Shared helpers for the "Render Tool Output" config toggle: one gate,
// one pair of message types (toolCallMsg / toolResultMsg), and the
// renderers below. Per-provider extraction lives in claude.go and
// codex.go so each wire format stays in its own file.

const (
	toolOutputMaxLines = 20
	toolOutputMaxChars = 2000
)

// renderToolCallBlock formats a tool invocation as a history entry:
//
//	▸ Read
//	    file_path: /foo/bar.go
//
// input fields render as "key: value" rows under a dimmed style so the
// call stays distinct from the result that follows. Non-string inputs
// are JSON-encoded so arrays and nested maps remain legible.
func renderToolCallBlock(name string, input map[string]any) string {
	header := diffPathStyle.Render("▸ " + nonEmpty(name, "tool"))
	lines := []string{outputStyle.Render(header)}
	for _, k := range sortedKeys(input) {
		lines = append(lines, outputStyle.Render(diffContextStyle.Render("    "+k+": "+formatToolInputValue(input[k]))))
	}
	return strings.Join(lines, "\n")
}

// renderToolResultBlock formats the output of a tool call. Long output
// is clipped to toolOutputMaxLines / toolOutputMaxChars with a trailing
// "(… N more lines)" marker. Error results render with the error style
// so a failed command stands out against a pile of successful ones.
func renderToolResultBlock(output string, isError bool) string {
	body, trimmedLines := clampToolOutput(output)
	var rows []string
	for _, ln := range strings.Split(body, "\n") {
		styled := diffContextStyle.Render("    " + ln)
		if isError {
			styled = errStyle.Render("    " + ln)
		}
		rows = append(rows, outputStyle.Render(styled))
	}
	if trimmedLines > 0 {
		rows = append(rows, outputStyle.Render(diffContextStyle.Render(
			"    (… "+pluralLines(trimmedLines)+" omitted)")))
	}
	return strings.Join(rows, "\n")
}

// clampToolOutput trims output to toolOutputMaxLines + toolOutputMaxChars.
// Returns the kept body plus the number of lines trimmed off so the
// caller can append a summary.
func clampToolOutput(s string) (string, int) {
	s = strings.TrimRight(s, "\n")
	if len(s) > toolOutputMaxChars {
		s = s[:toolOutputMaxChars]
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= toolOutputMaxLines {
		return s, 0
	}
	return strings.Join(lines[:toolOutputMaxLines], "\n"), len(lines) - toolOutputMaxLines
}

// formatToolInputValue stringifies one tool-input value. Short strings
// pass through verbatim; everything else becomes compact JSON so a
// reader can still see what was passed without drowning in pretty
// formatting.
func formatToolInputValue(v any) string {
	switch x := v.(type) {
	case string:
		return truncate(x, 200)
	case nil:
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return truncate(string(b), 200)
}

// sortedKeys returns the map keys in stable ("command" before "cwd")
// alphabetical order so successive renders of the same payload don't
// flicker.
func sortedKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// tiny n; manual insertion sort keeps us off "sort" imports already
	// used sparingly in this file.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func pluralLines(n int) string {
	if n == 1 {
		return "1 more line"
	}
	return itoa(n) + " more lines"
}

// itoa avoids pulling strconv just for plural rendering. n is always
// non-negative here.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
