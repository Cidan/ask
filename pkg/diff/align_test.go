package diff

import (
	"math/rand"
	"strings"
	"testing"
)

// lcsLen is the brute-force longest common subsequence length.
func lcsLen(a, b []byte) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

// checkAlignment asserts match pairs equal elements in increasing order and
// is a longest common subsequence.
func checkAlignment(t *testing.T, a, b []byte) {
	t.Helper()
	match := Align(a, b)
	if len(match) != len(a) {
		t.Fatalf("Align(%q, %q) returned %d entries", a, b, len(match))
	}
	last, n := -1, 0
	for i, j := range match {
		if j < 0 {
			continue
		}
		if j <= last || j >= len(b) || a[i] != b[j] {
			t.Fatalf("Align(%q, %q) = %v: bad pair %d→%d", a, b, match, i, j)
		}
		last = j
		n++
	}
	if want := lcsLen(a, b); n != want {
		t.Fatalf("Align(%q, %q) matched %d, the longest common subsequence is %d", a, b, n, want)
	}
}

func TestAlign_IsALongestCommonSubsequence(t *testing.T) {
	for _, c := range [][2]string{
		{"", ""}, {"abc", ""}, {"", "abc"}, {"abc", "abc"},
		{"abcabba", "cbabac"}, {"kitten", "sitting"},
		{"We **must** keep `go test` green", "We must keep go test green"},
		{"| a | b |", "│ a │ b │"}, {"- item", "• item"},
	} {
		checkAlignment(t, []byte(c[0]), []byte(c[1]))
	}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		a := make([]byte, r.Intn(40))
		b := make([]byte, r.Intn(40))
		for k := range a {
			a[k] = "abc "[r.Intn(4)]
		}
		for k := range b {
			b[k] = "abc "[r.Intn(4)]
		}
		checkAlignment(t, a, b)
	}
}

// A long text with markup removed and decoration added — the shape the chat
// aligns — keeps every one of its characters matched, far past the old
// frontier budget.
func TestAlign_LongTextWithManyEdits(t *testing.T) {
	var src, shown strings.Builder
	for i := 0; i < 1500; i++ {
		src.WriteString("**word** ")
		shown.WriteString("• word ")
	}
	a, b := []rune(shown.String()), []rune(src.String())
	match := Align(a, b)
	letters := 0
	for i, j := range match {
		if strings.ContainsRune("word", a[i]) {
			if j < 0 || b[j] != a[i] {
				t.Fatalf("shown letter %d (%q) unmatched", i, a[i])
			}
			letters++
		}
	}
	if letters != 4*1500 {
		t.Fatalf("checked %d letters", letters)
	}
}
