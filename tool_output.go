package main

import (
	"encoding/json"
	"strings"
)

// Shared helpers for the "Tool Output" config tri-state: one gate,
// one pair of message types (toolCallMsg / toolResultMsg), and the
// renderers below. Per-provider extraction lives in claude.go and
// codex.go so each wire format stays in its own file.

// toolOutputMode is the user-visible tri-state for tool output rendering.
//
//	full  — show the call header, every input field, and the result body
//	       (including the "Command running in background with ID: …" ack
//	       Bash emits for run_in_background calls).
//	short — show the call header, only the highest-signal input fields
//	       per known tool (see shortToolFields), and the result body for
//	       foreground calls. Background-call results are suppressed.
//	off   — render nothing for tool calls or their results, not even
//	       headers.
type toolOutputMode string

const (
	toolOutputFull  toolOutputMode = "full"
	toolOutputShort toolOutputMode = "short"
	toolOutputOff   toolOutputMode = "off"

	toolOutputMaxLines = 20
	toolOutputMaxChars = 2000
)

// defaultToolOutputMode is what new installs and unrecognized values
// settle on — short keeps history readable without hiding tool activity
// entirely.
const defaultToolOutputMode = toolOutputShort

// parseToolOutputMode coerces a config string to a known mode. Empty
// or unrecognized values fall back to defaultToolOutputMode so a typo
// in ask.json never silences tool output completely.
func parseToolOutputMode(s string) toolOutputMode {
	switch toolOutputMode(s) {
	case toolOutputFull, toolOutputShort, toolOutputOff:
		return toolOutputMode(s)
	}
	return defaultToolOutputMode
}

// shortToolFields lists the input keys we surface for each known tool
// when the mode is "short". A tool not present here renders just the
// header in short mode — letting the user know something happened
// without dumping arbitrary input maps. New built-ins should be added
// here with their highest-signal field(s).
var shortToolFields = map[string][]string{
	// Claude built-ins.
	"Bash":         {"command"},
	"BashOutput":   {"bash_id"},
	"Edit":         {"file_path"},
	"ExitPlanMode": {"plan"},
	"Glob":         {"pattern"},
	"Grep":         {"glob", "output_mode", "pattern"},
	"KillBash":     {"shell_id"},
	"NotebookEdit": {"notebook_path"},
	"Read":         {"file_path"},
	"Task":         {"description"},
	"WebFetch":     {"url"},
	"WebSearch":    {"query"},
	"Write":        {"file_path"},
	// Codex tool surface (commandExecution becomes "shell").
	"shell": {"command"},
}

// filterShortInputs keeps only the allowlisted keys for the named tool
// in short mode. Tools without an allowlist entry get no input rows at
// all — that's the explicit signal "we don't know what's important
// here, so skip it".
func filterShortInputs(name string, input map[string]any) map[string]any {
	if len(input) == 0 {
		return input
	}
	allow, ok := shortToolFields[name]
	if !ok {
		return nil
	}
	out := make(map[string]any, len(allow))
	for _, k := range allow {
		if v, present := input[k]; present {
			out[k] = v
		}
	}
	return out
}

// nextToolOutputMode advances the tri-state for /config row cycling:
// full → short → off → full. Unknown values reset to the default so
// the picker never gets stuck on an invalid setting.
func nextToolOutputMode(cur toolOutputMode) toolOutputMode {
	switch cur {
	case toolOutputFull:
		return toolOutputShort
	case toolOutputShort:
		return toolOutputOff
	case toolOutputOff:
		return toolOutputFull
	}
	return defaultToolOutputMode
}

// shouldRenderToolCall decides whether a tool call goes into history.
// Quiet mode and "off" suppress everything; in any other mode the call
// header always renders so the user knows something fired. Background
// calls render too (as their command/inputs are still useful) — only
// the result ack is gated on full mode in shouldRenderToolResult.
func (m model) shouldRenderToolCall(_ toolCallMsg) bool {
	if m.workflowRun != nil {
		// Workflow tabs render the clean per-step summary list, not the
		// raw transcript — no tool calls.
		return false
	}
	if m.quietMode || m.toolOutputMode == toolOutputOff {
		return false
	}
	return true
}

// shouldRenderToolResult decides whether a tool result goes into
// history. Background results are silenced in non-full modes — their
// payload is only the launch ack ("Command running in background with
// ID: …") and the actual completion arrives via task_notification.
func (m model) shouldRenderToolResult(msg toolResultMsg) bool {
	if m.workflowRun != nil {
		return false
	}
	if m.quietMode || m.toolOutputMode == toolOutputOff {
		return false
	}
	if msg.background && m.toolOutputMode != toolOutputFull {
		return false
	}
	return true
}

// renderToolCallBlock formats a tool invocation as a history entry:
//
//	▸ Read
//	    file_path: /foo/bar.go
//
// input fields render as "key: value" rows under a dimmed style so the
// call stays distinct from the result that follows. Non-string inputs
// are JSON-encoded so arrays and nested maps remain legible. In short
// mode, the input map is filtered through shortToolFields so only the
// highest-signal keys per known tool show up.
func renderToolCallBlock(name string, input map[string]any, mode toolOutputMode) string {
	if mode == toolOutputShort {
		input = filterShortInputs(name, input)
	}
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
