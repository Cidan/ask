package main

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"
)

func bridgeToolByName(t *testing.T, env *agentToolEnv, name string) fantasy.AgentTool {
	t.Helper()
	for _, tool := range agentLinearTools(env) {
		if tool.Info().Name == name {
			return tool
		}
	}
	t.Fatalf("native linear tool %q missing", name)
	return nil
}

func TestAgentLinearTools_CoversEveryLinearTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	want := []string{
		"linear_list_issues", "linear_get_issue", "linear_update_issue",
		"linear_create_comment", "linear_create_issue", "linear_delete_issue",
		"linear_list_teams", "linear_list_users", "linear_list_labels",
		"linear_list_states", "linear_list_projects", "linear_list_cycles",
	}
	got := map[string]bool{}
	for _, tool := range agentLinearTools(env) {
		got[tool.Info().Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing native linear twin for %s", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tool count %d want %d: %v", len(got), len(want), got)
	}
}

func TestNativeBridgeTool_SchemaCarriesJSONSchemaTags(t *testing.T) {
	env, _ := newTestToolEnv(t)
	info := bridgeToolByName(t, env, "linear_list_issues").Info()
	q, ok := info.Parameters["query"].(map[string]any)
	if !ok {
		t.Fatalf("query property missing: %+v", info.Parameters)
	}
	if desc, _ := q["description"].(string); !strings.Contains(desc, "state:open") {
		t.Errorf("jsonschema field doc must reach the model: %+v", q)
	}

	get := bridgeToolByName(t, env, "linear_get_issue").Info()
	var requiredNumber bool
	for _, r := range get.Required {
		if r == "number" {
			requiredNumber = true
		}
	}
	if !requiredNumber {
		t.Errorf("required fields must survive schema generation: %v", get.Required)
	}
}

func TestNativeBridgeTool_InjectsDescriptionPhrase(t *testing.T) {
	env, _ := newTestToolEnv(t)

	// Tools without their own description param get the injected
	// required phrase param so their calls render a headline too.
	info := bridgeToolByName(t, env, "linear_list_issues").Info()
	prop, ok := info.Parameters["description"].(map[string]any)
	if !ok {
		t.Fatalf("injected description param missing: %+v", info.Parameters)
	}
	if doc, _ := prop["description"].(string); doc != toolPhraseFieldDoc {
		t.Errorf("injected doc wrong: %q", doc)
	}
	var required bool
	for _, r := range info.Required {
		if r == "description" {
			required = true
		}
	}
	if !required {
		t.Errorf("injected description must be required: %v", info.Required)
	}

	// Tools whose input already uses "description" as real payload
	// (linear_update_issue: the issue's Markdown body) keep their own
	// schema untouched — no clobber, no forced requirement.
	upd := bridgeToolByName(t, env, "linear_update_issue").Info()
	uprop, ok := upd.Parameters["description"].(map[string]any)
	if !ok {
		t.Fatalf("linear_update_issue description param missing: %+v", upd.Parameters)
	}
	if doc, _ := uprop["description"].(string); !strings.Contains(doc, "Markdown") {
		t.Errorf("payload description doc was clobbered: %q", doc)
	}
	for _, r := range upd.Required {
		if r == "description" {
			t.Errorf("payload description must not become required: %v", upd.Required)
		}
	}
}

func TestNativeBridgeTool_LinearGateErrors(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)
	tool := bridgeToolByName(t, env, "linear_list_issues")
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "linear_list_issues", Input: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "not the active issue provider") {
		t.Errorf("unconfigured linear must gate: %+v", resp)
	}
}

func TestNativeBridgeTool_InvalidInputErrors(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tool := bridgeToolByName(t, env, "linear_get_issue")
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "linear_get_issue", Input: `{not json`})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "invalid parameters") {
		t.Errorf("malformed input must error: %+v", resp)
	}
}

func TestSetupAgentSessionTools_NoLoopbackAttachAndNativesPresent(t *testing.T) {
	if servers := agentSessionMCPServers(ProviderSessionArgs{MCPPort: 4242, Cwd: "/tmp"}, askConfig{}); len(servers) != 0 {
		t.Errorf("in-process sessions must not attach the loopback bridge: %+v", servers)
	}
	env, _ := newTestToolEnv(t)
	names := map[string]bool{}
	for _, tool := range agentLinearTools(env) {
		names[tool.Info().Name] = true
	}
	// The linear_* tools are the registry-resident bridge twins. The
	// workflow tools now live on the wire as core exceptions
	// (agent_tools_workflow.go); their presence in the registry would
	// mean a regression. Both are still assembled in the session — the
	// point of this test is the loopback bridge isn't attached.
	if !names["linear_list_issues"] {
		t.Error("linear_* native bridge twins must be present in the deferred registry")
	}
}

// TestClearPlans_NotInLinearTools sanity-checks that the clear_plans
// tool is NOT in agentLinearTools (it lives in the workflow core set
// now, not the linear twins). The full clear_plans behavior is
// covered by TestClearPlans_WorkflowCoreToolIdempotent in
// agent_tools_workflow_test.go.
func TestClearPlans_NotInLinearTools(t *testing.T) {
	env, _ := newTestToolEnv(t)
	for _, tool := range agentLinearTools(env) {
		if tool.Info().Name == "clear_plans" {
			t.Fatal("clear_plans must not live in agentLinearTools")
		}
	}
}

// TestNativeBridgeTool_AllWireSchemasClean runs the same
// Normalize-then-walk check the workflow suite uses, but across every
// linear_* native twin. Catches a future nullable field introduced
// into a linear input type, so the strict-validator bug can't sneak
// back in via a different tool family.
func TestNativeBridgeTool_AllWireSchemasClean(t *testing.T) {
	env, _ := newTestToolEnv(t)
	for _, tool := range agentLinearTools(env) {
		info := tool.Info()
		wire := map[string]any{
			"type":       "object",
			"properties": info.Parameters,
			"required":   info.Required,
		}
		schema.Normalize(wire)
		walkForItemsAnyOfConflict(t, info.Name, wire)
	}
}
