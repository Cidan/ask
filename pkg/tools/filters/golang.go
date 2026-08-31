package filters

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// GoFilter handles the `go` toolchain. `go test` gets the full treatment:
// a `-json` event stream is aggregated into a per-package pass/fail/skip
// summary with failing-test output kept verbatim, and human text mode
// drops the RUN/PASS chatter while keeping ok/FAIL lines. It handles only
// `go test`; the other `go` subcommands are declarative rules (rules.go:
// go build/vet → pass/fail, go mod/get → download strip, go run →
// passthrough), so subcommand behavior lives in one place.
type GoFilter struct{}

func (GoFilter) Name() string { return "go" }

func (GoFilter) Match(fields []string) bool {
	if len(fields) == 0 || progOf(fields[0]) != "go" {
		return false
	}
	sub, ok := subcommandAfter(fields, goValueFlags)
	return ok && sub == "test"
}

func (GoFilter) Filter(fields []string, raw string, exit int) string {
	if events, ok := parseGoTestJSON(raw); ok {
		return renderGoTest(events, exit, raw)
	}
	return filterGoTestText(raw, exit)
}

var goTextNoise = []string{"=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "--- PASS"}

// filterGoTestText compresses human-format `go test` output: RUN/PAUSE/
// CONT/NAME/PASS markers and bare `PASS` verdicts are dropped; per-package
// `ok`/`SKIP` lines, `--- FAIL` blocks with their detail, and final `FAIL`
// verdicts are kept. If the run exited nonzero but no FAIL marker is
// present (a compile error, a panic before any test ran), the raw text is
// returned untouched — an unmodeled failure must never be hidden.
func filterGoTestText(raw string, exit int) string {
	lines := SplitLines(raw)
	out := make([]string, 0, len(lines))
	sawFail := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "PASS" || strings.HasPrefix(t, "go: downloading") || hasAnyPrefix(t, goTextNoise) {
			continue
		}
		if strings.HasPrefix(t, "--- FAIL") || strings.HasPrefix(t, "FAIL") {
			sawFail = true
		}
		out = append(out, l)
	}
	if exit != 0 && !sawFail {
		return raw
	}
	return strings.Join(out, "\n")
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// goTestEvent is one line of `go test -json` NDJSON.
type goTestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// parseGoTestJSON commits to NDJSON mode when at least half of the
// non-empty lines parse as test events (stderr — compile output, vet
// warnings — can interleave). Plain-text output returns false so the
// caller falls back to the text parser.
func parseGoTestJSON(raw string) ([]goTestEvent, bool) {
	var events []goTestEvent
	total, parsed := 0, 0
	for _, l := range SplitLines(raw) {
		if strings.TrimSpace(l) == "" {
			continue
		}
		total++
		var ev goTestEvent
		if json.Unmarshal([]byte(l), &ev) == nil && ev.Action != "" && ev.Package != "" {
			parsed++
			events = append(events, ev)
		}
	}
	if parsed == 0 || parsed*2 < total {
		return nil, false
	}
	return events, true
}

// goTestPkg aggregates one package's run.
type goTestPkg struct {
	passed, failed, skipped int
	buildFailed             bool
	elapsed                 float64
	failures                []goTestFailure
	pkgOutput               []string
}

type goTestFailure struct {
	test   string
	output []string
}

// renderGoTest folds NDJSON events into a one-line summary plus verbatim
// failing-test detail. A clean run (exit 0, no failures) collapses to just
// the summary; a nonzero exit whose events show nothing failed returns the
// raw stream untouched (something unmodeled broke).
func renderGoTest(events []goTestEvent, exit int, raw string) string {
	pkgs := aggregateGoTest(events)

	var passed, failed, skipped, buildFailedPkgs int
	var elapsed float64
	names := make([]string, 0, len(pkgs))
	for name, p := range pkgs {
		passed += p.passed
		failed += p.failed
		skipped += p.skipped
		elapsed += p.elapsed
		if p.buildFailed && p.failed == 0 {
			buildFailedPkgs++
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var parts []string
	if passed > 0 {
		parts = append(parts, strconv.Itoa(passed)+" passed")
	}
	if failed > 0 {
		parts = append(parts, strconv.Itoa(failed)+" failed")
	}
	if skipped > 0 {
		parts = append(parts, strconv.Itoa(skipped)+" skipped")
	}
	if buildFailedPkgs > 0 {
		parts = append(parts, strconv.Itoa(buildFailedPkgs)+" build-failed")
	}
	if len(parts) == 0 {
		parts = append(parts, "no tests ran")
	}
	summary := "go test: " + strings.Join(parts, ", ") + " across " + strconv.Itoa(len(pkgs)) + " packages"
	if elapsed > 0 {
		summary += " (" + strconv.FormatFloat(elapsed, 'f', 1, 64) + "s)"
	}

	if failed == 0 && buildFailedPkgs == 0 {
		if exit == 0 {
			return summary
		}
		// Events say everything passed but the process failed — preserve all.
		return raw
	}

	var b strings.Builder
	b.WriteString(summary)
	for _, name := range names {
		p := pkgs[name]
		for _, f := range p.failures {
			b.WriteString("\n\nFAIL " + name + " " + f.test)
			for _, l := range f.output {
				if strings.HasPrefix(strings.TrimSpace(l), "=== ") {
					continue // RUN/CONT markers are noise inside detail
				}
				b.WriteString("\n    " + l)
			}
		}
		if p.buildFailed && p.failed == 0 {
			b.WriteString("\n\nFAIL " + name + " (build failed)")
			for _, l := range p.pkgOutput {
				b.WriteString("\n    " + l)
			}
		}
	}
	return b.String()
}

// aggregateGoTest folds events into per-package state. Failing tests keep
// their streamed output; passing and skipped tests discard theirs.
func aggregateGoTest(events []goTestEvent) map[string]*goTestPkg {
	pkgs := make(map[string]*goTestPkg)
	pkgOf := func(name string) *goTestPkg {
		if p := pkgs[name]; p != nil {
			return p
		}
		p := &goTestPkg{}
		pkgs[name] = p
		return p
	}
	type key struct{ pkg, test string }
	open := make(map[key]*[]string)

	for _, ev := range events {
		p := pkgOf(ev.Package)
		k := key{ev.Package, ev.Test}
		switch ev.Action {
		case "run":
			buf := []string{}
			open[k] = &buf
		case "output":
			if ev.Test != "" {
				if buf := open[k]; buf != nil {
					*buf = append(*buf, strings.TrimRight(ev.Output, "\n"))
				}
				continue
			}
			p.pkgOutput = append(p.pkgOutput, strings.TrimRight(ev.Output, "\n"))
		case "pass":
			if ev.Test != "" {
				p.passed++
				delete(open, k)
				continue
			}
			p.elapsed += ev.Elapsed
		case "fail":
			if ev.Test != "" {
				p.failed++
				f := goTestFailure{test: ev.Test}
				if buf := open[k]; buf != nil {
					f.output = *buf
				}
				p.failures = append(p.failures, f)
				delete(open, k)
				continue
			}
			p.elapsed += ev.Elapsed
			if len(p.failures) == 0 {
				p.buildFailed = true
			}
		case "skip":
			if ev.Test != "" {
				p.skipped++
				delete(open, k)
			}
		case "build-output":
			p.pkgOutput = append(p.pkgOutput, strings.TrimRight(ev.Output, "\n"))
		case "build-fail":
			p.pkgOutput = append(p.pkgOutput, strings.TrimRight(ev.Output, "\n"))
			p.buildFailed = true
		}
	}
	return pkgs
}
