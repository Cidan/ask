package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"charm.land/fantasy/schema"
)

// workflowToolByName looks up a tool by name in the workflow core tool
// set (agent_tools_workflow.go). Mirrors bridgeToolByName in
// agent_tools_bridge_test.go.
func workflowToolByName(t *testing.T, env *agentToolEnv, name string) fantasy.AgentTool {
	t.Helper()
	for _, tool := range agentWorkflowTools(env) {
		if tool.Info().Name == name {
			return tool
		}
	}
	t.Fatalf("workflow core tool %q missing", name)
	return nil
}

func TestAgentWorkflowTools_CoversEveryWorkflowTool(t *testing.T) {
	env, _ := newTestToolEnv(t)
	want := []string{
		"workflow_list", "workflow_get", "workflow_create",
		"workflow_edit", "workflow_delete", "workflow_copy", "workflow_run",
		"clear_plans",
	}
	got := map[string]bool{}
	for _, tool := range agentWorkflowTools(env) {
		got[tool.Info().Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing workflow core tool %s", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tool count %d want %d: %v", len(got), len(want), got)
	}
}

func TestNativeBridgeTool_WorkflowCRUDRoundTrip(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)

	// Empty project: list is a summary + structured JSON.
	list := workflowToolByName(t, env, "workflow_list")
	resp, err := list.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "workflow_list", Input: `{}`})
	if err != nil || resp.IsError {
		t.Fatalf("list: %+v %v", resp, err)
	}
	if !strings.Contains(resp.Content, "0 workflow(s) defined") {
		t.Errorf("list summary missing: %q", resp.Content)
	}

	create := workflowToolByName(t, env, "workflow_create")
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

	del := workflowToolByName(t, env, "workflow_delete")
	resp, err = del.Run(context.Background(), fantasy.ToolCall{ID: "4", Name: "workflow_delete", Input: `{"name":"review"}`})
	if err != nil || resp.IsError {
		t.Fatalf("delete: %+v %v", resp, err)
	}
	items, _ = workflowItemsForCwd(env.cwd)
	if len(items) != 0 {
		t.Errorf("delete must persist: %+v", items)
	}
}

