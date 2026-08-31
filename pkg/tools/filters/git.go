package filters

import (
	"strings"
)

// GitFilter compresses git's human output into compact one-line-per-item
// form: status becomes porcelain-style letters under a branch header, log
// collapses to short-hash + subject, a large diff/show becomes per-file
// insertion/deletion stats, and transport commands drop remote chatter.
// Any output that does not match the expected shape is returned unchanged —
// a filter must never destroy output it does not understand.
type GitFilter struct{}

func (GitFilter) Name() string { return "git" }

var gitSubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"push": true, "fetch": true, "pull": true,
}

func (GitFilter) Match(fields []string) bool {
	if len(fields) == 0 || progOf(fields[0]) != "git" {
		return false
	}
	sub, ok := subcommandAfter(fields, gitValueFlags)
	return ok && gitSubcommands[sub]
}

func (GitFilter) Filter(fields []string, raw string, exit int) string {
	if exit != 0 {
		return raw // a failed git command: the error is the whole point
	}
	sub, _ := subcommandAfter(fields, gitValueFlags)
	switch sub {
	case "status":
		return filterGitStatus(raw)
	case "log":
		return filterGitLog(raw)
	case "diff", "show":
		return filterGitDiff(raw, "git "+sub)
	default: // push, fetch, pull
		return filterGitRemote(raw)
	}
}

// --- status ---

var gitStatusPhrases = []struct{ phrase, letter string }{
	{"renamed:", "R"},
	{"copied:", "C"},
	{"modified:", "M"},
	{"new file:", "A"},
	{"deleted:", "D"},
}

func gitStatusLetter(entry string) (letter, phrase string, ok bool) {
	for _, m := range gitStatusPhrases {
		if strings.HasPrefix(entry, m.phrase) {
			return m.letter, m.phrase, true
		}
	}
	return "", "", false
}

// filterGitStatus converts human `git status` output into porcelain-style
// lines: a `## <branch>` header plus one `XY <path>` entry per change.
// Staged files get their letter in column one, unstaged in column two,
// untracked use `??`. Unrecognized output passes through untouched.
func filterGitStatus(raw string) string {
	lines := SplitLines(raw)
	branch := ""
	sawStructure := false
	section := "" // staged | unstaged | untracked
	var entries []string

	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "On branch "):
			branch = strings.TrimPrefix(t, "On branch ")
			sawStructure = true
		case strings.HasPrefix(t, "HEAD detached at "):
			branch = "(" + t + ")"
			sawStructure = true
		case strings.HasPrefix(t, "Changes to be committed:"):
			section, sawStructure = "staged", true
		case strings.HasPrefix(t, "Changes not staged for commit:"),
			strings.HasPrefix(t, "Unmerged paths:"):
			section, sawStructure = "unstaged", true
		case strings.HasPrefix(t, "Untracked files:"):
			section, sawStructure = "untracked", true
		case strings.HasPrefix(t, "nothing to commit"),
			strings.HasPrefix(t, "nothing added to commit"),
			strings.HasPrefix(t, "no changes added to commit"),
			strings.HasPrefix(t, "Your branch "),
			strings.HasPrefix(t, "No commits yet"),
			strings.HasPrefix(t, "(use "), t == "":
			// structural chrome — skip
		case strings.HasPrefix(ln, "\t") || (section == "untracked" && strings.HasPrefix(ln, "  ")):
			if t == "" {
				continue
			}
			if letter, phrase, ok := gitStatusLetter(t); ok {
				path := strings.TrimSpace(strings.TrimPrefix(t, phrase))
				if section == "staged" {
					entries = append(entries, letter+"  "+path)
				} else {
					entries = append(entries, " "+letter+" "+path)
				}
			} else if section == "untracked" {
				entries = append(entries, "?? "+t)
			}
		default:
			return raw // an unexpected shape: don't risk mangling it
		}
	}

	if !sawStructure || branch == "" {
		return raw
	}
	out := append([]string{"## " + branch}, entries...)
	return strings.Join(out, "\n")
}

// --- log ---

