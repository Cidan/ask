package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

const agentGlobToolDescription = `Find files by glob pattern, relative to the search path. Supports ** for crossing directories and {a,b} alternation (e.g. "**/*.go", "src/**/*.{ts,tsx}"). Results are sorted by modification time, newest first.`

type agentGlobParams struct {
	Pattern     string `json:"pattern" description:"glob pattern matched against paths relative to the search directory"`
	Path        string `json:"path,omitempty" description:"directory to search (default: working directory)"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentGlobTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"glob",
		agentGlobToolDescription,
		func(ctx context.Context, p agentGlobParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(p.Pattern) == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}
			root := env.absPath(p.Path)
			type hit struct {
				rel string
				mod int64
			}
			var hits []hit
			truncated := false
			err := filepath.WalkDir(root, func(fp string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil //nolint:nilerr // unreadable entries are skipped, not fatal
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				rel, err := filepath.Rel(root, fp)
				if err != nil {
					return nil //nolint:nilerr
				}
				rel = filepath.ToSlash(rel)
				if !agentGlobMatch(p.Pattern, rel) {
					return ctx.Err()
				}
				info, err := d.Info()
				if err != nil {
					return nil //nolint:nilerr
				}
				hits = append(hits, hit{rel, info.ModTime().UnixNano()})
				if len(hits) > agentMaxSearchHits*4 {
					truncated = true
					return filepath.SkipAll
				}
				return ctx.Err()
			})
			if err != nil && err != filepath.SkipAll {
				return fantasy.NewTextErrorResponse("glob walk: " + err.Error()), nil
			}
			if len(hits) == 0 {
				return fantasy.NewTextResponse("no files match " + p.Pattern + " under " + root), nil
			}
			sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })
			if len(hits) > agentMaxSearchHits {
				hits = hits[:agentMaxSearchHits]
				truncated = true
			}
			var out strings.Builder
			for _, h := range hits {
				out.WriteString(h.rel)
				out.WriteByte('\n')
			}
			if truncated {
				fmt.Fprintf(&out, "(capped at %d results — narrow the pattern for more)\n", agentMaxSearchHits)
			}
			return fantasy.NewTextResponse(out.String()), nil
		},
	)
}

// agentGlobMatch matches a slash-separated relative path against a
// doublestar-style pattern: ** crosses directory boundaries, single
// segments use path.Match syntax, and {a,b} alternation is expanded.
func agentGlobMatch(pattern, rel string) bool {
	for _, pat := range expandBraces(pattern) {
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

// expandBraces expands one level of {a,b,c} alternation. Nested braces
// expand recursively; a pattern without braces returns as-is.
func expandBraces(s string) []string {
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
					out = append(out, expandBraces(s[:open]+alt+s[i+1:])...)
				}
				return out
			}
		}
	}
	return []string{s} // unbalanced — treat literally
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

const agentGrepToolDescription = `Search file contents with a regular expression. Returns matching lines grouped by file, newest files first, capped at 100 matches. Set literal_text for exact-string search; use include to filter files (e.g. "*.go", "*.{ts,tsx}"). Uses ripgrep when available (respects .gitignore).`

type agentGrepParams struct {
	Pattern     string `json:"pattern" description:"regular expression to search for (exact string when literal_text is set)"`
	Path        string `json:"path,omitempty" description:"directory or file to search (default: working directory)"`
	Include     string `json:"include,omitempty" description:"only search files matching this glob, e.g. *.go or *.{ts,tsx}"`
	LiteralText bool   `json:"literal_text,omitempty" description:"treat pattern as a literal string instead of a regexp"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// agentRgPath is resolved once; tests force the pure-Go fallback by
// calling agentGrepRun with rgPath="".
var agentRgPath, _ = exec.LookPath("rg")

func agentGrepTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"grep",
		agentGrepToolDescription,
		func(ctx context.Context, p agentGrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if p.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}
			out, errText := agentGrepRun(ctx, agentRgPath, p, env.absPath(p.Path))
			if errText != "" {
				return fantasy.NewTextErrorResponse(errText), nil
			}
			return fantasy.NewTextResponse(out), nil
		},
	)
}

