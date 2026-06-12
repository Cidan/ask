package main

import (
	"encoding/json"
	"strings"
)

// Shared helpers for the "Tool Output" config tri-state: one gate,
// one pair of message types (toolCallMsg / toolResultMsg), and the
// renderers below. The agent runtime (agent_run.go) emits the
// messages; every native tool carries a model-authored "description"
// phrase that becomes the call's headline here and the streaming
// status line there.

// toolOutputMode is the user-visible tri-state for tool output rendering.
//
//	full  — show the call header (with the description phrase), every
//	       input field, and the result body (including the "started
//	       background job" ack bash emits for run_in_background calls).
//	short — show the call header with the description phrase only; for
//	       calls without a phrase, fall back to the highest-signal
//	       input fields per known tool (see shortToolFields). The
//	       result body renders for foreground calls; background-call
//	       results are suppressed.
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
// when the mode is "short" AND the call carries no description phrase
// (old transcripts, MCP tools without one). A tool not present here
// renders just the header in short mode — letting the user know
// something happened without dumping arbitrary input maps. New
// built-ins should be added here with their highest-signal field(s).
var shortToolFields = map[string][]string{
	"bash":       {"command"},
	"edit":       {"file_path"},
	"end_turn":   {"summary"},
	"fetch":      {"url"},
	"glob":       {"pattern"},
	"grep":       {"include", "pattern"},
	"job_kill":   {"job_id"},
	"job_output": {"job_id"},
	"ls":         {"path"},
	"read":       {"file_path"},
	"task":       {"agent", "prompt"},
	"write":      {"file_path"},
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

// toolPhraseFieldDoc is the schema doc every native tool's
// "description" param carries (struct tags repeat it verbatim; the
// bridge adapter injects it into generated schemas). One sentence,
// model-facing: the model authors the phrase in the same tool call.
const toolPhraseFieldDoc = "one short human-readable phrase (under 10 words) telling the user what this call is doing"

// toolPhraseMaxChars bounds what qualifies as a phrase. Inputs whose
// "description" field is real payload (linear_create_issue's Markdown
// body, arbitrary MCP tools) produce long or multi-line values that
// must not masquerade as the call headline.
const toolPhraseMaxChars = 120

// toolCallPhrase extracts the model-authored description phrase from
// a tool input map. Empty when absent or when the value doesn't look
// like a short single-line phrase.
func toolCallPhrase(input map[string]any) string {
	s, _ := input["description"].(string)
	s = strings.TrimSpace(s)
	if s == "" || len(s) > toolPhraseMaxChars || strings.ContainsRune(s, '\n') {
		return ""
	}
	return s
}

// renderToolCallBlock formats a tool invocation as a history entry.
// Every native tool call carries a model-authored phrase, so the
// normal shape is one line:
//
//	▸ bash — Looking for the latest files
//
// In short mode (the default) the phrase IS the rendering — no input
// rows. Calls without a phrase (old transcripts, MCP tools that lack
// the param) fall back to the shortToolFields allowlist:
//
//	▸ read
//	    file_path: /foo/bar.go
//
// Full mode keeps the phrase in the header and renders every input
// field as "key: value" rows (minus the description, which would just
// duplicate the header). Non-string inputs are JSON-encoded so arrays
// and nested maps remain legible.
func renderToolCallBlock(name string, input map[string]any, mode toolOutputMode) string {
	phrase := toolCallPhrase(input)
	header := diffPathStyle.Render("▸ " + nonEmpty(name, "tool"))
	if phrase != "" {
		header += diffContextStyle.Render(" — " + phrase)
	}
	lines := []string{outputStyle.Render(header)}
	if mode == toolOutputShort {
		if phrase != "" {
			return lines[0]
		}
		input = filterShortInputs(name, input)
	}
	for _, k := range sortedKeys(input) {
		if k == "description" && phrase != "" {
			continue
		}
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
