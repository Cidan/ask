package filters

import (
	"regexp"
	"strings"
)

// Rule is a declarative, data-driven filter — ask's equivalent of one of
// RTK's src/filters/*.toml files. A table of Rules (see rules.go) covers
// the long tail of commands without a bespoke Go type each; complex
// aggregators (go test, git, pytest) stay hand-written. Every Rule is
// registered as its own Filter through ruleFilter, so ordering and
// precedence work exactly like the hand-written filters.
type Rule struct {
	// Name is the diagnostics / ledger-key label.
	Name string
	// Command matches the normalized command (program base + args joined by
	// single spaces, env assignments stripped), e.g. `^make\b`.
	Command *regexp.Regexp
	// Strip drops any line matching one of these.
	Strip []*regexp.Regexp
	// Keep, when non-empty, keeps only lines matching one of these (applied
	// after Strip).
	Keep []*regexp.Regexp
	// Replace applies regex substitutions before line filtering.
	Replace []Replacement
	// Summarize collapses the whole output to Message when Pattern matches
	// AND the command succeeded (exit 0). This is the success-detection
	// short-circuit ("BUILD SUCCESSFUL" → "gradle: ok"); it never fires on a
	// failing run, so an error is never replaced by a rosy summary.
	Summarize []OutputMatch
	// MaxLines keeps the first N lines (0 = unbounded); TailLines keeps the
	// last N. Prefer leaving these zero on build/test commands so a late
	// error is never truncated — the universal cap still bounds runaways.
	MaxLines  int
	TailLines int
	// TruncateAt caps each line's length in bytes (0 = unbounded).
	TruncateAt int
	// OnEmpty is emitted when filtering removed everything ("make: ok").
	OnEmpty string
	// KeepOnError returns the raw output verbatim on a nonzero exit.
	KeepOnError bool
}

// Replacement is one regex substitution.
type Replacement struct {
	Pattern *regexp.Regexp
	Repl    string
}

// OutputMatch is a whole-output success detector.
type OutputMatch struct {
	Pattern *regexp.Regexp
	Message string
}

// ruleFilter adapts one Rule to the Filter interface.
type ruleFilter struct{ r Rule }

func (f ruleFilter) Name() string { return f.r.Name }

func (f ruleFilter) Match(fields []string) bool {
	return f.r.Command.MatchString(normalizedCommand(fields))
}

func (f ruleFilter) Filter(fields []string, raw string, exit int) string {
	return f.r.apply(raw, exit)
}

// normalizedCommand joins fields with single spaces after stripping the
// program's directory prefix, so `^make\b` matches `/usr/bin/make build`.
func normalizedCommand(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, len(fields))
	parts[0] = progOf(fields[0])
	copy(parts[1:], fields[1:])
	return strings.Join(parts, " ")
}

// apply runs the rule's pipeline in RTK's documented order: replacements →
// success short-circuit → strip/keep → per-line truncation → tail → max →
// empty fallback.
func (r Rule) apply(raw string, exit int) string {
	if r.KeepOnError && exit != 0 {
		return raw
	}

	text := raw
	for _, rep := range r.Replace {
		text = rep.Pattern.ReplaceAllString(text, rep.Repl)
	}

	if exit == 0 {
		for _, m := range r.Summarize {
			if m.Pattern.MatchString(text) {
				return m.Message
			}
		}
	}

	lines := SplitLines(text)
	if len(r.Strip) > 0 || len(r.Keep) > 0 {
		kept := lines[:0]
		for _, l := range lines {
			if matchesAny(r.Strip, l) {
				continue
			}
			if len(r.Keep) > 0 && !matchesAny(r.Keep, l) {
				continue
			}
			kept = append(kept, l)
		}
		lines = kept
	}

	if r.TruncateAt > 0 {
		for i, l := range lines {
			if len(l) > r.TruncateAt {
				lines[i] = l[:r.TruncateAt] + "..."
			}
		}
	}
	if r.TailLines > 0 && len(lines) > r.TailLines {
		lines = lines[len(lines)-r.TailLines:]
	}
	if r.MaxLines > 0 && len(lines) > r.MaxLines {
		lines = lines[:r.MaxLines]
	}

	out := strings.Join(lines, "\n")
	if strings.TrimSpace(out) == "" && r.OnEmpty != "" {
		return r.OnEmpty
	}
	return out
}

func matchesAny(patterns []*regexp.Regexp, s string) bool {
	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}