type grepMatch struct {
	file string
	line int
	text string
}

const agentMaxGrepLineChars = 500

// agentGrepRun executes the search and renders the grouped output.
// Returns (output, "") on success or ("", errorText) on failure.
func agentGrepRun(ctx context.Context, rgPath string, p agentGrepParams, root string) (string, string) {
	var matches []grepMatch
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
	if len(matches) > agentMaxSearchHits {
		matches = matches[:agentMaxSearchHits]
		truncated = true
	}

	// Group by file, files ordered by modification time (newest first).
	byFile := map[string][]grepMatch{}
	var files []string
	for _, m := range matches {
		if _, seen := byFile[m.file]; !seen {
			files = append(files, m.file)
		}
		byFile[m.file] = append(byFile[m.file], m)
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
			text := m.text
			if len(text) > agentMaxGrepLineChars {
				text = text[:agentMaxGrepLineChars] + "…"
			}
			fmt.Fprintf(&out, "  Line %d: %s\n", m.line, text)
		}
		out.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&out, "(capped at %d matches — narrow the pattern or path for more)\n", agentMaxSearchHits)
	}
	return out.String(), ""
}

func grepWithRg(ctx context.Context, rgPath string, p agentGrepParams, root string) ([]grepMatch, string) {
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
	var matches []grepMatch
	capped := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(matches) > agentMaxSearchHits {
			// Hit the cap: stop rg rather than draining a huge result set.
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
		matches = append(matches, grepMatch{file: file, line: n, text: rest[colon+1:]})
	}
	err = cmd.Wait()
	if err != nil && !capped {
		var exit *exec.ExitError
		// rg exits 1 on "no matches" — a result, not an error.
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

func grepWithGo(ctx context.Context, p agentGrepParams, root string) ([]grepMatch, string) {
	pattern := p.Pattern
	if p.LiteralText {
		pattern = regexp.QuoteMeta(pattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, "invalid pattern: " + err.Error()
	}
	var matches []grepMatch
	walkErr := filepath.WalkDir(root, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr
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
				return nil //nolint:nilerr
			}
			rel = filepath.ToSlash(rel)
			if !agentGlobMatch(p.Include, rel) && !agentGlobMatch(p.Include, path.Base(rel)) {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			return nil //nolint:nilerr
		}
		if looksBinary(data[:min(len(data), 8192)]) {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, grepMatch{file: fp, line: i + 1, text: line})
				if len(matches) > agentMaxSearchHits {
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

const agentLsToolDescription = `List a directory as a tree. Directories end with /. Use depth to limit recursion; output is capped at 1000 entries.`

type agentLsParams struct {
	Path        string `json:"path,omitempty" description:"directory to list (default: working directory)"`
	Depth       int    `json:"depth,omitempty" description:"maximum directory depth to descend (0 = unlimited)"`
	Description string `json:"description" description:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

func agentLsTool(env *agentToolEnv) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"ls",
		agentLsToolDescription,
		func(ctx context.Context, p agentLsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			root := env.absPath(p.Path)
			info, err := os.Stat(root)
			if err != nil {
				return fantasy.NewTextErrorResponse("stat " + root + ": " + err.Error()), nil
			}
			if !info.IsDir() {
				return fantasy.NewTextErrorResponse(root + " is not a directory"), nil
			}
			var out strings.Builder
			out.WriteString(root + "/\n")
			entries := 0
			truncated := false
			err = filepath.WalkDir(root, func(fp string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil //nolint:nilerr
				}
				if fp == root {
					return nil
				}
				rel, relErr := filepath.Rel(root, fp)
				if relErr != nil {
					return nil //nolint:nilerr
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
				if entries >= agentMaxListEntries {
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
				return fantasy.NewTextErrorResponse("ls walk: " + err.Error()), nil
			}
			if truncated {
				fmt.Fprintf(&out, "(capped at %d entries — use depth or a narrower path)\n", agentMaxListEntries)
			}
			return fantasy.NewTextResponse(out.String()), nil
		},
	)
}
