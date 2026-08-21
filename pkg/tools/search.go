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

const GrepToolDescription = `Search file contents with a regular expression. Returns matching lines grouped by file, newest files first, capped at 100 matches. Set literal_text for exact-string search; use include to filter files (e.g. "*.go", "*.{ts,tsx}"). Uses ripgrep when available (respects .gitignore).`

type GrepParams struct {
	Pattern     string `json:"pattern" jsonschema:"regular expression to search for (exact string when literal_text is set)"`
	Path        string `json:"path,omitempty" jsonschema:"directory or file to search (default: working directory)"`
	Include     string `json:"include,omitempty" jsonschema:"only search files matching this glob, e.g. *.go or *.{ts,tsx}"`
	LiteralText bool   `json:"literal_text,omitempty" jsonschema:"treat pattern as a literal string instead of a regexp"`
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
			if p.Pattern == "" {
				return GrepResult{}, errors.New("pattern is required")
			}
			out, errText := GrepRun(ctx, RgPath, p, env.AbsPath(p.Path))
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
}

const MaxGrepLineChars = 500

// GrepRun executes the search and renders the grouped output.
func GrepRun(ctx context.Context, rgPath string, p GrepParams, root string) (string, string) {
	var matches []GrepMatch
	var errText string
	if rgPath != "" {
		matches, errText = grepWithRg(ctx, rgPath, p, root)
	} else {
		matches, errText = grepWithGo(ctx, p, root)
	}
	if errText != "" {
		return "", errText
	}
	if len(matches) == 0 {
		return "no matches found for " + p.Pattern, ""
	}
	truncated := false
	if len(matches) > MaxSearchHits {
		matches = matches[:MaxSearchHits]
		truncated = true
	}

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
		for _, m := range byFile[f] {
			text := m.Text
			if len(text) > MaxGrepLineChars {
				text = text[:MaxGrepLineChars] + "…"
			}
			fmt.Fprintf(&out, "  Line %d: %s\n", m.Line, text)
		}
		out.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&out, "(capped at %d matches — narrow the pattern or path for more)\n", MaxSearchHits)
	}
	return out.String(), ""
}

func grepWithRg(ctx context.Context, rgPath string, p GrepParams, root string) ([]GrepMatch, string) {
	args := []string{"-n", "--no-heading", "--color=never", "--null"}
	if p.LiteralText {
		args = append(args, "-F")
	}
	if p.Include != "" {
		args = append(args, "--glob", p.Include)
	}
	args = append(args, "-e", p.Pattern, "--", root)
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
	var matches []GrepMatch
	capped := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(matches) > MaxSearchHits {
			capped = true
			_ = cmd.Process.Kill()
			break
		}
		line := sc.Text()
		nul := strings.IndexByte(line, 0)
		if nul < 0 {
			continue
		}
		file := line[:nul]
		rest := line[nul+1:]
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			continue
		}
		n, err := strconv.Atoi(rest[:colon])
		if err != nil {
			continue
		}
		matches = append(matches, GrepMatch{File: file, Line: n, Text: rest[colon+1:]})
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
	return matches, ""
}

func grepWithGo(ctx context.Context, p GrepParams, root string) ([]GrepMatch, string) {
	pattern := p.Pattern
	if p.LiteralText {
		pattern = regexp.QuoteMeta(pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, "invalid pattern: " + err.Error()
	}
	var matches []GrepMatch
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
		if p.Include != "" {
			rel, relErr := filepath.Rel(root, fp)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if !GlobMatch(p.Include, rel) && !GlobMatch(p.Include, path.Base(rel)) {
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
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, GrepMatch{File: fp, Line: i + 1, Text: line})
				if len(matches) > MaxSearchHits {
					return filepath.SkipAll
				}
			}
		}
		return ctx.Err()
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return nil, "grep walk: " + walkErr.Error()
	}
	return matches, ""
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
