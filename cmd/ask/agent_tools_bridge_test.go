package main

import (
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/tools"
	"google.golang.org/genai"
)

func bridgeToolByName(t *testing.T, env *agentToolEnv, name string) tools.Tool {
	t.Helper()
	for _, tool := range agentLinearTools(env) {
		if tool.Name() == name {
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
		got[tool.Name()] = true
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
	info := tools.ExtractToolInfo(bridgeToolByName(t, env, "linear_list_issues"))
	q, ok := info.Parameters["query"].(map[string]any)
	if !ok {
		t.Fatalf("query property missing: %+v", info.Parameters)
	}
	if desc, _ := q["description"].(string); !strings.Contains(desc, "state:open") {
		t.Errorf("jsonschema field doc must reach the model: %+v", q)
	}

	get := tools.ExtractToolInfo(bridgeToolByName(t, env, "linear_get_issue"))
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
	info := tools.ExtractToolInfo(bridgeToolByName(t, env, "linear_list_issues"))
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
	upd := tools.ExtractToolInfo(bridgeToolByName(t, env, "linear_update_issue"))
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
	resp, err := tools.RunToolWithJSON(testAgentCtx(), tool, `{}`)
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
	resp, err := tools.RunToolWithJSON(testAgentCtx(), tool, `{not json`)
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
		names[tool.Name()] = true
	}
	if !names["linear_list_issues"] {
		t.Error("linear_* native bridge twins must be present in the deferred registry")
	}
}

func TestClearPlans_NotInLinearTools(t *testing.T) {
	env, _ := newTestToolEnv(t)
	for _, tool := range agentLinearTools(env) {
		if tool.Name() == "clear_plans" {
			t.Fatal("clear_plans must not live in agentLinearTools")
		}
	}
}

func TestNativeBridgeTool_AllWireSchemasClean(t *testing.T) {
	env, _ := newTestToolEnv(t)
	for _, tool := range agentLinearTools(env) {
		declProvider, ok := tool.(interface {
			Declaration() *genai.FunctionDeclaration
		})
		if !ok || declProvider.Declaration() == nil {
			t.Fatalf("tool %s missing declaration", tool.Name())
		}
		decl := declProvider.Declaration()
		walkForItemsAnyOfConflict(t, tool.Name(), decl.ParametersJsonSchema)
	}
}

func TestSetupAgentSessionTools_AllCoreToolsUseParametersJsonSchemaAndNoParametersConflict(t *testing.T) {
	isolateHome(t)
	tmpDir := t.TempDir()
	env, _ := newTestToolEnv(t)
	s := &agentSession{
		args: ProviderSessionArgs{
			Cwd:   tmpDir,
			TabID: 1,
		},
		env: env,
	}
	cfg := askConfig{}
	setupAgentSessionTools(s, cfg)

	if len(s.coreTools) == 0 {
		t.Fatal("expected coreTools to be populated")
	}

	adkTools, err := engine.AsADKTools(s.coreTools)
	if err != nil {
		t.Fatalf("AsADKTools failed: %v", err)
	}

	for _, tool := range adkTools {
		declProvider, ok := tool.(interface {
			Declaration() *genai.FunctionDeclaration
		})
		if !ok {
			// Some tools like preload_memory only inject instructions
			continue
		}
		decl := declProvider.Declaration()
		if decl == nil {
			continue
		}

		if decl.Parameters != nil {
			t.Errorf("tool %q has decl.Parameters set; GenAI/Vertex requires ParametersJsonSchema and forbids Parameters mixing", decl.Name)
		}
		if decl.ParametersJsonSchema == nil {
			t.Errorf("tool %q has nil ParametersJsonSchema", decl.Name)
		}
	}
}
