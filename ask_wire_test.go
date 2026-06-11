package main

import (
	"encoding/json"
	"testing"
)

func TestConvertMCPQuestions_KindMapping(t *testing.T) {
	qs := []mcpQuestion{
		{Kind: "pick_one", Prompt: "one", Options: []mcpOption{{Label: "a"}, {Label: "b"}}, AllowCustom: true},
		{Kind: "pick_many", Prompt: "many", Options: []mcpOption{{Label: "x"}}, AllowCustom: true},
		{Kind: "pick_diagram", Prompt: "dia", Options: []mcpOption{{Label: "d", Diagram: "▓"}}, AllowCustom: true},
		{Kind: "unknown", Prompt: "fallback", Options: []mcpOption{{Label: "z"}}},
	}
	out := convertMCPQuestions(qs)
	if len(out) != 4 {
		t.Fatalf("len=%d want 4", len(out))
	}
	if out[0].kind != qPickOne {
		t.Errorf("[0] kind=%v want qPickOne", out[0].kind)
	}
	if out[1].kind != qPickMany {
		t.Errorf("[1] kind=%v want qPickMany", out[1].kind)
	}
	if out[2].kind != qPickDiagram {
		t.Errorf("[2] kind=%v want qPickDiagram", out[2].kind)
	}
	if out[3].kind != qPickOne {
		t.Errorf("[3] unknown kind should fall back to qPickOne, got %v", out[3].kind)
	}
}

func TestConvertMCPQuestions_AllowCustomAppendsEnterYourOwn(t *testing.T) {
	// pick_one + AllowCustom → options has trailing "Enter your own"
	qs := []mcpQuestion{{Kind: "pick_one", Options: []mcpOption{{Label: "a"}}, AllowCustom: true}}
	out := convertMCPQuestions(qs)
	if len(out[0].options) != 2 || out[0].options[1] != "Enter your own" {
		t.Errorf("pick_one AllowCustom options=%v", out[0].options)
	}

	// pick_many same
	qs = []mcpQuestion{{Kind: "pick_many", Options: []mcpOption{{Label: "a"}}, AllowCustom: true}}
	out = convertMCPQuestions(qs)
	if len(out[0].options) != 2 || out[0].options[1] != "Enter your own" {
		t.Errorf("pick_many AllowCustom options=%v", out[0].options)
	}

	// pick_diagram + AllowCustom → still no custom trailer
	qs = []mcpQuestion{{Kind: "pick_diagram", Options: []mcpOption{{Label: "d"}}, AllowCustom: true}}
	out = convertMCPQuestions(qs)
	if len(out[0].options) != 1 {
		t.Errorf("pick_diagram AllowCustom must not add Enter your own; options=%v", out[0].options)
	}
}

func TestConvertMCPAnswers_EmptyPicksReturnsEmptySlice(t *testing.T) {
	qs := []mcpQuestion{{Kind: "pick_one", Options: []mcpOption{{Label: "a"}, {Label: "b"}}}}
	answers := []qAnswer{{picks: map[int]bool{}}}
	out := convertMCPAnswers(qs, answers)
	if len(out) != 1 {
		t.Fatalf("len=%d want 1", len(out))
	}
	if out[0].Picks == nil {
		t.Errorf("empty picks should produce []string{} not nil")
	}
	if len(out[0].Picks) != 0 {
		t.Errorf("empty picks should be empty, got %v", out[0].Picks)
	}
}

func TestConvertMCPAnswers_PassesCustomAndNote(t *testing.T) {
	qs := []mcpQuestion{{
		Kind: "pick_one", Options: []mcpOption{{Label: "a"}}, AllowCustom: true,
	}}
	// picks: index 0 = "a", index 1 = Enter your own
	answers := []qAnswer{{picks: map[int]bool{1: true}, custom: "freeform", note: "ditto"}}
	out := convertMCPAnswers(qs, answers)
	if len(out[0].Picks) != 0 {
		t.Errorf("only custom selected; Picks should be empty: %v", out[0].Picks)
	}
	if out[0].Custom != "freeform" {
		t.Errorf("Custom=%q want freeform", out[0].Custom)
	}
	if out[0].Note != "ditto" {
		t.Errorf("Note=%q want ditto", out[0].Note)
	}
}

func TestConvertMCPAnswers_DropsCustomWhenNotAllowed(t *testing.T) {
	qs := []mcpQuestion{{Kind: "pick_one", Options: []mcpOption{{Label: "a"}}}} // AllowCustom=false
	answers := []qAnswer{{picks: map[int]bool{0: true}, custom: "ignored"}}
	out := convertMCPAnswers(qs, answers)
	if out[0].Custom != "" {
		t.Errorf("custom should be dropped when AllowCustom=false, got %q", out[0].Custom)
	}
	if len(out[0].Picks) != 1 || out[0].Picks[0] != "a" {
		t.Errorf("Picks=%v want [a]", out[0].Picks)
	}
}

func TestConvertMCPAnswers_RoundTripJSON(t *testing.T) {
	qs := []mcpQuestion{{Kind: "pick_many", Options: []mcpOption{{Label: "a"}, {Label: "b"}}, AllowCustom: true}}
	answers := []qAnswer{{picks: map[int]bool{0: true, 1: true, 2: true}, custom: "cust", note: "hello"}}
	out := convertMCPAnswers(qs, answers)
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("convertMCPAnswers output must marshal: %v", err)
	}
	if len(out[0].Picks) != 2 {
		t.Errorf("Picks=%v want [a b]", out[0].Picks)
	}
}

func TestPermissionRuleFor_FileTools(t *testing.T) {
	for _, tool := range []string{"Edit", "Write", "MultiEdit", "NotebookEdit", "Read"} {
		r := permissionRuleFor(tool, map[string]any{"file_path": "/a/b"})
		if r.toolName != tool || r.ruleContent != "/a/b" {
			t.Errorf("%s: rule=%+v", tool, r)
		}
	}
	r := permissionRuleFor("Edit", map[string]any{})
	if r.toolName != "Edit" || r.ruleContent != "" {
		t.Errorf("missing file_path should yield empty ruleContent, got %+v", r)
	}
}

func TestPermissionRuleFor_Bash(t *testing.T) {
	r := permissionRuleFor("Bash", map[string]any{"command": "ls -la"})
	if r.toolName != "Bash" || r.ruleContent != "ls -la" {
		t.Errorf("bash rule=%+v", r)
	}
}

func TestPermissionRuleFor_OtherTool(t *testing.T) {
	r := permissionRuleFor("Glob", map[string]any{"pattern": "*.go"})
	if r.toolName != "Glob" || r.ruleContent != "" {
		t.Errorf("non-file/bash tools should leave ruleContent empty, got %+v", r)
	}
}

func TestConvertMCPAnswers_PicksMappedByIndex(t *testing.T) {
	qs := []mcpQuestion{{
		Kind: "pick_many",
		Options: []mcpOption{
			{Label: "red"}, {Label: "green"}, {Label: "blue"},
		},
		AllowCustom: false,
	}}
	answers := []qAnswer{{picks: map[int]bool{0: true, 2: true}}}
	out := convertMCPAnswers(qs, answers)
	if len(out[0].Picks) != 2 {
		t.Fatalf("Picks=%v want 2", out[0].Picks)
	}
	if out[0].Picks[0] != "red" || out[0].Picks[1] != "blue" {
		t.Errorf("order should preserve option order; got %v", out[0].Picks)
	}
}
