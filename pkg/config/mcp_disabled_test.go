package config

import (
	"testing"
)

func TestIsProjectConfigEmpty_MCPDisabled(t *testing.T) {
	if IsProjectConfigEmpty(ProjectConfig{MCPDisabled: map[string]bool{"x": true}}) {
		t.Error("a project block with an MCP override is not empty")
	}
	if !IsProjectConfigEmpty(ProjectConfig{MCPDisabled: map[string]bool{}}) {
		t.Error("an empty override map is empty")
	}
	if !IsProjectConfigEmpty(ProjectConfig{}) {
		t.Error("zero value is empty")
	}
}

func TestConfigMCPDisabled_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Config{MCPDisabled: map[string]bool{"s": true}}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.MCPDisabled["s"] {
		t.Fatalf("user MCPDisabled must round-trip: %+v", got.MCPDisabled)
	}
}

func TestProjectMCPDisabled_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	pc := ProjectConfig{MCPDisabled: map[string]bool{"s": false}}
	if err := SaveProject(cwd, pc); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got := LoadProjectConfig(cfg, cwd)
	v, ok := got.MCPDisabled["s"]
	if !ok || v != false {
		t.Fatalf("project MCPDisabled must round-trip: %+v", got.MCPDisabled)
	}
}
