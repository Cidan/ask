package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunPathCommand_Dispatches covers the pure dispatcher: each
// recognised command name routes to the matching do* method, and an
// unknown command is a passthrough.
func TestRunPathCommand_Dispatches(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"cd", "doCd"},
		{"ls", "doLs"},
		{"/add-dir", "doAddDir"},
		{"bogus", ""},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			m := newTestModel(t, newFakeProvider())
			mm, _ := m.runPathCommand(tc.cmd, "/tmp")
			got := mm.(model)
			if tc.want == "" {
				// Unknown command: passthrough, no history entry, no kill.
				if len(got.history) != 0 {
					t.Errorf("unknown cmd %q should not append history; got %+v", tc.cmd, got.history)
				}
				return
			}
			// Each dispatched handler writes a history entry on the
			// happy path. Asserting on the presence of *some* entry
			// pins the dispatch without coupling to message wording.
			if len(got.history) == 0 {
				t.Errorf("dispatcher should have routed to %s; history empty", tc.want)
			}
		})
	}
}

// TestDoCd_SameCwdNoOp verifies that `cd <abs already-cwd>` is a
// no-op: no history entry, no proc kill, no session id reset. This
// guard matters because doCd also clears m.history — accidentally
// firing on a same-cwd noop would wipe the user's transcript.
func TestDoCd_SameCwdNoOp(t *testing.T) {
	dir := t.TempDir()
	abs, _ := filepath.Abs(dir)
	m := newTestModel(t, newFakeProvider())
	m.cwd = abs
	m.proc = &providerProc{stdin: &bufferCloser{}}
	m.sessionID = "S-keep"
	m.sessionMinted = true
	m.history = []historyEntry{{kind: histUser, text: "earlier"}}

	mm, _ := m.doCd(abs)
	got := mm.(model)

	if len(got.history) != 1 || got.history[0].text != "earlier" {
		t.Errorf("same-cwd cd should not mutate history; got %+v", got.history)
	}
	if got.sessionID != "S-keep" || !got.sessionMinted {
		t.Errorf("same-cwd cd should preserve session id+minted; got id=%q minted=%v", got.sessionID, got.sessionMinted)
	}
	if got.proc != m.proc {
		t.Errorf("same-cwd cd should not kill proc; got %+v", got.proc)
	}
}

// TestDoCd_ChdirFailureErrorInHistory: an unresolvable target writes
// a red error history entry and leaves cwd untouched. This protects
// users from a confusing silent failure on typo'd paths.
func TestDoCd_ChdirFailureErrorInHistory(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	originalCwd := m.cwd
	// A path that doesn't exist. resolveDir returns it as Abs and
	// does NOT error; os.Chdir is what fails.
	mm, _ := m.doCd("/nonexistent/never-exists-zzz/abc")
	got := mm.(model)

	if got.cwd != originalCwd {
		t.Errorf("cwd should be unchanged on chdir failure; got %q want %q", got.cwd, originalCwd)
	}
	if len(got.history) != 1 {
		t.Fatalf("expected one error history entry; got %d", len(got.history))
	}
	if !strings.Contains(stripAnsi(got.history[0].text), "cd:") {
		t.Errorf("error history should mention 'cd:'; got %q", got.history[0].text)
	}
}

