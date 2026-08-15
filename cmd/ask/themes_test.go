package main

import (
	"testing"

	lipgloss "charm.land/lipgloss/v2"
)

func TestThemeRegistryContainsAyu(t *testing.T) {
	got := themeByName("ayu")
	if got.name != "ayu" {
		t.Fatalf("themeByName(\"ayu\").name = %q, want %q", got.name, "ayu")
	}
	if got.accent != lipgloss.Color("#E6B450") {
		t.Errorf("ayu accent = %v, want #E6B450", got.accent)
	}
	if got.background != lipgloss.Color("#1F2430") {
		t.Errorf("ayu background = %v, want #1F2430", got.background)
	}
	if got.foreground != lipgloss.Color("#E6E1CF") {
		t.Errorf("ayu foreground = %v, want #E6E1CF", got.foreground)
	}
	if got.highlightFG != lipgloss.Color("#E6B450") {
		t.Errorf("ayu highlightFG = %v, want #E6B450", got.highlightFG)
	}
	if got.stringFG != lipgloss.Color("#95E6CB") {
		t.Errorf("ayu stringFG = %v, want #95E6CB", got.stringFG)
	}
}

func TestThemeByNameUnknownFallsBackToDefault(t *testing.T) {
	got := themeByName("definitely-not-a-theme")
	if got.name != "default" {
		t.Fatalf("themeByName(unknown).name = %q, want %q", got.name, "default")
	}
}

func TestBuildGlamourStyleUsesThemeMarkdownOverrides(t *testing.T) {
	style := buildGlamourStyle(ayuTheme())
	if got := style.Code.StylePrimitive.Color; got == nil || *got != "#E6B450" {
		t.Fatalf("inline code color = %v, want #E6B450", got)
	}
	if got := style.CodeBlock.Chroma.LiteralString.Color; got == nil || *got != "#95E6CB" {
		t.Fatalf("string token color = %v, want #95E6CB", got)
	}
}

func TestBuildGlamourStyleFallsBackToSemanticThemeColors(t *testing.T) {
	style := buildGlamourStyle(draculaTheme())
	if got := style.Code.StylePrimitive.Color; got == nil || *got != "#BFBFBF" {
		t.Fatalf("inline code fallback color = %v, want #BFBFBF", got)
	}
	if got := style.CodeBlock.Chroma.LiteralString.Color; got == nil || *got != "#50FA7B" {
		t.Fatalf("string token fallback color = %v, want #50FA7B", got)
	}
}
