package filters

import (
	"regexp"
	"strings"
)

// KubectlFilter compresses kubectl output. Its biggest win is dropping the
// `managedFields` block from `-o yaml` / `-o json` resource dumps — pure
// server-side-apply bookkeeping that is often larger than the resource
// itself and never useful to a reader. It also strips kubectl's own
// deprecation and version-skew warnings. Everything else (get tables,
// describe, logs) passes through, and a failed command is returned
// verbatim so the error is never hidden.
type KubectlFilter struct{}

func (KubectlFilter) Name() string { return "kubectl" }

func (KubectlFilter) Match(fields []string) bool {
	return len(fields) >= 1 && progOf(fields[0]) == "kubectl"
}

// kubectlWarning matches kubectl's own preamble noise: deprecation notices
// and the client/server version-skew banner. Kept narrow so a log line or
// a resource value that merely contains "Warning" is never dropped.
var kubectlWarning = regexp.MustCompile(`^(Warning: .+ is deprecated|WARNING: version difference between client)`)

// yamlManagedFields matches the `managedFields:` key line, capturing its
// indentation so the whole block beneath it can be dropped.
var yamlManagedFields = regexp.MustCompile(`^(\s*)managedFields:\s*$`)

// jsonManagedFields matches the opening of a JSON `"managedFields": [`
// array, capturing its indentation.
var jsonManagedFields = regexp.MustCompile(`^(\s*)"managedFields":\s*\[`)

func (KubectlFilter) Filter(fields []string, raw string, exit int) string {
	if exit != 0 {
		return raw
	}
	lines := SplitLines(raw)
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if kubectlWarning.MatchString(strings.TrimSpace(l)) {
			continue
		}
		kept = append(kept, l)
	}
	kept = stripManagedFields(kept)
	return strings.Join(kept, "\n")
}

// stripManagedFields removes YAML and JSON managedFields blocks, replacing
// each with a one-line placeholder that records how many lines were
// dropped. Unrecognized shapes are left untouched.
func stripManagedFields(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		if m := yamlManagedFields.FindStringSubmatch(lines[i]); m != nil {
			indent := len(m[1])
			end := i + 1
			for end < len(lines) {
				ind, trimmed := lineIndent(lines[end])
				if trimmed == "" {
					break
				}
				if ind > indent || (ind == indent && strings.HasPrefix(trimmed, "-")) {
					end++
					continue
				}
				break
			}
			dropped := end - (i + 1)
			out = append(out, m[1]+"managedFields: [... "+itoa(dropped)+" lines omitted to save tokens ...]")
			i = end
			continue
		}
		if m := jsonManagedFields.FindStringSubmatch(lines[i]); m != nil {
			end := skipJSONArray(lines, i)
			dropped := end - (i + 1)
			out = append(out, m[1]+`"managedFields": [... `+itoa(dropped)+" lines omitted to save tokens ...],")
			i = end
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return out
}

// lineIndent returns a line's leading-space count and its trimmed content.
func lineIndent(s string) (int, string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i, strings.TrimSpace(s[i:])
}

// skipJSONArray returns the index of the line after the JSON array whose
// opening `[` is on lines[start], counting brackets outside string
// literals so nested arrays and braces are handled.
func skipJSONArray(lines []string, start int) int {
	depth := 0
	i := start
	for i < len(lines) {
		depth += bracketDelta(lines[i])
		i++
		if depth <= 0 {
			break
		}
	}
	return i
}

// bracketDelta returns the net `[` minus `]` count on a line, ignoring
// brackets inside double-quoted strings.
func bracketDelta(s string) int {
	d, inStr, esc := 0, false, false
	for _, c := range s {
		switch {
		case esc:
			esc = false
		case c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '[':
			d++
		case c == ']':
			d--
		}
	}
	return d
}
