package filters

import "regexp"

// re compiles a rule pattern at package load; a bad pattern is a
// programming error and should panic the process, never silently skip.
func re(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// ruleTable is the declarative long-tail of command filters — ask's own
// take on RTK's src/filters/*.toml. Most rules are strip-only (they remove
// progress/bookkeeping noise, never error lines, so failures pass through
// intact); build commands whose output is pure noise when they succeed set
// PassFail (collapse to "<name>: ok" on exit 0, filtered error on failure).
// Granularity is just the Command regex: `^make\b` is whole-program
// pass/fail, `^go\s+build\b` is one subcommand. Complex aggregators (go
// test, git, pytest) are hand-written filters registered ahead of these.
var ruleTable = []Rule{
	{
		// make is treated as pass/fail: a successful build collapses to
		// "make: ok" (the recipe output is pure noise when it works). A
		// failed build falls through to the strip pipeline, which clears the
		// directory chatter and cmake progress so the error is easy to find.
		Name:     "make",
		Command:  re(`^(make|gmake)\b`),
		PassFail: true,
		Strip: []*regexp.Regexp{
			re(`^(make|gmake)\[\d+\]: (Entering|Leaving) directory`), // not the "*** Error" line
			re(`^(make|gmake): Nothing to be done`),
			re(`^(make|gmake): '.*' is up to date`),
			re(`^\[\s*\d+%\]`), // cmake progress: [ 12%] Building…
			re(`^-- `),         // cmake configure probes
		},
	},
	{
		// go build/vet/install/generate are pass/fail: silent on success
		// (an empty result already), and on failure the compile errors show
		// with only download chatter stripped. go test is GoFilter; go run
		// and go tool fall through to the universal passthrough.
		Name:     "go-build",
		Command:  re(`^go\s+(build|vet|install|generate)\b`),
		PassFail: true,
		OnEmpty:  "go: ok",
		Strip:    []*regexp.Regexp{re(`^go: downloading`)},
	},
	{
		// go module plumbing: keep everything but the download chatter.
		Name:    "go-mod",
		Command: re(`^go\s+(mod|get|work)\b`),
		Strip:   []*regexp.Regexp{re(`^go: downloading`)},
	},
	{
		// cargo test: strip the build noise and every passing/ignored test
		// line, keeping FAILED lines, the `failures:` panic blocks, and the
		// `test result:` summaries. Strip-only, so failures survive. Must
		// precede the generic cargo rule below (first match wins).
		Name:    "cargo-test",
		Command: re(`^cargo\s+(test|nextest\s+run)\b`),
		Strip: []*regexp.Regexp{
			re(`^\s+(Compiling|Finished|Running|Updating|Downloading|Downloaded|Locking|Blocking|Doc-tests) `),
			re(`^running \d+ tests?$`),
			re(`^test .+ \.\.\. ok$`),
			re(`^test .+ \.\.\. ignored`),
		},
	},
	{
		// cargo build/check are pass/fail: "cargo: ok" on success, the
		// compiler errors on failure (with the Compiling/Updating status
		// chatter stripped). cargo run/clippy/etc fall through to the
		// universal passthrough so their output is kept. The strip patterns
		// require leading whitespace so a program's own "Compiling…" at
		// column zero is never mistaken for cargo status.
		Name:     "cargo-build",
		Command:  re(`^cargo\s+(build|check|b|c)\b`),
		PassFail: true,
		OnEmpty:  "cargo: ok",
		Strip: []*regexp.Regexp{
			re(`^\s+(Compiling|Updating|Downloading|Downloaded|Blocking|Locking|Adding|Installing) `),
		},
	},
	{
		// vitest / jest: strip passing-suite lines and the run banner so a
		// green run collapses to its summary and a red run keeps only the
		// FAIL blocks plus the summary. Strip-only ⇒ failures survive.
		Name:    "jstest",
		Command: re(`(^|\s)(vitest|jest)\b`),
		Strip: []*regexp.Regexp{
			re(`^PASS `),            // jest passing suite
			re(`^\s*[✓√] `),         // vitest/jest passing test (verbose)
			re(`^\s*RUN\s+v\d`),     // vitest banner
			re(`^\s*(Snapshots|Time):\s`), // jest footer noise
		},
	},
	{
		// gradle collapses to "gradle: ok" only when it prints BUILD
		// SUCCESSFUL (Summarize, not PassFail) — a long-running task like
		// `gradle bootRun` never prints it, so its output is never swallowed.
		// The strips clean up an ordinary build that isn't collapsed.
		Name:      "gradle",
		Command:   re(`^(gradle|gradlew|\./gradlew)\b`),
		Summarize: []OutputMatch{{Pattern: re(`(?m)^BUILD SUCCESSFUL`), Message: "gradle: ok"}},
		Strip: []*regexp.Regexp{
			re(`^> Task :.*\b(UP-TO-DATE|SKIPPED|NO-SOURCE|FROM-CACHE)\s*$`),
			re(`^Download(ing)? https?://`),
			re(`^Welcome to Gradle `),
			re(`^Starting a Gradle Daemon`),
		},
	},
	{
		Name:    "pip",
		Command: re(`(?:^|\s)pip[0-9]*\s+(install|download|wheel)\b`),
		Strip: []*regexp.Regexp{
			re(`^Requirement already satisfied`),
			re(`^\s*Collecting `),
			re(`^\s*Downloading `),
			re(`^\s*Using cached `),
			re(`^\s*Preparing metadata`),
			re(`^\s*Building wheel `),
			re(`^\s*Created wheel `),
			re(`^\s*Stored in directory`),
			re(`^\s*Getting requirements`),
			re(`^Installing collected packages`),
		},
	},
	{
		// Scoped to uv's package subcommands — never `uv run <cmd>`, whose
		// output belongs to the wrapped program.
		Name:    "uv",
		Command: re(`^uv\s+(pip|sync|add|remove|lock|install|export|tree|venv)\b`),
		Strip: []*regexp.Regexp{
			re(`^\s*Resolved \d+ package`),
			re(`^\s*Downloading `),
			re(`^\s*Building `),
			re(`^\s*Built `),
			re(`^\s*Prepared \d+ package`),
			re(`^\s*Audited \d+ package`),
			re(`^\s*Bytecode compiled `),
		},
	},
	{
		Name:    "poetry",
		Command: re(`^poetry\b`),
		Strip: []*regexp.Regexp{
			re(`^\s*[•·-] (Installing|Updating|Downgrading|Removing|Preparing) `),
			re(`^Package operations:`),
			re(`^Writing lock file`),
			re(`^Resolving dependencies`),
		},
	},
	{
		Name:    "bundle",
		Command: re(`^bundle\b`),
		Strip: []*regexp.Regexp{
			re(`^Using \S+ \d`),
			re(`^Fetching \S`),
			re(`^Resolving dependencies`),
			re(`^Following files may not be writable`),
		},
	},
	{
		Name:      "mypy",
		Command:   re(`(^|\s)mypy\b`),
		Summarize: []OutputMatch{{Pattern: re(`(?m)^Success: no issues found`), Message: "mypy: ok"}},
	},
	{
		Name:      "ruff",
		Command:   re(`(^|\s)ruff\b`),
		Summarize: []OutputMatch{{Pattern: re(`(?m)^All checks passed!`), Message: "ruff: ok"}},
	},
	{
		// cmake configure/build output is noise when it works → "cmake: ok".
		// On failure the `-- Detecting…` / `[N%]` progress is stripped so the
		// CMake Error stands out.
		Name:     "cmake",
		Command:  re(`^cmake\b`),
		PassFail: true,
		Strip:    []*regexp.Regexp{re(`^-- `), re(`^\[\s*\d+%\]`)},
	},
	{
		// bazel build is pass/fail → "bazel: ok"; on failure the progress
		// counters and phases are stripped so the error stands out. Must
		// precede the generic bazel rule (test/run keep their output).
		Name:     "bazel-build",
		Command:  re(`^bazel\s+build\b`),
		PassFail: true,
		OnEmpty:  "bazel: ok",
		Strip: []*regexp.Regexp{
			re(`^\s*\[[\d,]+ / [\d,]+\]`),
			re(`^(Loading|Analyzing|Computing): `),
			re(`^INFO: (Analyzed|From |Found \d+ target|Elapsed|Invocation ID|Streaming build)`),
		},
	},
	{
		// dotnet build/publish/restore are pass/fail → "dotnet: ok"; on
		// failure the restore/build chatter is stripped. dotnet run/test fall
		// through to their own handling / passthrough.
		Name:     "dotnet-build",
		Command:  re(`^dotnet\s+(build|publish|restore|msbuild|pack)\b`),
		PassFail: true,
		OnEmpty:  "dotnet: ok",
		Strip: []*regexp.Regexp{
			re(`^\s*Determining projects to restore`),
			re(`^\s*Restored `),
			re(`^\s*[0-9]+ Warning\(s\)$`),
		},
	},
	{
		// bazel's `[1,234 / 5,678] …` progress counters and loading/analyzing
		// phases are pure noise; the errors and the final result stay.
		Name:        "bazel",
		Command:     re(`^bazel\b`),
		KeepOnError: true,
		Strip: []*regexp.Regexp{
			re(`^\s*\[[\d,]+ / [\d,]+\]`),
			re(`^(Loading|Analyzing|Computing): `),
			re(`^INFO: (Analyzed|From |Found \d+ target|Elapsed|Invocation ID|Streaming build)`),
		},
	},
	{
		// terraform/tofu spend most of a plan echoing state refresh chatter;
		// strip it and keep the diff, the Plan line, and the apply summary.
		Name:        "terraform",
		Command:     re(`^(terraform|tofu)\b`),
		KeepOnError: true,
		Strip: []*regexp.Regexp{
			re(`: Refreshing state\.\.\.`),
			re(`: Reading\.\.\.`),
			re(`: Read complete after `),
			re(`: Still (reading|creating|modifying|destroying)\.\.\.`),
		},
	},
	{
		// docker build is pass/fail: "docker build: ok" on success (the whole
		// BuildKit log is noise when it works), and the full log verbatim on
		// failure (KeepOnError) so the failing step is fully visible.
		Name:        "docker-build",
		Command:     re(`^docker\s+(build|buildx\s+build)\b`),
		PassFail:    true,
		OnEmpty:     "docker build: ok",
		KeepOnError: true,
	},
}

// ruleFilters materializes the rule table as Filter values for the
// registry.
func ruleFilters() []Filter {
	fs := make([]Filter, len(ruleTable))
	for i, r := range ruleTable {
		fs[i] = ruleFilter{r}
	}
	return fs
}