// filterGitLog collapses default-format `git log` blocks into one
// `<short-sha>[ (decoration)] <subject>` line per commit. --oneline, custom
// --pretty, and patch/stat mode (anything with a diff) pass through.
func filterGitLog(raw string) string {
	if strings.Contains(raw, "diff --git") {
		return raw
	}
	lines := SplitLines(raw)
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "commit ") {
		return raw
	}
	type entry struct{ sha, deco, subject string }
	var entries []entry
	cur := entry{}
	stage := 0 // 0 between, 1 headers, 2 body
	flush := func() {
		if cur.sha != "" && cur.subject != "" {
			entries = append(entries, cur)
		}
		cur = entry{}
	}
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if rest, ok := strings.CutPrefix(t, "commit "); ok && stage != 2 {
			flush()
			stage = 1
			sha, deco := rest, ""
			if i := strings.IndexByte(rest, '('); i >= 0 && strings.HasSuffix(rest, ")") {
				sha = strings.TrimSpace(rest[:i])
				deco = rest[i:]
			}
			if len(sha) > 7 {
				sha = sha[:7]
			}
			cur = entry{sha: sha, deco: deco}
			continue
		}
		switch stage {
		case 1:
			if t == "" {
				stage = 2
			}
		case 2:
			if t == "" {
				continue
			}
			cur.subject = t
			flush()
			stage = 0
		}
	}
	flush()
	if len(entries) == 0 {
		return raw
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.sha)
		if e.deco != "" {
			b.WriteString(" " + e.deco)
		}
		b.WriteString(" " + e.subject + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- diff / show ---

type diffFileStat struct {
	status             string
	inserted, deleted  int
	binary             bool
	oldPath, newPath   string
}

// filterGitDiff summarizes large unified diffs as per-file stats. Diffs of
// forty lines or fewer pass through untouched — exact context is cheap and
// beats a summary at that size. Output with no diff markers passes through.
func filterGitDiff(raw, label string) string {
	if !strings.Contains(raw, "diff --git ") {
		return raw
	}
	lines := SplitLines(raw)
	if len(lines) <= 40 {
		return raw
	}
	var stats []diffFileStat
	var cur *diffFileStat
	totalIns, totalDel := 0, 0
	flush := func() {
		if cur != nil {
			stats = append(stats, *cur)
			cur = nil
		}
	}
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "diff --git "):
			flush()
			a, b := parseDiffHeaderPaths(ln)
			cur = &diffFileStat{status: "M", oldPath: a, newPath: b}
		case cur == nil:
			continue
		case strings.HasPrefix(ln, "new file mode"):
			cur.status = "A"
		case strings.HasPrefix(ln, "deleted file mode"):
			cur.status = "D"
		case strings.HasPrefix(ln, "rename from "):
			cur.status, cur.oldPath = "R", strings.TrimPrefix(ln, "rename from ")
		case strings.HasPrefix(ln, "rename to "):
			cur.newPath = strings.TrimPrefix(ln, "rename to ")
		case strings.HasPrefix(ln, "copy from "):
			cur.status, cur.oldPath = "C", strings.TrimPrefix(ln, "copy from ")
		case strings.HasPrefix(ln, "copy to "):
			cur.newPath = strings.TrimPrefix(ln, "copy to ")
		case strings.HasPrefix(ln, "Binary files "), strings.HasPrefix(ln, "GIT binary patch"):
			cur.binary = true
		case strings.HasPrefix(ln, "+++ "), strings.HasPrefix(ln, "--- "), strings.HasPrefix(ln, "@@"):
			// headers / hunk markers — no count
		case strings.HasPrefix(ln, "+"):
			cur.inserted++
			totalIns++
		case strings.HasPrefix(ln, "-"):
			cur.deleted++
			totalDel++
		}
	}
	flush()
	if len(stats) == 0 {
		return raw
	}
	var b strings.Builder
	b.WriteString(label + ": " + itoa(len(stats)) + " files changed, +" + itoa(totalIns) + " -" + itoa(totalDel) + "\n")
	for _, st := range stats {
		name := st.newPath
		switch st.status {
		case "R", "C":
			name = st.oldPath + " -> " + st.newPath
		case "D":
			name = st.oldPath
		}
		detail := "+" + itoa(st.inserted) + " -" + itoa(st.deleted)
		if st.binary {
			detail = "binary"
		}
		b.WriteString(st.status + " " + name + " | " + detail + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func parseDiffHeaderPaths(header string) (a, b string) {
	rest := strings.TrimSpace(strings.TrimPrefix(header, "diff --git "))
	mid := strings.Index(rest, " b/")
	if mid < 0 {
		return strings.TrimPrefix(rest, "a/"), ""
	}
	return strings.TrimPrefix(rest[:mid], "a/"), strings.TrimPrefix(rest[mid+1:], "b/")
}

// --- push / fetch / pull ---

func filterGitRemote(raw string) string {
	lines := SplitLines(raw)
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "remote: "),
			strings.HasPrefix(t, "To "),
			strings.HasPrefix(t, "From "),
			strings.HasSuffix(t, "[new branch]"),
			strings.Contains(t, "[new tag]"),
			strings.Contains(t, "[up to date]"):
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
