package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathComplete_HiddenDirsOnlyWhenDotPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir(filepath.Join(dir, ".hidden"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "visible"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	all := pathComplete("")
	var sawHidden bool
	for _, p := range all {
		if p == ".hidden" {
			sawHidden = true
		}
	}
	if sawHidden {
		t.Errorf("empty query should not show hidden dirs: %v", all)
	}
	dot := pathComplete(".h")
	var found bool
	for _, p := range dot {
		if p == ".hidden" {
			found = true
		}
	}
	if !found {
		t.Errorf("dot prefix query should match hidden dir: %v", dot)
	}
}

func TestPathComplete_FiltersNonDirs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "a.txt"), "content")
	got := pathComplete("")
	for _, p := range got {
		if strings.HasSuffix(p, "a.txt") {
			t.Errorf("pathComplete should not list files: got %v", got)
		}
	}
	var sawDir bool
	for _, p := range got {
		if p == "sub" {
			sawDir = true
		}
	}
	if !sawDir {
		t.Errorf("sub dir missing from results: %v", got)
	}
}

func TestPathComplete_TildePrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.Mkdir(filepath.Join(home, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := pathComplete("~/d")
	var saw bool
	for _, p := range got {
		if p == "~/docs" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected ~/docs in results: %v", got)
	}
}

func TestPathComplete_PrependsDotDot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	got := pathComplete("..")
	if len(got) == 0 || got[0] != ".." {
		t.Errorf(".. query should prepend '..': %v", got)
	}
}

func TestModelPathPickerCmd(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.input.SetValue("cd foo")
	if got := m.pathPickerCmd(); got != "cd" {
		t.Errorf("pathPickerCmd=%q want cd", got)
	}
	m.input.SetValue("ls bar")
	if got := m.pathPickerCmd(); got != "ls" {
		t.Errorf("pathPickerCmd=%q want ls", got)
	}
	m.input.SetValue("random")
	if got := m.pathPickerCmd(); got != "" {
		t.Errorf("pathPickerCmd=%q want empty", got)
	}
}

func TestModelPathQuery(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.input.SetValue("cd some/path")
	if got := m.pathQuery(); got != "some/path" {
		t.Errorf("pathQuery=%q want some/path", got)
	}
	m.input.SetValue("nope")
	if got := m.pathQuery(); got != "" {
		t.Errorf("pathQuery=%q want empty", got)
	}
}

func TestRefreshPathMatches_ClearsWhenInactive(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.pathMatches = []string{"lingering"}
	m.input.SetValue("")
	m.refreshPathMatches()
	if len(m.pathMatches) != 0 {
		t.Errorf("pathMatches should be cleared: %v", m.pathMatches)
	}
}

func TestParseFrontmatter_BOMStrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bom.md")
	writeFile(t, path, "\ufeff---\nname: bar\ndescription: bom test\n---\nbody\n")
	name, desc := parseFrontmatter(path)
	if name != "bar" || desc != "bom test" {
		t.Errorf("BOM-prefixed frontmatter not read: name=%q desc=%q", name, desc)
	}
}

func TestParseFrontmatter_MissingFileReturnsEmpty(t *testing.T) {
	name, desc := parseFrontmatter("/no/such/path.md")
	if name != "" || desc != "" {
		t.Errorf("missing file should return empty, got (%q,%q)", name, desc)
	}
}

func TestSlashCmdDescriptions_IndexesLocalCommands(t *testing.T) {
	home := isolateHome(t)
	// Build ~/.claude/commands/test.md with frontmatter.
	writeFile(t, filepath.Join(home, ".claude", "commands", "alpha.md"),
		"---\nname: alpha\ndescription: alpha does X\n---\nbody\n")
	writeFile(t, filepath.Join(home, ".claude", "skills", "beta", "SKILL.md"),
		"---\nname: beta\ndescription: beta skill\n---\nbody\n")
	// Run with cwd set to a fresh tmp so local commands stay empty.
	cwd := t.TempDir()
	t.Chdir(cwd)
	index := slashCmdDescriptions()
	if index["alpha"] != "alpha does X" {
		t.Errorf("alpha lookup missing: %q", index["alpha"])
	}
	if index["beta"] != "beta skill" {
		t.Errorf("beta lookup missing: %q", index["beta"])
	}
}

func TestEnrichSlashCommands_LooksUpDescriptions(t *testing.T) {
	home := isolateHome(t)
	writeFile(t, filepath.Join(home, ".claude", "commands", "named.md"),
		"---\nname: named\ndescription: does a thing\n---\n")
	t.Chdir(t.TempDir())
	out := enrichSlashCommands([]string{"named", "unknown"})
	if len(out) != 2 {
		t.Fatalf("len=%d want 2", len(out))
	}
	if out[0].Description != "does a thing" {
		t.Errorf("named desc=%q want 'does a thing'", out[0].Description)
	}
	if out[1].Description != "" {
		t.Errorf("unknown desc=%q want empty", out[1].Description)
	}
}

func TestResolveDir_EmptyIsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := resolveDir("")
	if err != nil {
		t.Fatalf("resolveDir(''): %v", err)
	}
	if got != home {
		t.Errorf("resolveDir('')=%q want %q", got, home)
	}
}

func TestResolveDir_TildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := resolveDir("~/sub")
	if err != nil {
		t.Fatalf("resolveDir(~/sub): %v", err)
	}
	want := filepath.Join(home, "sub")
	if got != want {
		t.Errorf("resolveDir=%q want %q", got, want)
	}
	got2, _ := resolveDir("~")
	if got2 != home {
		t.Errorf("resolveDir(~)=%q want %q", got2, home)
	}
}