// TestDoCd_SuccessUpdatesCwdAndClearsSession is the happy path. cwd
// updates, the running proc is killed, and a confirmation message is
// appended on top of any existing history.
//
// doCd calls os.Chdir internally; the test pins and restores the
// process cwd so a later test in the same package that depends on
// the real cwd (TestResolvePath in util_test.go) doesn't see a
// deleted temp directory.
func TestDoCd_SuccessUpdatesCwdAndClearsSession(t *testing.T) {
	origCwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	dir := t.TempDir()
	target := filepath.Join(dir, "dest")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	abs, _ := filepath.Abs(target)

	m := newTestModel(t, newFakeProvider())
	m.cwd = "/somewhere/else"
	m.proc = &providerProc{stdin: &bufferCloser{}}
	m.sessionID = "S-old"
	m.sessionMinted = true

	mm, _ := m.doCd(target)
	got := mm.(model)

	if got.cwd != abs {
		t.Errorf("cwd=%q want %q", got.cwd, abs)
	}
	if got.proc != nil {
		t.Errorf("cd should kill proc; got %+v", got.proc)
	}
	if got.sessionID != "" || got.sessionMinted {
		t.Errorf("cd should clear session id and minted; got id=%q minted=%v", got.sessionID, got.sessionMinted)
	}
	if len(got.history) != 1 {
		t.Fatalf("expected one confirmation entry; got %d", len(got.history))
	}
	if !strings.Contains(stripAnsi(got.history[0].text), "cd "+abs) {
		t.Errorf("confirmation should mention target path; got %q", got.history[0].text)
	}
}

// TestDoLs_DirectoryListsEntries covers the non-glob, dir target
// path. doLs reads the dir, builds the lsRow list, and renders the
// header + per-row output. Asserting on the bare name in the
// rendered output pins both the readdir path and the lsRow sort.
func TestDoLs_DirectoryListsEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	m := newTestModel(t, newFakeProvider())
	mm, _ := m.doLs(dir)
	got := mm.(model)

	if len(got.history) != 1 {
		t.Fatalf("expected one history entry; got %d", len(got.history))
	}
	stripped := stripAnsi(got.history[0].text)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(stripped, name) {
			t.Errorf("ls output missing %q; got:\n%s", name, stripped)
		}
	}
}

// TestDoLs_GlobExpands verifies the glob branch: a pattern with `*`
// in the target fans out via filepath.Glob and renders the matches.
func TestDoLs_GlobExpands(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"foo.go", "foo_test.go", "README.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	m := newTestModel(t, newFakeProvider())
	mm, _ := m.doLs(filepath.Join(dir, "foo*.go"))
	got := mm.(model)

	if len(got.history) != 1 {
		t.Fatalf("expected one history entry; got %d", len(got.history))
	}
	stripped := stripAnsi(got.history[0].text)
	if !strings.Contains(stripped, "foo.go") || !strings.Contains(stripped, "foo_test.go") {
		t.Errorf("glob should match both foo*.go; got:\n%s", stripped)
	}
	if strings.Contains(stripped, "README.md") {
		t.Errorf("glob should NOT match non-glob files; got README in:\n%s", stripped)
	}
}

// TestDoLs_GlobNoMatchDimMessage: zero glob hits writes a dim "no
// matches" line and does NOT spin up an error history entry.
func TestDoLs_GlobNoMatchDimMessage(t *testing.T) {
	dir := t.TempDir()
	m := newTestModel(t, newFakeProvider())
	mm, _ := m.doLs(filepath.Join(dir, "*.does-not-exist"))
	got := mm.(model)

	if len(got.history) != 1 {
		t.Fatalf("expected one history entry; got %d", len(got.history))
	}
	stripped := stripAnsi(got.history[0].text)
	if !strings.Contains(stripped, "no matches") {
		t.Errorf("expected 'no matches' dim line; got %q", stripped)
	}
}

// TestDoLs_LstatErrorInHistory: a non-existent path surfaces a
// red error entry rather than panicking.
func TestDoLs_LstatErrorInHistory(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	mm, _ := m.doLs("/nope/never/there-xyz")
	got := mm.(model)

	if len(got.history) != 1 {
		t.Fatalf("expected one history entry; got %d", len(got.history))
	}
	stripped := stripAnsi(got.history[0].text)
	if !strings.Contains(stripped, "ls:") {
		t.Errorf("expected 'ls:' error line; got %q", stripped)
	}
}

