package tools

import "github.com/Cidan/ask/pkg/tools/filters"

// ExtractBaseCommand returns the savings-ledger key for a command: its
// primary program plus, for subcommand-style tools, the subcommand ("go
// test", "git diff", "kubectl get"). It delegates to filters.LedgerKey so
// the ledger's keys and the filter registry always agree on what "the
// command" is — global flags (`git --no-pager diff`) and pipelines (`go
// test ./... | tail`) resolve to the same key the filter dispatches on.
func ExtractBaseCommand(command string) string {
	return filters.LedgerKey(command)
}

// IsTrivialCommand reports whether a command is not a meaningful token-
// savings opportunity — its primary program is a pager, text transformer,
// or a tiny builtin (cat, grep, head, cd, echo, …), or there is no command
// at all. The bash tool skips these when recording savings so the ledger
// reflects real build/test/tooling commands rather than file reads and
// shell plumbing. It delegates to filters.IsTrivial.
func IsTrivialCommand(command string) bool {
	return filters.IsTrivial(command)
}

// ApplyBashFilter compresses command output to save tokens. It dispatches
// through the command-aware semantic filter registry (pkg/tools/filters)
// and falls back to the universal squeeze/dedup/cap pipeline for anything
// unmodeled. exit steers verbosity: a successful run may collapse to a
// summary while a failing run keeps its detail.
func ApplyBashFilter(command, rawOutput string, exit int) (string, int) {
	return filters.Apply(command, rawOutput, exit)
}