func TestNativeBridgeTool_WorkflowDescriptionRoundTrip(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)

	create := workflowToolByName(t, env, "workflow_create")
	resp, err := create.Run(context.Background(), fantasy.ToolCall{
		ID: "1", Name: "workflow_create",
		Input: `{"name":"ship","description":"Use for any code change you ship.","steps":[{"name":"s","provider":"deepseek","prompt":"p"}]}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("create: %+v %v", resp, err)
	}

	// list surfaces the description so the agent can judge fit.
	list := workflowToolByName(t, env, "workflow_list")
	resp, err = list.Run(context.Background(), fantasy.ToolCall{ID: "2", Name: "workflow_list", Input: `{}`})
	if err != nil || resp.IsError {
		t.Fatalf("list: %+v %v", resp, err)
	}
	if !strings.Contains(resp.Content, "Use for any code change you ship.") {
		t.Errorf("workflow_list must surface the description: %q", resp.Content)
	}

	// edit replaces the description; omitting it would leave it unchanged.
	edit := workflowToolByName(t, env, "workflow_edit")
	resp, err = edit.Run(context.Background(), fantasy.ToolCall{
		ID: "3", Name: "workflow_edit",
		Input: `{"name":"ship","description":"Updated purpose statement."}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("edit: %+v %v", resp, err)
	}
	items, err := workflowItemsForCwd(env.cwd)
	if err != nil || len(items) != 1 || items[0].Description != "Updated purpose statement." {
		t.Fatalf("edit must persist new description: %+v %v", items, err)
	}

	// Omitting description on a later edit leaves it intact (rename only).
	resp, err = edit.Run(context.Background(), fantasy.ToolCall{
		ID: "4", Name: "workflow_edit",
		Input: `{"name":"ship","new_name":"deploy"}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("rename edit: %+v %v", resp, err)
	}
	items, _ = workflowItemsForCwd(env.cwd)
	if len(items) != 1 || items[0].Name != "deploy" || items[0].Description != "Updated purpose statement." {
		t.Errorf("omitted description must stay unchanged across a rename: %+v", items)
	}

	// get returns the description.
	get := workflowToolByName(t, env, "workflow_get")
	resp, err = get.Run(context.Background(), fantasy.ToolCall{ID: "5", Name: "workflow_get", Input: `{"name":"deploy"}`})
	if err != nil || resp.IsError {
		t.Fatalf("get: %+v %v", resp, err)
	}
	if !strings.Contains(resp.Content, "Updated purpose statement.") {
		t.Errorf("workflow_get must return the description: %q", resp.Content)
	}
}

func TestNativeBridgeTool_WorkflowRunDispatches(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)
	env.tabID = 9

	create := workflowToolByName(t, env, "workflow_create")
	if resp, err := create.Run(context.Background(), fantasy.ToolCall{
		ID: "1", Name: "workflow_create",
		Input: `{"name":"go","steps":[{"name":"s1","provider":"deepseek","prompt":"do it"}]}`,
	}); err != nil || resp.IsError {
		t.Fatalf("create: %+v %v", resp, err)
	}

	var (
		mu      sync.Mutex
		spawned []spawnWorkflowTabMsg
	)
	prev := mcpSpawnWorkflowTab
	mcpSpawnWorkflowTab = func(msg spawnWorkflowTabMsg) error {
		mu.Lock()
		spawned = append(spawned, msg)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { mcpSpawnWorkflowTab = prev })

	run := workflowToolByName(t, env, "workflow_run")
	resp, err := run.Run(context.Background(), fantasy.ToolCall{ID: "2", Name: "workflow_run", Input: `{"name":"go","append":"full plan and context"}`})
	if err != nil || resp.IsError {
		t.Fatalf("run: %+v %v", resp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(spawned) != 1 || spawned[0].Workflow.Name != "go" ||
		spawned[0].OriginTabID != 9 || spawned[0].Cwd != env.cwd {
		t.Errorf("spawn msg wrong: %+v", spawned)
	}
	if !strings.Contains(resp.Content, "dispatched") {
		t.Errorf("run output: %q", resp.Content)
	}
}

// TestWorkflowRun_AppendIsRequiredInSchema pins that the generated
// workflow_run schema marks `append` required — dropping omitempty from
// the field is what forces the model to supply the full plan, and the
// jsonschema generator infers "required" from the absence of omitempty.
// A future edit that re-adds omitempty would silently make it optional;
// this guards against that.
func TestWorkflowRun_AppendIsRequiredInSchema(t *testing.T) {
	env, _ := newTestToolEnv(t)
	var run fantasy.AgentTool
	for _, tool := range agentWorkflowTools(env) {
		if tool.Info().Name == "workflow_run" {
			run = tool
			break
		}
	}
	if run == nil {
		t.Fatal("workflow_run tool not found")
	}
	required := map[string]bool{}
	for _, r := range run.Info().Required {
		required[r] = true
	}
	if !required["name"] {
		t.Error("name must be required")
	}
	if !required["append"] {
		t.Errorf("append must be required; got required=%v", run.Info().Required)
	}
}

// TestClearPlans_WorkflowCoreToolIdempotent: the clear_plans core tool
// is wired, clears ask/plans/ children (leaving the dir itself), and is
// idempotent over empty or missing directories. Mirrors the prior
// registry-tool coverage now that the tool lives on the wire.
func TestClearPlans_WorkflowCoreToolIdempotent(t *testing.T) {
	isolateHome(t)
	env, _ := newTestToolEnv(t)

	tool := workflowToolByName(t, env, "clear_plans")
	if tool == nil {
		t.Fatal("clear_plans tool not found in agentWorkflowTools")
	}
	info := tool.Info()
	if info.Name != "clear_plans" {
		t.Fatalf("name: %s", info.Name)
	}
	if !strings.Contains(info.Description, "Clear the workflow plans directory") {
		t.Errorf("description missing expected text: %s", info.Description)
	}

	// Clear when ask/plans/ does not exist: succeeds (no-op).
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID: "1", Name: "clear_plans", Input: `{}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("clear_plans on absent dir: %v %+v", err, resp)
	}
	if !strings.Contains(resp.Content, "cleared") {
		t.Errorf("response should confirm cleared; got %q", resp.Content)
	}

	// Create some plan files.
	base := filepath.Join(env.cwd, "ask", "plans")
	if err := os.MkdirAll(filepath.Join(base, "start"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "start", "plan.md"), []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Clear: removes start/ but leaves ask/plans/.
	resp, err = tool.Run(context.Background(), fantasy.ToolCall{
		ID: "2", Name: "clear_plans", Input: `{}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("clear_plans with contents: %v %+v", err, resp)
	}
	if _, e := os.Stat(filepath.Join(base, "start")); !os.IsNotExist(e) {
		t.Error("start/ should be removed after clear")
	}
	if _, e := os.Stat(base); os.IsNotExist(e) {
		t.Error("ask/plans/ itself should survive clear")
	}

	// Clear again on empty dir: still succeeds.
	resp, err = tool.Run(context.Background(), fantasy.ToolCall{
		ID: "3", Name: "clear_plans", Input: `{}`,
	})
	if err != nil || resp.IsError {
		t.Fatalf("clear_plans second time: %v %+v", err, resp)
	}
}

// TestAgentWorkflowTools_DisarmHooksFire checks the workflow-guard
// disarm hooks the plan pinned in agent_tools_workflow.go. After the
// promotion, the disarm is in the tool closures (not in invoke_tool),
// so a direct workflow_list / workflow_run call clears the guard.
func TestAgentWorkflowTools_DisarmHooksFire(t *testing.T) {
	isolateHome(t)
	cwd := t.TempDir()
	if err := saveAllWorkflows(cwd, []workflowDef{
		{Name: "ship-it", Scope: workflowScopeRepo, Steps: []workflowStep{
			{Name: "do", Provider: "deepseek", Model: "deepseek-chat", Prompt: "go"},
		}},
	}); err != nil {
		t.Fatalf("saveAllWorkflows: %v", err)
	}
	// env must point at the seeded cwd so workflow_run resolves
	// "ship-it" (env.cwd is the only place workflow tools look).
	env := newAgentToolEnv(cwd, 1, true, true, false, func(tea.Msg) {})

	list := workflowToolByName(t, env, "workflow_list")
	if r, _ := list.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "workflow_list", Input: `{}`}); r.IsError {
		t.Fatalf("workflow_list: %q", r.Content)
	}
	if !env.workflowsChecked {
		t.Error("calling workflow_list core must mark workflowsChecked")
	}

	// workflow_run also marks both flags; capture the spawn so the test
	// can run unattended (mcpSpawnWorkflowTab is installed by the test
	// run — we restore it after).
	prev := mcpSpawnWorkflowTab
	mcpSpawnWorkflowTab = func(spawnWorkflowTabMsg) error { return nil }
	t.Cleanup(func() { mcpSpawnWorkflowTab = prev })
	run := workflowToolByName(t, env, "workflow_run")
	if r, _ := run.Run(context.Background(), fantasy.ToolCall{
		ID: "2", Name: "workflow_run", Input: `{"name":"ship-it","append":"plan"}`,
	}); r.IsError {
		t.Fatalf("workflow_run: %q", r.Content)
	}
	if !env.workflowRunDispatched {
		t.Error("calling workflow_run core must mark workflowRunDispatched")
	}
}

// TestNativeBridgeTool_FlattensNullableTypeArrays pins the wire shape
// the bridge adapter emits. jsonschema-go tags *string and `omitempty`
// slices as `type: ["null", "X"]`; the strict Moonshot / OpenAI
// schema validators reject the `anyOf` rewrite fantasy's downstream
// schema.Normalize would otherwise produce (its array branch carries
// its own `items: {}` while the parent keeps its real `items` — the
// "conflicting keywords" error that surfaced on every workflow_create
// / workflow_edit call against kimi). The adapter flattens the
// nullable type down to a single non-null type, so the normalize
// rewrite has nothing to do.
func TestNativeBridgeTool_FlattensNullableTypeArrays(t *testing.T) {
	env, _ := newTestToolEnv(t)
	tools := map[string]fantasy.AgentTool{}
	for _, tool := range agentWorkflowTools(env) {
		tools[tool.Info().Name] = tool
	}
	cases := []struct {
		name string
		// fields to inspect on Parameters
		fields map[string]struct {
			wantType string // expected single type
		}
	}{
		{
			name: "workflow_create",
			fields: map[string]struct{ wantType string }{
				"steps": {"array"},
			},
		},
		{
			name: "workflow_edit",
			fields: map[string]struct{ wantType string }{
				"steps":       {"array"},
				"description": {"string"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := tools[tc.name]
			if !ok {
				t.Fatalf("tool %q missing from workflow core set", tc.name)
			}
			params := tool.Info().Parameters
			for fname, want := range tc.fields {
				prop, ok := params[fname].(map[string]any)
				if !ok {
					t.Fatalf("%s: property %q missing or not a map: %+v", tc.name, fname, params[fname])
				}
				typ, ok := prop["type"].(string)
				if !ok {
					t.Errorf("%s.%s: type must be a single string, got %T %v", tc.name, fname, prop["type"], prop["type"])
					continue
				}
				if typ != want.wantType {
					t.Errorf("%s.%s: type = %q, want %q", tc.name, fname, typ, want.wantType)
				}
				if prop["anyOf"] != nil {
					t.Errorf("%s.%s: must not carry an anyOf (Normalize only adds anyOf from type-arrays, which we've eliminated)", tc.name, fname)
				}
			}
		})
	}
}

// TestNativeBridgeTool_WireSchemaNoConflictingItems walks the wire
// shape fantasy ships — i.e. the bridge adapter's output, wrapped
// like agent.prepareTools does, then run through schema.Normalize —
// and asserts that no node carries both a parent `items` and an
// `anyOf` whose array branch also defines `items`. That's the exact
// pattern Moonshot's strict validator rejects with "conflicting
// keywords found in anyOf with parent: keywords (items) are defined
// on the parent schema and inside anyOf".
func TestNativeBridgeTool_WireSchemaNoConflictingItems(t *testing.T) {
	env, _ := newTestToolEnv(t)
	for _, tool := range agentWorkflowTools(env) {
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

// walkForAnyOfItemsConflict recursively scans a JSON-Schema-shaped
// tree and fails the test the moment it sees a node that has both
// `items` at its own level AND an `anyOf` array whose branches also
// define `items`. That node is what strict validators (Moonshot,
// OpenAI strict) reject as "conflicting keywords found in anyOf with
// parent".
func walkForItemsAnyOfConflict(t *testing.T, toolName string, node any) {
	t.Helper()
	var walk func(path string, n any)
	walk = func(path string, n any) {
		switch v := n.(type) {
		case map[string]any:
			if _, hasItems := v["items"]; hasItems {
				if ao, ok := v["anyOf"].([]any); ok {
					for _, b := range ao {
						bm, ok := b.(map[string]any)
						if !ok {
							continue
						}
						if _, hasBranchItems := bm["items"]; hasBranchItems {
							raw, _ := json.MarshalIndent(v, "", "  ")
							t.Errorf("%s at %s: parent has items AND anyOf branch has items (conflicting keywords):\n%s", toolName, path, string(raw))
							return
						}
					}
				}
			}
			for k, child := range v {
				walk(path+"."+k, child)
			}
		case []any:
			for i, item := range v {
				walk(path+fmt.Sprintf("[%d]", i), item)
			}
		}
	}
	walk("$", node)
}

// TestFlattenNullableTypes pins the nullable-type-array rewriter used
// by nativeBridgeTool. It must:
//   - drop "null" from type arrays of any length, reducing
//     ["null","array"] → "array" and ["null","string"] → "string"
//   - leave non-null multi-type arrays alone (we don't generate them
//     today, but the helper mustn't break if a future schema does)
//   - recurse into nested maps and arrays
//   - remove the type key entirely when the array contained only
//     "null" (defensive — should not happen for ask's schemas)
//   - leave single-string type fields untouched
//   - leave maps with no type key untouched
func TestFlattenNullableTypes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{
			name: "nullable array with items",
			in: map[string]any{
				"type":  []any{"null", "array"},
				"items": map[string]any{"type": "object"},
			},
			want: map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "object"},
			},
		},
		{
			name: "nullable string",
			in: map[string]any{
				"type":        []any{"null", "string"},
				"description": "x",
			},
			want: map[string]any{
				"type":        "string",
				"description": "x",
			},
		},
		{
			name: "null first in type array",
			in:   map[string]any{"type": []any{"null", "string"}},
			want: map[string]any{"type": "string"},
		},
		{
			name: "no type key",
			in:   map[string]any{"description": "x"},
			want: map[string]any{"description": "x"},
		},
		{
			name: "non-null single type untouched",
			in:   map[string]any{"type": "string"},
			want: map[string]any{"type": "string"},
		},
		{
			name: "multi-type non-null array unchanged",
			in:   map[string]any{"type": []any{"string", "integer"}},
			want: map[string]any{"type": []any{"string", "integer"}},
		},
		{
			name: "all-null type array drops the key",
			in:   map[string]any{"type": []any{"null"}, "description": "x"},
			want: map[string]any{"description": "x"},
		},
		{
			name: "recurses into nested maps",
			in: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": []any{"null", "string"}},
					"b": map[string]any{"type": "integer"},
				},
			},
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "string"},
					"b": map[string]any{"type": "integer"},
				},
			},
		},
		{
			name: "recurses into arrays of schemas",
			in: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"x": map[string]any{
						"type": "array",
						"items": []any{
							map[string]any{"type": []any{"null", "string"}},
							map[string]any{"type": "integer"},
						},
					},
				},
			},
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"x": map[string]any{
						"type": "array",
						"items": []any{
							map[string]any{"type": "string"},
							map[string]any{"type": "integer"},
						},
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flattenNullableTypes(tc.in)
			got, _ := json.Marshal(tc.in)
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) {
				t.Errorf("flattenNullableTypes(%s):\n got  %s\n want %s", tc.name, got, want)
			}
		})
	}
}