// TestResolveDir covers the four resolution branches: empty (→ home),
// `~`, `~/x`, and an absolute path. With HOME pinned to a temp dir,
// the home branch is testable without polluting the user's HOME.
func TestResolveDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		in, want string
	}{
		{"", home},
		{"~", home},
		{"~/x", filepath.Join(home, "x")},
		{"/abs/whatever", "/abs/whatever"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := resolveDir(tc.in)
			if err != nil {
				t.Fatalf("resolveDir(%q) err=%v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("resolveDir(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResolveDir_RelativePathIsAbsolified: a non-empty relative path
// must come back as an absolute path (filepath.Abs applied to the
// cleaned input). We don't pin a specific value because cwd varies;
// we pin the *property* that the result is absolute. The
// filepath.Abs fallback also gets exercised when HOME is pinned but
// is empty, so the tilde branch in resolveDir is bypassed.
func TestResolveDir_RelativePathIsAbsolified(t *testing.T) {
	t.Chdir(t.TempDir())
	got, err := resolveDir("rel/path")
	if err != nil {
		t.Fatalf("resolveDir: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolveDir(relative)=%q, want absolute", got)
	}
	if !strings.HasSuffix(got, filepath.Join("rel", "path")) {
		t.Errorf("resolveDir(relative)=%q, want suffix rel/path", got)
	}
}

// TestRenderLsOutput_SortsDirsFirstAlphabetical: the brief requires
// directories sort before files, and ties break alphabetically. The
// assertion is on the rendered row order, with ANSI stripped so
// styles don't break the substring check.
func TestRenderLsOutput_SortsDirsFirstAlphabetical(t *testing.T) {
	dir := t.TempDir()
	// Build: file B, dir A, file A, dir B (interleaved on disk so the
	// sort is what produces the order, not creation order).
	if err := os.MkdirAll(filepath.Join(dir, "zdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zfile"), nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "afile"), nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := renderLsOutput(dir, []string{
		filepath.Join(dir, "zfile"),
		filepath.Join(dir, "zdir"),
		filepath.Join(dir, "afile"),
		filepath.Join(dir, "adir"),
	})
	stripped := stripAnsi(out)
	adir := strings.Index(stripped, "adir")
	zdir := strings.Index(stripped, "zdir")
	afile := strings.Index(stripped, "afile")
	zfile := strings.Index(stripped, "zfile")
	if adir < 0 || zdir < 0 || afile < 0 || zfile < 0 {
		t.Fatalf("expected all 4 names in output; got:\n%s", stripped)
	}
	if !(adir < zdir && zdir < afile && afile < zfile) {
		t.Errorf("sort order wrong; got adir=%d zdir=%d afile=%d zfile=%d in:\n%s", adir, zdir, afile, zfile, stripped)
	}
	// Directories get a trailing `/`, files do not.
	if !strings.Contains(stripped, "adir/") {
		t.Errorf("dir rows should be suffixed with '/'; got:\n%s", stripped)
	}
}

// TestRenderLsOutput_SymlinkAndExeSuffixes: a symlink row renders
// `name → target`, and an executable file row renders `name*`.
func TestRenderLsOutput_SymlinkAndExeSuffixes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "lnk")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	exe := filepath.Join(dir, "runme")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	out := renderLsOutput(dir, []string{link, exe})
	stripped := stripAnsi(out)
	if !strings.Contains(stripped, "lnk → "+target) {
		t.Errorf("symlink row should be `lnk → target`; got:\n%s", stripped)
	}
	if !strings.Contains(stripped, "runme*") {
		t.Errorf("exe row should be `runme*`; got:\n%s", stripped)
	}
}

// TestRenderLsOutput_EmptyDir says an empty directory renders the
// `(empty)` placeholder instead of the header alone.
func TestRenderLsOutput_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	out := renderLsOutput(dir, []string{})
	stripped := stripAnsi(out)
	if !strings.Contains(stripped, "(empty)") {
		t.Errorf("empty dir should render '(empty)'; got:\n%s", stripped)
	}
	if !strings.Contains(stripped, "(0 items)") {
		t.Errorf("empty dir header should say '0 items'; got:\n%s", stripped)
	}
}
