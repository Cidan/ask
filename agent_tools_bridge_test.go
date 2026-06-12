package main

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func bridgeToolByName(t *testing.T, env *agentToolEnv, name string) fantasy.AgentTool {
	t.Helper()
	for _, tool := range agentBridgeTools(env) {
		if tool.Info().Name == name {
			return tool
		}
	}
	t.Fatalf("native bridge tool %q missing", name)
	return nil
}

func TestAgentBridgeTools_CoversEveryBridgeTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	want := []string{
		"linear_list_issues", "linear_get_issue", "linear_update_issue",
		"linear_create_comment", "linear_create_issue", "linear_delete_issue",
		"linear_list_teams", "linear_list_users", "linear_list_labels",
		"linear_list_states", "linear_list_projects", "linear_list_cycles",
		"workflow_list", "workflow_get", "workflow_create",
		"workflow_edit", "workflow_delete", "workflow_run",
	}
	got := map[string]bool{}
	for _, tool := range agentBridgeTools(env) {
		got[tool.Info().Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing native twin for %s", name)
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

func TestNativeBridgeTool_WorkflowCRUDRoundTrip(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)

	// Empty project: list is a summary + structured JSON.
	list := bridgeToolByName(t, env, "workflow_list")
	resp, err := list.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "workflow_list", Input: `{}`})
	if err != nil || resp.IsError {
		t.Fatalf("list: %+v %v", resp, err)
	}
	if !strings.Contains(resp.Content, "0 workflow(s) defined") {
		t.Errorf("list summary missing: %q", resp.Content)
	}

	create := bridgeToolByName(t, env, "workflow_create")
	resp, err = create.Run(context.Background(), fantasy.ToolCall{
		ID: "2", Name: "workflow_create",
		Input: `{"name":"review","steps":[{"name":"step1","provider":"deepseek","prompt":"review the issue"}]}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("create: %+v %v", resp, err)
	}
	if !strings.Contains(resp.Content, `workflow "review" created`) ||
		!strings.Contains(resp.Content, `"review the issue"`) {
		t.Errorf("create must return summary + structured JSON: %q", resp.Content)
	}

	// The definition persisted to the project config.
	items, err := workflowItemsForCwd(env.cwd)
	if err != nil || len(items) != 1 || items[0].Name != "review" {
		t.Fatalf("workflow must persist: %+v %v", items, err)
	}

	// Duplicate create gates.
	resp, _ = create.Run(context.Background(), fantasy.ToolCall{
		ID: "3", Name: "workflow_create",
		Input: `{"name":"review","steps":[{"name":"s","provider":"deepseek","prompt":"p"}]}`,
	})
	if !resp.IsError || !strings.Contains(resp.Content, "already exists") {
		t.Errorf("duplicate create must error: %+v", resp)
	}

	del := bridgeToolByName(t, env, "workflow_delete")
	resp, err = del.Run(context.Background(), fantasy.ToolCall{ID: "4", Name: "workflow_delete", Input: `{"name":"review"}`})
	if err != nil || resp.IsError {
		t.Fatalf("delete: %+v %v", resp, err)
	}
	items, _ = workflowItemsForCwd(env.cwd)
	if len(items) != 0 {
		t.Errorf("delete must persist: %+v", items)
	}
}

func TestNativeBridgeTool_WorkflowRunDispatches(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)
	env.tabID = 9

	create := bridgeToolByName(t, env, "workflow_create")
	if resp, err := create.Run(context.Background(), fantasy.ToolCall{
		ID: "1", Name: "workflow_create",
		Input: `{"name":"go","steps":[{"name":"s1","provider":"deepseek","prompt":"do it"}]}`,
	}); err != nil || resp.IsError {
		t.Fatalf("create: %+v %v", resp, err)
	}

	var spawned []spawnWorkflowTabMsg
	prev := mcpSpawnWorkflowTab
	mcpSpawnWorkflowTab = func(msg spawnWorkflowTabMsg) error {
		spawned = append(spawned, msg)
		return nil
	}
	t.Cleanup(func() { mcpSpawnWorkflowTab = prev })

	run := bridgeToolByName(t, env, "workflow_run")
	resp, err := run.Run(context.Background(), fantasy.ToolCall{ID: "2", Name: "workflow_run", Input: `{"name":"go"}`})
	if err != nil || resp.IsError {
		t.Fatalf("run: %+v %v", resp, err)
	}
	if len(spawned) != 1 || spawned[0].Workflow.Name != "go" ||
		spawned[0].OriginTabID != 9 || spawned[0].Cwd != env.cwd {
		t.Errorf("spawn msg wrong: %+v", spawned)
	}
	if !strings.Contains(resp.Content, "dispatched") {
		t.Errorf("run output: %q", resp.Content)
	}
}

func TestSetupAgentSessionTools_NoLoopbackAttachAndNativesPresent(t *testing.T) {
	if servers := agentSessionMCPServers(ProviderSessionArgs{MCPPort: 4242, Cwd: "/tmp"}, askConfig{}); len(servers) != 0 {
		t.Errorf("in-process sessions must not attach the loopback bridge: %+v", servers)
	}
	env, _ := newTestToolEnv(t)
	names := map[string]bool{}
	for _, tool := range agentBridgeTools(env) {
		names[tool.Info().Name] = true
	}
	if !names["workflow_run"] || !names["linear_list_issues"] {
		t.Error("native bridge twins must be present")
	}
}
