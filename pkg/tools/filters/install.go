package filters

import "strings"

// InstallFilter strips the noise from JavaScript package-manager installs
// (npm, yarn, pnpm, bun): deprecation warnings, funding/audit footers, and
// progress lines. The final "added N packages" style result line is kept.
// A failed install keeps everything, since the error text is what matters.
type InstallFilter struct{}

func (InstallFilter) Name() string { return "install" }

var installManagers = map[string]bool{"npm": true, "yarn": true, "pnpm": true, "bun": true}
var installVerbs = map[string]bool{"install": true, "i": true, "ci": true, "add": true}

func (InstallFilter) Match(fields []string) bool {
	// Only explicit install verbs — never a bare `yarn build` or `pnpm
	// <script>`, whose warnings must not be stripped as install noise.
	return len(fields) >= 2 && installManagers[progOf(fields[0])] && installVerbs[fields[1]]
}

func (InstallFilter) Filter(fields []string, raw string, exit int) string {
	if exit != 0 {
		return raw
	}
	lines := SplitLines(raw)
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "npm WARN"),
			strings.HasPrefix(t, "npm warn"),
			strings.HasPrefix(t, "warning "),
			strings.HasPrefix(t, "npm notice"),
			strings.Contains(t, "deprecated"),
			strings.HasPrefix(t, "found 0 vulnerabilities"),
			strings.HasSuffix(t, "looking for funding"),
			strings.HasPrefix(t, "run `npm fund`"):
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
