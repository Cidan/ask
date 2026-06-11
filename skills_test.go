package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, frontmatterExtra, body string) string {
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
	home := isolateHome(t)
	cwd := t.TempDir()

	globalDir := filepath.Join(home, ".claude", "skills")
	projectDir := filepath.Join(cwd, ".claude", "skills")
	writeSkill(t, globalDir, "deploy", "", "global deploy instructions")
	writeSkill(t, projectDir, "deploy", "", "project deploy instructions")
	writeSkill(t, globalDir, "review", "user-invocable: false\n", "review instructions")
	writeSkill(t, globalDir, "secret", "disable-model-invocation: true\n", "secret instructions")

	// Invalid packages are skipped: bad name characters, name/dir
	// mismatch, missing description.
	writeSkill(t, globalDir, "Bad_Name", "", "x")
	mismatchDir := filepath.Join(globalDir, "mismatch")
	_ = os.MkdirAll(mismatchDir, 0o755)
	_ = os.WriteFile(filepath.Join(mismatchDir, "SKILL.md"),
		[]byte("---\nname: other\ndescription: d\n---\nbody"), 0o644)
	nodescDir := filepath.Join(globalDir, "nodesc")
	_ = os.MkdirAll(nodescDir, 0o755)
	_ = os.WriteFile(filepath.Join(nodescDir, "SKILL.md"),
		[]byte("---\nname: nodesc\n---\nbody"), 0o644)

	skills := discoverSkills(cwd)
	byName := map[string]askSkill{}
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
	home := isolateHome(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "deploy", "", "body here")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "hidden", "disable-model-invocation: true\n", "body")

	block := skillsPromptBlock(discoverSkills(cwd))
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

	if got := skillsPromptBlock(nil); got != "" {
		t.Errorf("no skills must render nothing: %q", got)
	}
}

func TestExpandSkillInvocation(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "deploy", "", "Step 1: build.\nStep 2: ship.")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "modelonly", "user-invocable: false\n", "body")

	msg, ok := expandSkillInvocation(cwd, "/deploy to prod")
	if !ok {
		t.Fatal("known skill must expand")
	}
	if !strings.Contains(msg, `<loaded_skill name="deploy"`) ||
		!strings.Contains(msg, "Step 2: ship.") ||
		!strings.Contains(msg, "arguments: to prod") {
		t.Errorf("expansion wrong: %q", msg)
	}

	msg, ok = expandSkillInvocation(cwd, "/deploy")
	if !ok || !strings.Contains(msg, "no arguments") {
		t.Errorf("argless expansion wrong: %v %q", ok, msg)
	}

	if _, ok := expandSkillInvocation(cwd, "/modelonly"); ok {
		t.Error("user-invocable:false skills must not expand")
	}
	if _, ok := expandSkillInvocation(cwd, "/unknown"); ok {
		t.Error("unknown skills must not expand")
	}
	if _, ok := expandSkillInvocation(cwd, "plain text"); ok {
		t.Error("non-slash text must not expand")
	}
}

func TestParseMarkdownFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.md")
	_ = os.WriteFile(path, []byte("---\nname: x\ndescription: \"quoted: value\"\nflag: true\n---\nthe body\nline two"), 0o644)
	fields, body, ok := parseMarkdownFrontmatter(path)
	if !ok || fields["name"] != "x" || fields["description"] != "quoted: value" || fields["flag"] != "true" {
		t.Errorf("fields wrong: %v %v", fields, ok)
	}
	if !strings.Contains(body, "the body") || !strings.Contains(body, "line two") {
		t.Errorf("body wrong: %q", body)
	}

	_ = os.WriteFile(path, []byte("no frontmatter"), 0o644)
	if _, _, ok := parseMarkdownFrontmatter(path); ok {
		t.Error("missing frontmatter must report !ok")
	}
}

func TestAgentProviderProbeInit_SkillsAsSlashCommands(t *testing.T) {
	home := isolateHome(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "deploy", "", "body")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "modelonly", "user-invocable: false\n", "body")

	p := deepseekAgentProvider()
	cmd := p.ProbeInit(ProviderSessionArgs{Cwd: cwd, TabID: 5})
	if cmd == nil {
		t.Fatal("agent providers must probe for skills")
	}
	msg, ok := cmd().(providerInitLoadedMsg)
	if !ok || msg.tabID != 5 {
		t.Fatalf("probe msg wrong: %+v", msg)
	}
	if len(msg.slashCmds) != 1 || msg.slashCmds[0].Name != "deploy" {
		t.Errorf("user-invocable skills must surface as slash commands: %+v", msg.slashCmds)
	}
}
