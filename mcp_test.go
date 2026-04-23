package main

import (
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"testing"
)

func TestNewMCPBridge_AllocatesEphemeralPort(t *testing.T) {
	b, err := newMCPBridge(42)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}
	defer b.stop()
	if b.port <= 0 {
		t.Errorf("port should be > 0: %d", b.port)
	}
	if b.tabID != 42 {
		t.Errorf("tabID=%d want 42", b.tabID)
	}
	if b.ln == nil || b.server == nil {
		t.Errorf("bridge unfinished: ln=%v server=%v", b.ln, b.server)
	}
	// The listener must be accepting on the reported port.
	addr := "127.0.0.1:" + strconv.Itoa(b.port)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial bridge at %s: %v", addr, err)
	}
	_ = conn.Close()
}

func TestMCPBridge_StopIsIdempotent(t *testing.T) {
	b, err := newMCPBridge(1)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	b.stop()
	b.stop() // should not panic on a closed listener
	var nilB *mcpBridge
	nilB.stop() // nil-safety
}

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

func TestBuildApprovalBody_Deny(t *testing.T) {
	body := buildApprovalBody(false, map[string]any{"command": "rm -rf"}, nil)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if parsed["behavior"] != "deny" {
		t.Errorf("behavior=%v want deny", parsed["behavior"])
	}
	if _, ok := parsed["message"]; !ok {
		t.Error("deny body should include message")
	}
	if _, ok := parsed["updatedInput"]; ok {
		t.Error("deny body should NOT include updatedInput")
	}
}

func TestBuildApprovalBody_AllowWithoutRemember(t *testing.T) {
	in := map[string]any{"command": "ls"}
	body := buildApprovalBody(true, in, nil)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if parsed["behavior"] != "allow" {
		t.Errorf("behavior=%v want allow", parsed["behavior"])
	}
	upd, ok := parsed["updatedInput"].(map[string]any)
	if !ok || upd["command"] != "ls" {
		t.Errorf("updatedInput missing/wrong: %v", parsed["updatedInput"])
	}
	if _, ok := parsed["updatedPermissions"]; ok {
		t.Error("no remember → should not include updatedPermissions")
	}
}

func TestBuildApprovalBody_AllowWithRememberSession(t *testing.T) {
	rule := permissionRule{toolName: "Edit", ruleContent: "/a/b"}
	body := buildApprovalBody(true, nil, &rule)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if parsed["behavior"] != "allow" {
		t.Errorf("behavior=%v want allow", parsed["behavior"])
	}
	upd, _ := parsed["updatedInput"].(map[string]any)
	if upd == nil {
		t.Errorf("updatedInput should be a non-nil map (empty ok): %v", parsed["updatedInput"])
	}
	permsAny, ok := parsed["updatedPermissions"].([]any)
	if !ok || len(permsAny) != 1 {
		t.Fatalf("updatedPermissions missing or wrong shape: %v", parsed["updatedPermissions"])
	}
	p := permsAny[0].(map[string]any)
	if p["type"] != "addRules" {
		t.Errorf("type=%v want addRules", p["type"])
	}
	if p["destination"] != "session" {
		t.Errorf("destination=%v want session", p["destination"])
	}
	if p["behavior"] != "allow" {
		t.Errorf("inner behavior=%v want allow", p["behavior"])
	}
	rules := p["rules"].([]any)
	r0 := rules[0].(map[string]any)
	if r0["toolName"] != "Edit" || r0["ruleContent"] != "/a/b" {
		t.Errorf("inner rule=%+v", r0)
	}
}

func TestBuildApprovalBody_EmptyRuleContentBecomesNull(t *testing.T) {
	rule := permissionRule{toolName: "Glob"} // no ruleContent
	body := buildApprovalBody(true, map[string]any{"pattern": "*.md"}, &rule)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	perms := parsed["updatedPermissions"].([]any)
	r0 := perms[0].(map[string]any)["rules"].([]any)[0].(map[string]any)
	if _, ok := r0["ruleContent"]; ok {
		// ruleContent should marshal as JSON null and thus be present but nil.
		if v := r0["ruleContent"]; v != nil {
			t.Errorf("ruleContent=%v want null, JSON nil", v)
		}
	}
}

func TestBridge_RememberAndRuleAlwaysAllowed(t *testing.T) {
	b := &mcpBridge{alwaysAllow: map[permissionRule]struct{}{}}
	rule := permissionRule{toolName: "Read", ruleContent: "/x"}
	if b.ruleAlwaysAllowed(rule) {
		t.Error("empty allowlist must not claim allowed")
	}
	b.rememberAlwaysAllow(rule)
	if !b.ruleAlwaysAllowed(rule) {
		t.Error("rule should be allowed after remember")
	}
	empty := permissionRule{}
	b.rememberAlwaysAllow(empty)
	if b.ruleAlwaysAllowed(empty) {
		t.Error("empty toolName must never be allowed")
	}
}

func TestBridge_AlwaysAllowIsGoroutineSafe(t *testing.T) {
	b := &mcpBridge{alwaysAllow: map[permissionRule]struct{}{}}
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := permissionRule{toolName: "Bash", ruleContent: strconv.Itoa(i)}
			b.rememberAlwaysAllow(r)
			b.ruleAlwaysAllowed(r)
		}(i)
	}
	wg.Wait()
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

func TestPermissionRuleFor_UnknownToolEmptyContent(t *testing.T) {
	r := permissionRuleFor("CustomTool", map[string]any{"anything": "x"})
	if r.ruleContent != "" {
		t.Errorf("unknown tool should leave ruleContent empty, got %+v", r)
	}
}
