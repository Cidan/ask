// Package testhome keeps a package's tests off the developer's machine.
package testhome

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Main runs m with $HOME and the XDG base directories pointed at a fresh temp
// directory, so no test reads or writes the real ~/.config/ask or ~/.claude —
// or the developer's config steers a test at a real model — by accident.
// A test that needs a home of its own still sets one with t.Setenv. env adds
// further variables for the whole run, as key, value pairs.
func Main(m *testing.M, env ...string) {
	realHome = os.Getenv("HOME")
	home, err := os.MkdirTemp("", "ask-test-home-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome:", err)
		os.Exit(1)
	}
	vars := []string{
		"HOME", home,
		"XDG_CONFIG_HOME", filepath.Join(home, ".config"),
		"XDG_CACHE_HOME", filepath.Join(home, ".cache"),
		"XDG_DATA_HOME", filepath.Join(home, ".local", "share"),
		"JJ_USER", "Test User",
		"JJ_EMAIL", "test@example.com",
	}
	vars = append(vars, env...)
	for i := 0; i+1 < len(vars); i += 2 {
		if err := os.Setenv(vars[i], vars[i+1]); err != nil {
			fmt.Fprintln(os.Stderr, "testhome:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

var realHome string

// UseRealHome points $HOME back at the developer's own for one test. Only
// opt-in tests that deliberately drive real tools use it — the claude CLI
// finds its login there.
func UseRealHome(t *testing.T) {
	t.Helper()
	if realHome == "" {
		t.Fatal("testhome: no real home recorded; is TestMain calling testhome.Main?")
	}
	t.Setenv("HOME", realHome)
}

// NoClaude is the env pair that points the Claude Code provider at a binary
// that does not exist, so a test that reaches it (a model listing over the
// real registry) fails fast instead of starting the real CLI.
func NoClaude() []string {
	return []string{"ASK_CLAUDE_BIN", filepath.Join(os.TempDir(), "ask-test-no-claude")}
}
