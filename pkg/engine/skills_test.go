package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestSkill(t *testing.T, root, name, frontmatterExtra, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + name + " does things\n" + frontmatterExtra + "---\n" + body
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverSkills_ValidationAndPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	globalDir := filepath.Join(home, ".claude", "skills")
	projectDir := filepath.Join(cwd, ".claude", "skills")
	writeTestSkill(t, globalDir, "deploy", "", "global deploy instructions")
	writeTestSkill(t, projectDir, "deploy", "", "project deploy instructions")
	writeTestSkill(t, globalDir, "review", "user-invocable: false\n", "review instructions")
	writeTestSkill(t, globalDir, "secret", "disable-model-invocation: true\n", "secret instructions")

	// Invalid packages are skipped: bad name characters, name/dir
	// mismatch, missing description.
	writeTestSkill(t, globalDir, "Bad_Name", "", "x")
	mismatchDir := filepath.Join(globalDir, "mismatch")
	_ = os.MkdirAll(mismatchDir, 0o755)
	_ = os.WriteFile(filepath.Join(mismatchDir, "SKILL.md"),
		[]byte("---\nname: other\ndescription: d\n---\nbody"), 0o644)
	nodescDir := filepath.Join(globalDir, "nodesc")
	_ = os.MkdirAll(nodescDir, 0o755)
	_ = os.WriteFile(filepath.Join(nodescDir, "SKILL.md"),
		[]byte("---\nname: nodesc\n---\nbody"), 0o644)

	skills := DiscoverSkills(cwd)
	byName := map[string]Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}
	if len(skills) != 3 {
		t.Fatalf("want deploy+review+secret, got %d: %v", len(skills), byName)
	}
	if !strings.Contains(byName["deploy"].Path, cwd) {
		t.Errorf("project skill must win over global: %s", byName["deploy"].Path)
	}
	if byName["review"].UserInvocable {
		t.Error("user-invocable: false must be honoured")
	}
	if !byName["secret"].DisableModelInvocation {
		t.Error("disable-model-invocation must be honoured")
	}
}

func TestSkillsPromptBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	writeTestSkill(t, filepath.Join(home, ".claude", "skills"), "deploy", "", "body here")
	writeTestSkill(t, filepath.Join(home, ".claude", "skills"), "hidden", "disable-model-invocation: true\n", "body")

	block := SkillsPromptBlock(DiscoverSkills(cwd))
	if !strings.Contains(block, "<available_skills>") ||
		!strings.Contains(block, "<name>deploy</name>") ||
		!strings.Contains(block, "deploy does things") ||
		!strings.Contains(block, "SKILL.md") {
		t.Errorf("trigger block wrong: %q", block)
	}
	if strings.Contains(block, "hidden") {
		t.Error("disable-model-invocation skills must not be listed")
	}
	if strings.Contains(block, "body here") {
		t.Error("progressive disclosure: the body must NOT be in the prompt")
	}
	if !strings.Contains(block, "read the skill's location file") {
		t.Error("usage contract missing")
	}

	if got := SkillsPromptBlock(nil); got != "" {
		t.Errorf("no skills must render nothing: %q", got)
	}
}

func TestExpandSkillInvocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	writeTestSkill(t, filepath.Join(home, ".claude", "skills"), "deploy", "", "Step 1: build.\nStep 2: ship.")
	writeTestSkill(t, filepath.Join(home, ".claude", "skills"), "modelonly", "user-invocable: false\n", "body")

	msg, ok := ExpandSkillInvocation(cwd, "/deploy to prod")
	if !ok {
		t.Fatal("known skill must expand")
	}
	if !strings.Contains(msg, `<loaded_skill name="deploy"`) ||
		!strings.Contains(msg, "Step 2: ship.") ||
		!strings.Contains(msg, "arguments: to prod") {
		t.Errorf("expansion wrong: %q", msg)
	}

	msg, ok = ExpandSkillInvocation(cwd, "/deploy")
	if !ok || !strings.Contains(msg, "no arguments") {
		t.Errorf("argless expansion wrong: %v %q", ok, msg)
	}

	if _, ok := ExpandSkillInvocation(cwd, "/modelonly"); ok {
		t.Error("user-invocable:false skills must not expand")
	}
	if _, ok := ExpandSkillInvocation(cwd, "/unknown"); ok {
		t.Error("unknown skills must not expand")
	}
	if _, ok := ExpandSkillInvocation(cwd, "plain text"); ok {
		t.Error("non-slash text must not expand")
	}
}

func TestParseMarkdownFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.md")
	_ = os.WriteFile(path, []byte("---\nname: x\ndescription: \"quoted: value\"\nflag: true\n---\nthe body\nline two"), 0o644)
	fields, body, ok := ParseMarkdownFrontmatter(path)
	if !ok || fields["name"] != "x" || fields["description"] != "quoted: value" || fields["flag"] != "true" {
		t.Errorf("fields wrong: %v %v", fields, ok)
	}
	if !strings.Contains(body, "the body") || !strings.Contains(body, "line two") {
		t.Errorf("body wrong: %q", body)
	}

	_ = os.WriteFile(path, []byte("no frontmatter"), 0o644)
	if _, _, ok := ParseMarkdownFrontmatter(path); ok {
		t.Error("missing frontmatter must report !ok")
	}
}

func TestExpandSkillInvocation_LinkedDocs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	_ = os.MkdirAll(filepath.Join(cwd, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(cwd, "docs", "setup.md"), []byte("# Setup\nRun ./setup.sh first.\n"), 0o644)

	writeTestSkill(t, filepath.Join(home, ".claude", "skills"), "deploy", "",
		"Step 1: build.\nSee @docs/setup.md for setup.\nStep 2: ship.")

	msg, ok := ExpandSkillInvocation(cwd, "/deploy")
	if !ok {
		t.Fatal("skill must expand")
	}
	if !strings.Contains(msg, "Step 1: build.") {
		t.Errorf("skill body missing: %q", msg)
	}
	if !strings.Contains(msg, "Run ./setup.sh first.") {
		t.Errorf("linked doc body must be included: %q", msg)
	}
	if !strings.Contains(msg, `<file path="`+filepath.Join(cwd, "docs", "setup.md")+`"`) {
		t.Errorf("linked doc path must appear: %q", msg)
	}
	fileIdx := strings.Index(msg, "<file path=")
	argsIdx := strings.Index(msg, "no arguments")
	if fileIdx < 0 || argsIdx < 0 || fileIdx >= argsIdx {
		t.Errorf("linked docs must appear before the args tail: %s", msg)
	}
}
