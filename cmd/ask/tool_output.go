package main

import (
	"encoding/json"
	"math"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// Shared helpers for the "Tool Output" config tri-state: one gate,
// one pair of message types (toolCallMsg / toolResultMsg), and the
// renderers below. The agent runtime (agent_run.go) emits the
// messages; every native tool carries a model-authored "description"
// phrase that becomes the call's headline here and the streaming
// status line there.

// toolOutputMode is the user-visible tri-state for tool output rendering.
// The call header always leads with a per-tool primary argument (the file,
// the command, the query — see toolPrimaryArg), falling back to the
// model-authored phrase, then the shortToolFields allowlist. Result bodies
// are per-tool (renderToolResultBlock): read shows a line count, bash shows
// its exit code plus output, edit/write show nothing (the diff carries it).
//
//	full  — call header with every input field, and the full (unclamped)
//	       result body, including the "started background job" ack bash
//	       emits for run_in_background calls.
//	short — one compact line per call: the header only. The result body is
//	       dropped, with two exceptions that stay one line — read keeps its
//	       line-count summary, and bash keeps its "exit N" status. Errors
//	       and every other tool's output are hidden; the ▸ glyph carries
//	       the pass/fail signal (it lands red on error, see settleToolCall).
//	off   — render nothing for tool calls or their results, not even
//	       headers.
type toolOutputMode string

const (
	toolOutputFull  toolOutputMode = "full"
	toolOutputShort toolOutputMode = "short"
	toolOutputOff   toolOutputMode = "off"
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
	"bash":         {"command"},
	"edit":         {"file_path"},
	"end_turn":     {"summary"},
	"fetch":        {"url"},
	"glob":         {"pattern"},
	"grep":         {"include", "pattern"},
	"invoke_tool":  {"tool_name"},
	"job_kill":     {"job_id"},
	"job_output":   {"job_id"},
	"ls":           {"path"},
	"read":         {"file_path"},
	"search_tools": {"query"},
	"task":         {"agent", "prompt"},
	"web_search":   {"query"},
	"write":        {"file_path"},
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
	return renderToolCallBlockStyled(name, input, mode, diffPathStyle)
}

// renderToolCallBlockStyled is renderToolCallBlock with the "▸ name" glyph
// drawn in a caller-supplied style instead of the resting diffPathStyle.
// The projection bakes the resting color; the in-flight animation
// (ensureEntryWrapped) re-invokes this each frame with a hue-rotated style
// so the arrow pulses while the call runs, and settleToolCall re-bakes it
// with restingToolGlyphStyle (red on error) once the result lands. Only the
// glyph unit is restyled — the primary/phrase and any input rows keep the
// dim context style.
func renderToolCallBlockStyled(name string, input map[string]any, mode toolOutputMode, glyphStyle lipgloss.Style) string {
	phrase := toolCallPhrase(input)
	primary := toolPrimaryArg(name, input)
	header := glyphStyle.Render("▸ " + nonEmpty(name, "tool"))
	switch {
	case primary != "":
		// Concrete argument (the file, the command, the query) beats the
		// phrase in the permanent header — the phrase still shows live on
		// the spinner status line while the call runs.
		header += diffContextStyle.Render("  " + primary)
	case phrase != "":
		header += diffContextStyle.Render(" — " + phrase)
	}
	lines := []string{outputStyle.Render(header)}
	if mode == toolOutputShort {
		if primary != "" || phrase != "" {
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

// inflightGlyphPeriod is how long, in seconds, the ▸ glyph takes to make
// one full hue rotation while a tool call is in flight. The viewport
// repaints at the spinner's 60fps while any call runs (see
// contentFingerprint), so the arrow reads as a smooth rainbow at that rate.
const inflightGlyphPeriod = 1.5

// inflightGlyphHex returns the color the ▸ glyph pulses through at time t
// (seconds). Hue rotates a full turn every inflightGlyphPeriod; saturation
// and value match the EQ meter's palette so the two animations feel of a
// piece.
func inflightGlyphHex(t float64) string {
	hue := math.Mod(t/inflightGlyphPeriod, 1.0)
	if hue < 0 {
		hue += 1.0
	}
	return hsvToHex(hue*360.0, 0.75, 0.58)
}

// inflightToolGlyphStyle is the animated glyph style for time t (seconds),
// bold to match the resting diffPathStyle weight.
func inflightToolGlyphStyle(t float64) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(inflightGlyphHex(t))).Bold(true)
}

// restingToolGlyphStyle is the color the ▸ glyph settles on when a call
// finishes: the normal accent (diffPathStyle) on success, red on error so a
// failed call is visible at a glance even in short mode where its body is
// hidden.
func restingToolGlyphStyle(errored bool) lipgloss.Style {
	if errored {
		return errStyle.Bold(true)
	}
	return diffPathStyle
}

// toolPrimaryArg returns the single highest-signal argument to show in a
// known tool's header — the file for read/write/edit/ls, the command for
// bash, the pattern/query for search tools. Empty for tools without an
// obvious primary (task, MCP, …), where the phrase is used instead.
func toolPrimaryArg(name string, input map[string]any) string {
	str := func(k string) string { s, _ := input[k].(string); return strings.TrimSpace(s) }
	var v string
	switch strings.ToLower(name) {
	case "read", "write", "edit":
		if p := str("file_path"); p != "" {
			v = shortenPath(p)
		}
	case "bash":
		if c := str("command"); c != "" {
			v = "$ " + firstLine(c)
		}
	case "grep":
		if p := str("pattern"); p != "" {
			if inc := str("include"); inc != "" {
				v = p + "  (" + inc + ")"
			} else {
				v = p
			}
		}
	case "glob":
		v = str("pattern")
	case "ls":
		if p := str("path"); p != "" {
			v = shortenPath(p)
		}
	case "fetch":
		v = str("url")
	case "web_search":
		v = str("query")
	}
	return truncate(v, 160)
}

// firstLine collapses a multi-line value (a heredoc command, say) to its
// first line with an ellipsis so a header stays one logical line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

// renderToolResultBlock formats a tool result as the body under its call
// header. It dispatches per tool so each reads cleanly: a read shows just
// its line count (never the file dump), a bash shows its exit code and a
// clipped tail of output, an edit/write shows nothing (its diff already
// tells the story). Everything else — and every error, whichever tool —
// falls to the generic clipped body. Returns "" when there is nothing
// worth showing, in which case the projection drops the entry entirely.
func renderToolResultBlock(name, output string, isError bool, exitCode int, hasExitCode bool, mode toolOutputMode) string {
	if !isError {
		switch strings.ToLower(name) {
		case "read":
			return renderReadResult(output)
		case "edit", "write":
			return ""
		case "bash":
			return renderBashResult(output, exitCode, hasExitCode, mode)
		}
	}
	return renderGenericResult(output, isError, mode)
}

// renderReadResult collapses a read to a one-line summary — the line
// count, or the tool's own parenthetical notice ("(empty file)",
// "(no lines at offset …)"). The file itself is on the call header; the
// content is deliberately never dumped into history.
func renderReadResult(output string) string {
	trimmed := strings.TrimRight(output, "\n")
	var summary string
	switch {
	case trimmed == "":
		summary = "(empty)"
	case strings.HasPrefix(trimmed, "("):
		summary = trimmed
	default:
		summary = pluralReadLines(strings.Count(trimmed, "\n") + 1)
	}
	return outputStyle.Render(diffContextStyle.Render("  " + summary))
}

// renderBashResult shows the exit code (green for 0, red otherwise). In full
// mode the combined output follows, unclipped; in short mode the exit line
// stands alone. Background launches and still-running polls carry no exit
// code, so short mode renders nothing for them and their header stands alone.
func renderBashResult(output string, exitCode int, hasExitCode bool, mode toolOutputMode) string {
	var rows []string
	if hasExitCode {
		st := successStyle
		if exitCode != 0 {
			st = errStyle
		}
		rows = append(rows, outputStyle.Render(st.Render("  exit "+itoa(exitCode))))
	}
	// Short mode stays one line: the exit status, never the output body.
	if mode != toolOutputFull {
		return strings.Join(rows, "\n")
	}
	body := strings.TrimRight(output, "\n")
	if strings.TrimSpace(body) != "" {
		for _, ln := range strings.Split(body, "\n") {
			rows = append(rows, outputStyle.Render(diffContextStyle.Render("  "+ln)))
		}
	}
	return strings.Join(rows, "\n")
}

// renderGenericResult is the fallback body for every non-read/bash/edit tool
// and for every error. In full mode it renders the whole output one row per
// line (error style when the call failed); in short mode it renders nothing —
// the header alone stands, and a failed call is signalled by the glyph
// landing red (settleToolCall). Returns "" so the projection drops the entry.
func renderGenericResult(output string, isError bool, mode toolOutputMode) string {
	if mode != toolOutputFull {
		return ""
	}
	body := strings.TrimRight(output, "\n")
	var rows []string
	for _, ln := range strings.Split(body, "\n") {
		styled := diffContextStyle.Render("  " + ln)
		if isError {
			styled = errStyle.Render("  " + ln)
		}
		rows = append(rows, outputStyle.Render(styled))
	}
	return strings.Join(rows, "\n")
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

// extractExitCode pulls a shell tool's process exit code out of its raw
// result map. Values arrive as float64 after the tool result round-trips
// through JSON, but int/int64/json.Number are handled too. ok is false
// for tools that carry no exit_code (read, edit, MCP, …) so the renderer
// can tell "exit 0" apart from "not a shell result".
func extractExitCode(resp map[string]any) (int, bool) {
	v, ok := resp["exit_code"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
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

func pluralReadLines(n int) string {
	if n == 1 {
		return "1 line"
	}
	return itoa(n) + " lines"
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
