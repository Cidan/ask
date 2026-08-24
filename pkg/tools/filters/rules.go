package filters

import "regexp"

// re compiles a rule pattern at package load; a bad pattern is a
// programming error and should panic the process, never silently skip.
func re(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// ruleTable is the declarative long-tail of command filters — ask's own
// take on RTK's src/filters/*.toml. Every rule here is strip-only (it
// removes progress/bookkeeping noise, never error lines), so failures pass
// through intact without needing KeepOnError; the exceptions set it
// explicitly. Complex aggregators (go test, git, pytest) are hand-written
// filters, not rules, and are registered ahead of these.
var ruleTable = []Rule{
	{
		Name:    "make",
		Command: re(`^(make|gmake)\b`),
		Strip: []*regexp.Regexp{
			re(`^(make|gmake)\[\d+\]:`),          // Entering/Leaving directory
			re(`^(make|gmake): Nothing to be done`),
			re(`^(make|gmake): '.*' is up to date`),
		},
		OnEmpty: "make: nothing to do",
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
		// cargo indents its status words (`   Compiling foo`); require the
		// leading whitespace so a program's own "Compiling…" at column zero
		// (under `cargo run`) is never mistaken for cargo noise.
		Name:    "cargo",
		Command: re(`^cargo\b`),
		Strip: []*regexp.Regexp{
			re(`^\s+Compiling `),
			re(`^\s+Updating `),
			re(`^\s+Downloading `),
			re(`^\s+Downloaded `),
			re(`^\s+Blocking `),
			re(`^\s+Locking `),
			re(`^\s+Adding `),
			re(`^\s+Installing `),
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
		Name:    "gradle",
		Command: re(`^(gradle|gradlew)\b`),
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
		// cmake configure prints a long run of `-- Detecting …` / `-- Check …`
		// probe lines; strip them, keep warnings/errors and the summary.
		Name:    "cmake",
		Command: re(`^cmake\b`),
		Strip:   []*regexp.Regexp{re(`^-- `)},
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
		// docker build's BuildKit output interleaves real step logs
		// (`#N 12.3 <output>`) with bookkeeping (`#N DONE`, `#N CACHED`,
		// layer sha256 lines). Strip only the bookkeeping; keep the logs.
		// A failed build keeps everything.
		Name:        "docker-build",
		Command:     re(`^docker\s+(build|buildx\s+build)\b`),
		KeepOnError: true,
		Strip: []*regexp.Regexp{
			re(`^#\d+ (DONE|CACHED|extracting|transferring|resolve|naming|exporting|writing|preparing|sealing|load )`),
			re(`^#\d+ sha256:`),
			re(`^#\d+ \[internal\]`),
		},
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
