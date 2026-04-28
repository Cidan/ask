package main

import (
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

func pathComplete(query string) []string {
	expanded, tildeStripped := expandTilde(query)
	dir, prefix := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	showHidden := strings.HasPrefix(prefix, ".")
	prefixLower := strings.ToLower(prefix)
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
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
		out = append(out, full)
	}
	sort.Strings(out)
	if prefix == "" {
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
