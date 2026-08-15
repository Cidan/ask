package engine

import (
	"path"
	"strings"
)

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
	alts = append(alts, s[start:])
	return alts
}
