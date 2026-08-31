// Package filters compresses shell-command output before it reaches the
// model, RTK-style: an ordered set of command-aware semantic filters over
// a universal fallback. Every filter is pure (no I/O, no shared state), so
// the whole pipeline is safe to run concurrently from each bash call.
package filters

import "strings"

// Filter is a command-aware output compressor.
type Filter interface {
	// Name identifies the filter in diagnostics; it is also the shape of
	// the savings-ledger key for commands this filter claims.
	Name() string
	// Match reports whether this filter handles the command whose parsed
	// fields these are — env assignments already stripped, fields[0] the
	// program (path base not yet stripped; use the progOf helper).
	Match(fields []string) bool
	// Filter rewrites raw (ANSI already stripped) into compact form. exit
	// is the process exit code and steers verbosity: 0 may collapse hard to
	// a summary; nonzero must preserve failure detail; an unmodeled nonzero
	// should return raw untouched so a real failure is never hidden.
	Filter(fields []string, raw string, exit int) string
}

// registry is the ordered filter list. Order matters: the first filter
// whose Match returns true wins, so the hand-written aggregators precede
// the declarative rule table (rules.go), which precedes the universal
// fallback. Built once at load; the slice is immutable thereafter.
var registry = buildRegistry()

func buildRegistry() []Filter {
	fs := []Filter{
		GoFilter{},
		GitFilter{},
		PytestFilter{},
		InstallFilter{},
		KubectlFilter{},
	}
	return append(fs, ruleFilters()...)
}

// match returns the first registered filter that claims fields, or nil for
// the universal fallback.
func match(fields []string) Filter {
	if len(fields) == 0 {
		return nil
	}
	for _, f := range registry {
		if f.Match(fields) {
			return f
		}
	}
	return nil
}

// Apply routes raw output through the matching semantic filter (if any),
// then the universal fallback pipeline: trailing-space trim, blank
// squeeze, edge trim, middle-out cap. Commands with no dedicated filter
// additionally get consecutive-run dedup — a semantic filter's output is
// left un-deduped because those formats carry meaningful repeats. saved is
// estimated at bytes/4 against the untouched input, matching the ledger's
// convention elsewhere.
func Apply(command, raw string, exit int) (filtered string, saved int) {
	if raw == "" {
		return "", 0
	}
	text := StripANSI(raw)
	endedNL := strings.HasSuffix(raw, "\n")

	fields := BaseCommand(command)
	var lines []string
	if f := match(fields); f != nil {
		lines = SplitLines(f.Filter(fields, text, exit))
	} else {
		lines = DedupConsecutive(SplitLines(text))
	}
	lines = CapLines(TrimBlankEdges(SqueezeBlanks(TrimTrailingSpace(lines))))

	filtered = JoinLines(lines, endedNL)
	if filtered == "" && raw != "" {
		filtered = "(command output filtered out to save tokens)\n"
	}
	saved = max((len(raw)-len(filtered))/4, 0)
	return filtered, saved
}
