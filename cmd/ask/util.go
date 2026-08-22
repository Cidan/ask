package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"
)

func short(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(b)/float64(div), "KMGTPE"[exp])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func wordWrap(s string, width int) []string {
	if width <= 0 || s == "" {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	cur := words[0]
	curW := lipgloss.Width(cur)
	for _, w := range words[1:] {
		ww := lipgloss.Width(w)
		if curW+1+ww > width {
			lines = append(lines, cur)
			cur = w
			curW = ww
			continue
		}
		cur += " " + w
		curW += 1 + ww
	}
	return append(lines, cur)
}

// naturalLess orders strings the way a person reads version-like names:
// digit runs compare numerically ("gemini-3-pro" < "gemini-3.1-pro" <
// "gemini-10"), everything else byte-wise. Callers fold case themselves.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		if isASCIIDigit(a[0]) && isASCIIDigit(b[0]) {
			an, bn := digitRunLen(a), digitRunLen(b)
			ai, bi := strings.TrimLeft(a[:an], "0"), strings.TrimLeft(b[:bn], "0")
			if ai != bi {
				if len(ai) != len(bi) {
					return len(ai) < len(bi)
				}
				return ai < bi
			}
			a, b = a[an:], b[bn:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

func digitRunLen(s string) int {
	n := 0
	for n < len(s) && isASCIIDigit(s[n]) {
		n++
	}
	return n
}

// groupDigits renders 1048576 as "1,048,576".
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func padRight(s string, w int) string {
	pad := w - lipgloss.Width(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

func shortCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	return shortCwdOf(cwd)
}

func shortCwdOf(cwd string) string {
	if cwd == "" {
		return "?"
	}
	home, _ := os.UserHomeDir()
	p := cwd
	if home != "" && (cwd == home || strings.HasPrefix(cwd, home+string(os.PathSeparator))) {
		p = "~" + strings.TrimPrefix(cwd, home)
	}
	if p == "~" || p == string(os.PathSeparator) {
		return p
	}
	parts := strings.Split(p, string(os.PathSeparator))
	last := len(parts) - 1
	for i, part := range parts {
		if i == last || part == "" || part == "~" {
			continue
		}
		r := []rune(part)
		parts[i] = string(r[:1])
	}
	return strings.Join(parts, string(os.PathSeparator))
}
