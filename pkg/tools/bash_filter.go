package tools

import "github.com/Cidan/ask/pkg/tools/filters"

// ExtractBaseCommand parses the executable plus the first argument that
// identifies the tool action ("go test", "git push"), for use as the
// savings-ledger key. It re-exports filters.BaseCommand so the ledger's
// keys and the filter registry always agree on what "the command" is.
func ExtractBaseCommand(command string) string {
	fields := filters.BaseCommand(command)
	if len(fields) == 0 {
		return ""
	}
	base := fields[0]
	if len(fields) > 1 {
		switch base {
		case "go", "npm", "git", "yarn", "cargo", "pnpm", "bun", "pip", "uv":
			return base + " " + fields[1]
		}
	}
	return base
}

// ApplyBashFilter compresses command output to save tokens. It dispatches
// through the command-aware semantic filter registry (pkg/tools/filters)
// and falls back to the universal squeeze/dedup/cap pipeline for anything
// unmodeled. exit steers verbosity: a successful run may collapse to a
// summary while a failing run keeps its detail.
func ApplyBashFilter(command, rawOutput string, exit int) (string, int) {
	return filters.Apply(command, rawOutput, exit)
}
