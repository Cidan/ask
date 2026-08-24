package filters

import (
	"regexp"
	"strings"
)

// PytestFilter compresses pytest runs. On a clean run it collapses to the
// final summary line ("=== 42 passed in 1.2s ==="), dropping the platform
// banner and the per-file progress dots. On a failing run it keeps the
// FAILURES/ERRORS section onward — the tracebacks, the short test summary,
// and the final line — while dropping the noisy preamble. Anything it does
// not recognize is returned untouched.
type PytestFilter struct{}

func (PytestFilter) Name() string { return "pytest" }

func (PytestFilter) Match(fields []string) bool { return hasPytestInvocation(fields) }

// hasPytestInvocation detects `pytest`, `py.test`, and `python[3] -m
// pytest` shapes anywhere in the command.
func hasPytestInvocation(fields []string) bool {
	for i, f := range fields {
		p := progOf(f)
		if i == 0 && (p == "pytest" || p == "py.test") {
			return true
		}
		if f == "-m" && i+1 < len(fields) && fields[i+1] == "pytest" {
			return true
		}
	}
	return false
}

var (
	pytestSummary = regexp.MustCompile(`^=+ .*\b(passed|failed|error|errors|skipped|xfailed|xpassed|no tests ran|deselected)\b.* =+\s*$`)
	pytestSection = regexp.MustCompile(`^=+ (FAILURES|ERRORS) =+\s*$`)
)

func (PytestFilter) Filter(fields []string, raw string, exit int) string {
	lines := SplitLines(raw)

	summaryIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if pytestSummary.MatchString(strings.TrimSpace(lines[i])) {
			summaryIdx = i
			break
		}
	}
	if summaryIdx < 0 {
		return raw // no recognizable pytest summary — don't touch it
	}

	if exit == 0 {
		return strings.TrimSpace(lines[summaryIdx])
	}

	// Failure: keep from the first FAILURES/ERRORS banner to the end.
	for i, l := range lines {
		if pytestSection.MatchString(strings.TrimSpace(l)) {
			return strings.Join(lines[i:], "\n")
		}
	}
	return raw // failed but no failures section we recognize
}
