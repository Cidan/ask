package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var pathPickerPrefixes = []string{"cd ", "ls ", "/add-dir "}

func bareCommand(line string) string {
	for _, p := range pathPickerPrefixes {
		cmd := strings.TrimSpace(p)
		if line == cmd {
			return cmd
		}
	}
	return ""
}

func (m model) pathPickerCmd() string {
	val := m.input.Value()
	if strings.Contains(val, "\n") {
		return ""
	}
	for _, p := range pathPickerPrefixes {
		if strings.HasPrefix(val, p) {
			return strings.TrimSpace(p)
		}
	}
	return ""
}

func (m model) pathPickerActive() bool {
	return m.pathPickerCmd() != ""
}

func (m model) pathQuery() string {
	val := m.input.Value()
	for _, p := range pathPickerPrefixes {
		if strings.HasPrefix(val, p) {
			return strings.TrimPrefix(val, p)
		}
	}
	return ""
}

func (m *model) refreshPathMatches() {
	if !m.pathPickerActive() {
		m.pathMatches = nil
		m.pathIdx = 0
		return
	}
	matches := pathComplete(m.pathQuery())
	m.pathMatches = matches
	if m.pathIdx >= len(matches) {
		m.pathIdx = 0
	}
}

// pathMatch is one completion offered while a path is typed: the path as it
// would be typed, whether it is a directory to descend into, and where a
// symlink points.
type pathMatch struct {
	path string
	dir  bool
	dest string
}

// dirCompletion is one entry completeDirEntries found: the path spelled the
// way the query was typed, where it is on disk, and the entry itself.
type dirCompletion struct {
	path   string
	onDisk string
	entry  fs.DirEntry
}

// completeDirEntries lists the entries of query's directory whose names
// start with query's last element (case-insensitively; hidden ones only
// when that element starts with '.') and that keep accepts. Paths are
// spelled the way query is: under ~ when it starts with ~, relative when it
// is relative — read from base, or the working directory when base is "".
func completeDirEntries(query, base string, keep func(fs.DirEntry) bool) ([]dirCompletion, error) {
	expanded, tildeStripped := expandTildeDir(query)
	dir, prefix := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	readDir := dir
	if base != "" && !filepath.IsAbs(dir) {
		readDir = filepath.Join(base, dir)
	}
	entries, err := os.ReadDir(readDir)
	if err != nil {
		return nil, err
	}
	showHidden := strings.HasPrefix(prefix, ".")
	prefixLower := strings.ToLower(prefix)
	var out []dirCompletion
	for _, e := range entries {
		if !keep(e) {
			continue
		}
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(name), prefixLower) {
			continue
		}
		full := filepath.Join(dir, name)
		if tildeStripped {
			if home, err := os.UserHomeDir(); err == nil {
				if rel, err := filepath.Rel(home, full); err == nil && !strings.HasPrefix(rel, "..") {
					full = "~/" + rel
				}
			}
		} else if dir == "." {
			full = name
		}
		out = append(out, dirCompletion{path: full, onDisk: filepath.Join(readDir, name), entry: e})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func pathComplete(query string) []string {
	found, err := completeDirEntries(query, "", fs.DirEntry.IsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range found {
		out = append(out, c.path)
	}
	expanded, _ := expandTildeDir(query)
	if _, prefix := filepath.Split(expanded); prefix == "" {
		parent := strings.TrimRight(query, "/")
		if parent != "" {
			out = append([]string{parent}, out...)
		}
	}
	switch strings.TrimSpace(query) {
	case ".":
		out = append([]string{".", ".."}, out...)
	case "..":
		out = append([]string{".."}, out...)
	}
	return out
}

func expandTilde(p string) (string, bool) {
	if !strings.HasPrefix(p, "~") {
		return p, false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p, false
	}
	if p == "~" {
		return home, true
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), true
	}
	return p, false
}

// expandTildeDir is expandTilde for a query being completed: a trailing
// slash, which asks for the directory's contents, survives the expansion.
func expandTildeDir(p string) (string, bool) {
	out, stripped := expandTilde(p)
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out, stripped
}

// tildePath spells p under ~ when it is inside the home directory.
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func resolvePath(p string) string {
	if p == "" {
		p = "."
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func hasGlob(p string) bool {
	return strings.ContainsAny(p, "*?[")
}
