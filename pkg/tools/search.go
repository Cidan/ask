package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const GlobToolDescription = `Find files by glob pattern, relative to the search path. Supports ** for crossing directories and {a,b} alternation (e.g. "**/*.go", "src/**/*.{ts,tsx}"). Results are sorted by modification time, newest first.`

type GlobParams struct {
	Pattern     string `json:"pattern" jsonschema:"glob pattern matched against paths relative to the search directory"`
	Path        string `json:"path,omitempty" jsonschema:"directory to search (default: working directory)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// GlobTool returns the native glob tool.
// GlobResult is the glob tool's response.
type GlobResult struct {
	Listing   string `json:"listing,omitempty" jsonschema:"matching file paths, one per line"`
	Matches   int    `json:"matches,omitempty" jsonschema:"number of files matched"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"true when more files matched than were listed"`
}

func GlobTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"glob",
		GlobToolDescription,
		func(ctx agent.Context, p GlobParams) (GlobResult, error) {
			if strings.TrimSpace(p.Pattern) == "" {
				return GlobResult{}, errors.New("pattern is required")
			}
			root := env.AbsPath(p.Path)
			type hit struct {
				rel string
				mod int64
			}
			var hits []hit
			truncated := false
			err := filepath.WalkDir(root, func(fp string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil // unreadable entries are skipped
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				rel, err := filepath.Rel(root, fp)
				if err != nil {
					return nil
				}
				rel = filepath.ToSlash(rel)
				if !GlobMatch(p.Pattern, rel) {
					return ctx.Err()
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				hits = append(hits, hit{rel, info.ModTime().UnixNano()})
				if len(hits) > MaxSearchHits*4 {
					truncated = true
					return filepath.SkipAll
				}
				return ctx.Err()
			})
			if err != nil && err != filepath.SkipAll {
				return GlobResult{}, errors.New("glob walk: " + err.Error())
			}
			if len(hits) == 0 {
				return GlobResult{Listing: "no files match " + p.Pattern + " under " + root}, nil
			}
			sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })
			if len(hits) > MaxSearchHits {
				hits = hits[:MaxSearchHits]
				truncated = true
			}
			var out strings.Builder
			for _, h := range hits {
				out.WriteString(h.rel)
				out.WriteByte('\n')
			}
			if truncated {
				fmt.Fprintf(&out, "(capped at %d results — narrow the pattern for more)\n", MaxSearchHits)
			}
			return GlobResult{Listing: out.String(), Matches: len(hits), Truncated: truncated}, nil
		},
	)
}

// GlobMatch matches a slash-separated relative path against a doublestar pattern.
func GlobMatch(pattern, rel string) bool {
	for _, pat := range ExpandBraces(pattern) {
		if globSegMatch(strings.Split(pat, "/"), strings.Split(rel, "/")) {
			return true
		}
	}
	return false
}

func globSegMatch(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if globSegMatch(pat[1:], segs) {
			return true
		}
		return len(segs) > 0 && globSegMatch(pat, segs[1:])
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return globSegMatch(pat[1:], segs[1:])
}

// ExpandBraces expands one level of {a,b,c} alternation.
func ExpandBraces(s string) []string {
	open := strings.IndexByte(s, '{')
	if open < 0 {
		return []string{s}
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var out []string
				for _, alt := range splitBraceAlts(s[open+1 : i]) {
					out = append(out, ExpandBraces(s[:open]+alt+s[i+1:])...)
				}
				return out
			}
		}
	}
	return []string{s}
}

func splitBraceAlts(s string) []string {
	var alts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				alts = append(alts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(alts, s[start:])
}

const GrepToolDescription = `Search file contents with regular expressions. Returns matching lines grouped by file, newest files first, capped at 100 matches per pattern. Pass several patterns at once in patterns — each is reported under its own "==> grep" heading — instead of making one call per pattern, and set context to get the lines around each match (marked "Line N-") so you rarely need a follow-up read. Set literal_text for exact-string search; use include to filter files (e.g. "*.go", "*.{ts,tsx}"). Uses ripgrep when available (respects .gitignore). To read files in the same call, use read with its grep field. Use this tool, never grep, rg, or find piped through bash.`

// MaxGrepContext caps the context lines around each match.
const MaxGrepContext = 20

// GrepSearch is one grep request: the grep tool's parameters, and one entry
// of the read tool's grep list.
type GrepSearch struct {
	Pattern     string   `json:"pattern,omitempty" jsonschema:"regular expression to search for (exact string when literal_text is set)"`
	Patterns    []string `json:"patterns,omitempty" jsonschema:"several patterns to search in this one call, each reported under its own heading"`
	Path        string   `json:"path,omitempty" jsonschema:"directory or file to search (default: working directory)"`
	Include     string   `json:"include,omitempty" jsonschema:"only search files matching this glob, e.g. *.go or *.{ts,tsx}"`
	LiteralText bool     `json:"literal_text,omitempty" jsonschema:"treat the patterns as literal strings instead of regexps"`
	Context     int      `json:"context,omitempty" jsonschema:"lines to show before and after each match (default 0, max 20)"`
}

// AllPatterns returns pattern followed by patterns, without blanks or
// repeats.
func (s GrepSearch) AllPatterns() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range append([]string{s.Pattern}, s.Patterns...) {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

type GrepParams struct {
	GrepSearch
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

var RgPath, _ = exec.LookPath("rg")

// GrepTool returns the native grep tool.
// GrepResult is the grep tool's response.
type GrepResult struct {
	Listing string `json:"listing,omitempty" jsonschema:"matching lines, prefixed with file and line number"`
}

func GrepTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"grep",
		GrepToolDescription,
		func(ctx agent.Context, p GrepParams) (GrepResult, error) {
			if len(p.AllPatterns()) == 0 {
				return GrepResult{}, errors.New("pattern or patterns is required")
			}
			out, errText := GrepRun(ctx, RgPath, p.GrepSearch, env.AbsPath(p.Path))
			if errText != "" {
				return GrepResult{}, errors.New(errText)
			}
			return GrepResult{Listing: out}, nil
		},
	)
}

type GrepMatch struct {
	File string
	Line int
	Text string
	// Context marks a line shown around a match rather than a match.
	Context bool
}

const MaxGrepLineChars = 500

// GrepRun runs every pattern of s under root. One pattern renders bare; each
// of several renders under its own heading, and a pattern that fails
// reports its error there without failing the others. The error string is
// set only when every pattern failed.
func GrepRun(ctx context.Context, rgPath string, s GrepSearch, root string) (string, string) {
	patterns := s.AllPatterns()
	if len(patterns) == 1 {
		return grepPattern(ctx, rgPath, patterns[0], s, root)
	}
	var out strings.Builder
	var errs []string
	for i, pat := range patterns {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(GrepHeading(pat, s))
		body, errText := grepPattern(ctx, rgPath, pat, s, root)
		if errText != "" {
			errs = append(errs, errText)
			body = "error: " + errText
		}
		out.WriteString(strings.TrimRight(body, "\n") + "\n")
	}
	if len(errs) == len(patterns) {
		return "", strings.Join(errs, "; ")
	}
	return out.String(), ""
}

// GrepHeading is the section heading for one pattern of a batched search.
func GrepHeading(pattern string, s GrepSearch) string {
	return "==> " + grepLabel(pattern, s) + " <==\n"
}

func grepLabel(pattern string, s GrepSearch) string {
	h := "grep " + strconv.Quote(pattern)
	if s.Path != "" {
		h += " in " + s.Path
	}
	if s.Include != "" {
		h += " (" + s.Include + ")"
	}
	return h
}

func grepPattern(ctx context.Context, rgPath, pattern string, s GrepSearch, root string) (string, string) {
	around := min(max(s.Context, 0), MaxGrepContext)
	var matches []GrepMatch
	var errText string
	if rgPath != "" {
		matches, errText = grepWithRg(ctx, rgPath, pattern, s, around, root)
	} else {
		matches, errText = grepWithGo(ctx, pattern, s, around, root)
	}
	if errText != "" {
		return "", errText
	}
	if len(matches) == 0 {
		return "no matches found for " + pattern + "\n", ""
	}
	matches, truncated := capGrepMatches(matches, around)

	byFile := map[string][]GrepMatch{}
	var files []string
	for _, m := range matches {
		if _, seen := byFile[m.File]; !seen {
			files = append(files, m.File)
		}
		byFile[m.File] = append(byFile[m.File], m)
	}
	mtime := map[string]int64{}
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			mtime[f] = info.ModTime().UnixNano()
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return mtime[files[i]] > mtime[files[j]] })

	var out strings.Builder
	for _, f := range files {
		out.WriteString(f)
		out.WriteByte('\n')
		prev := 0
		for _, m := range byFile[f] {
			if around > 0 && prev > 0 && m.Line > prev+1 {
				out.WriteString("  --\n")
			}
			prev = m.Line
			text := m.Text
			if len(text) > MaxGrepLineChars {
				text = text[:MaxGrepLineChars] + "…"
			}
			sep := ":"
			if m.Context {
				sep = "-"
			}
			fmt.Fprintf(&out, "  Line %d%s %s\n", m.Line, sep, text)
		}
		out.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&out, "(capped at %d matches — narrow the pattern or path for more)\n", MaxSearchHits)
	}
	return out.String(), ""
}

// capGrepMatches keeps the first MaxSearchHits matches and the context
// lines that belong to them, reporting whether any match was dropped.
func capGrepMatches(lines []GrepMatch, around int) ([]GrepMatch, bool) {
	seen := 0
	for i, m := range lines {
		if m.Context {
			continue
		}
		seen++
		if seen <= MaxSearchHits {
			continue
		}
		kept := lines[:i]
		var last GrepMatch
		for j := len(kept) - 1; j >= 0; j-- {
			if !kept[j].Context {
				last = kept[j]
				break
			}
		}
		for len(kept) > 0 {
			t := kept[len(kept)-1]
			if !t.Context || (t.File == last.File && t.Line <= last.Line+around) {
				break
			}
			kept = kept[:len(kept)-1]
		}
		return kept, true
	}
	return lines, false
}

func grepWithRg(ctx context.Context, rgPath, pattern string, s GrepSearch, around int, root string) ([]GrepMatch, string) {
	args := []string{"-n", "--no-heading", "--color=never", "--null"}
	if s.LiteralText {
		args = append(args, "-F")
	}
	if s.Include != "" {
		args = append(args, "--glob", s.Include)
	}
	if around > 0 {
		args = append(args, "-C", strconv.Itoa(around))
	}
	args = append(args, "-e", pattern, "--", root)
	cmd := exec.CommandContext(ctx, rgPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "rg: " + err.Error()
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, "rg: " + err.Error()
	}
	var lines []GrepMatch
	found := 0
	capped := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		m, ok := parseRgLine(sc.Text())
		if !ok {
			continue
		}
		if !m.Context {
			found++
			if found > MaxSearchHits+1 {
				capped = true
				_ = cmd.Process.Kill()
				break
			}
		}
		lines = append(lines, m)
	}
	err = cmd.Wait()
	if err != nil && !capped {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, ""
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, "rg: " + detail
	}
	return lines, ""
}

// parseRgLine reads one line of `rg -n --null` output: the path, a NUL, the
// line number, then ':' before a match or '-' before a context line. The
// "--" lines between context groups carry no NUL and are not lines.
func parseRgLine(line string) (GrepMatch, bool) {
	nul := strings.IndexByte(line, 0)
	if nul < 0 {
		return GrepMatch{}, false
	}
	rest := line[nul+1:]
	sep := strings.IndexAny(rest, ":-")
	if sep < 0 {
		return GrepMatch{}, false
	}
	n, err := strconv.Atoi(rest[:sep])
	if err != nil {
		return GrepMatch{}, false
	}
	return GrepMatch{File: line[:nul], Line: n, Text: rest[sep+1:], Context: rest[sep] == '-'}, true
}

func grepWithGo(ctx context.Context, pattern string, s GrepSearch, around int, root string) ([]GrepMatch, string) {
	if s.LiteralText {
		pattern = regexp.QuoteMeta(pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, "invalid pattern: " + err.Error()
	}
	var lines []GrepMatch
	found := 0
	walkErr := filepath.WalkDir(root, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return ctx.Err()
		}
		if s.Include != "" {
			rel, relErr := filepath.Rel(root, fp)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if !GlobMatch(s.Include, rel) && !GlobMatch(s.Include, path.Base(rel)) {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			return nil
		}
		if LooksBinary(data[:min(len(data), 8192)]) {
			return nil
		}
		text := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		next := 0
		for i, line := range text {
			if !re.MatchString(line) {
				continue
			}
			for c := max(next, i-around); c < i; c++ {
				lines = append(lines, GrepMatch{File: fp, Line: c + 1, Text: text[c], Context: true})
			}
			lines = append(lines, GrepMatch{File: fp, Line: i + 1, Text: line})
			next = i + 1
			found++
			if found > MaxSearchHits {
				return filepath.SkipAll
			}
			for c := i + 1; c < min(i+1+around, len(text)); c++ {
				if re.MatchString(text[c]) {
					break
				}
				lines = append(lines, GrepMatch{File: fp, Line: c + 1, Text: text[c], Context: true})
				next = c + 1
			}
		}
		return ctx.Err()
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return nil, "grep walk: " + walkErr.Error()
	}
	return lines, ""
}

const LsToolDescription = `List a directory as a tree. Directories end with /. Use depth to limit recursion; output is capped at 1000 entries.`

type LsParams struct {
	Path        string `json:"path,omitempty" jsonschema:"directory to list (default: working directory)"`
	Depth       int    `json:"depth,omitempty" jsonschema:"maximum directory depth to descend (0 = unlimited)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// LsTool returns the native ls tool.
// LsResult is the ls tool's response.
type LsResult struct {
	Listing string `json:"listing,omitempty" jsonschema:"directory tree"`
}

func LsTool(env *ToolEnv) Tool {
	return NewTypedTool(
		"ls",
		LsToolDescription,
		func(ctx agent.Context, p LsParams) (LsResult, error) {
			root := env.AbsPath(p.Path)
			info, err := os.Stat(root)
			if err != nil {
				return LsResult{}, errors.New("stat " + root + ": " + err.Error())
			}
			if !info.IsDir() {
				return LsResult{}, errors.New(root + " is not a directory")
			}
			var out strings.Builder
			out.WriteString(root + "/\n")
			entries := 0
			truncated := false
			err = filepath.WalkDir(root, func(fp string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if fp == root {
					return nil
				}
				rel, relErr := filepath.Rel(root, fp)
				if relErr != nil {
					return nil
				}
				rel = filepath.ToSlash(rel)
				depth := strings.Count(rel, "/") + 1
				if d.IsDir() && d.Name() == ".git" {
					return filepath.SkipDir
				}
				if p.Depth > 0 && depth > p.Depth {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if entries >= MaxListEntries {
					truncated = true
					return filepath.SkipAll
				}
				indent := strings.Repeat("  ", depth)
				name := d.Name()
				if d.IsDir() {
					name += "/"
				}
				out.WriteString(indent + name + "\n")
				entries++
				return ctx.Err()
			})
			if err != nil && err != filepath.SkipAll {
				return LsResult{}, errors.New("ls walk: " + err.Error())
			}
			if truncated {
				fmt.Fprintf(&out, "(capped at %d entries — use depth or a narrower path)\n", MaxListEntries)
			}
			return LsResult{Listing: out.String()}, nil
		},
	)
}
