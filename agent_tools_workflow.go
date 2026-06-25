package main

import (
	"context"

	"charm.land/fantasy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// agent_tools_workflow.go hoists the ask-built-in workflow tools onto
// the core wire toolset — workflow_list, workflow_get, workflow_create,
// workflow_edit, workflow_delete, workflow_copy, workflow_run, and
// clear_plans. They used to live in the deferred registry (reached via
// search_tools → invoke_tool), but the two-stage workflow guard in
// agent_tools_todos.go forces the model to call workflow_list and
// workflow_run as a precondition for any multi-step work, so paying an
// extra search_tools round-trip on every guard interaction is pure
// overhead. The linear_* bridge twins and external MCP tools stay in
// the deferred registry; only the ask-built-in workflow tools are
// promoted.
//
// The runtime disarms the workflow guard inside the workflow_list and
// workflow_run closures (env.markWorkflowsChecked and
// env.markWorkflowRunDispatched). The disarm must live here — not in
// invoke_tool — so a model that follows the new direct-call pattern
// (the only legal one now that the tools are core) still disarms the
// guard. markWorkflowsChecked and markWorkflowRunDispatched are
// idempotent one-shot latches; calling them from anywhere is safe.
//
// The tool bodies reuse the nativeBridgeTool adapter from
// agent_tools_bridge.go (and its cwd-parameterized cores in
// mcp_workflows.go), so the wire schemas are byte-identical to the
// prior registry shape — jsonschema field docs survive verbatim, and
// tools whose input declares a real "description" payload (workflow_
// create / workflow_edit) keep their own schema and do NOT get the
// injected phrase field.
//
// Note: nativeBridgeTool runs every input schema through
// flattenNullableTypes before handing it back. That step is what
// keeps the workflow_create / workflow_edit "steps" array (a
// []workflowStepView with omitempty) and the *string "description"
// payload from emitting `type: ["null", X]` — the strict Moonshot
// schema validator rejects the downstream anyOf rewrite as
// "conflicting keywords found in anyOf with parent". Don't
// re-introduce a hand-rolled workflow tool that bypasses the bridge
// adapter; you'll re-introduce the bug.
func agentWorkflowTools(env *agentToolEnv) []fantasy.AgentTool {
	cwd := func() string { return env.cwd }
	return []fantasy.AgentTool{
		nativeBridgeTool("workflow_list", workflowListToolDescription,
			func(_ context.Context, in workflowListInput) (*mcp.CallToolResult, workflowListOutput, error) {
				env.markWorkflowsChecked()
				return workflowListCore(cwd(), in)
			}),
		nativeBridgeTool("workflow_get", workflowGetToolDescription,
			func(_ context.Context, in workflowGetInput) (*mcp.CallToolResult, workflowGetOutput, error) {
				return workflowGetCore(cwd(), in)
			}),
		nativeBridgeTool("workflow_create", workflowCreateToolDescription,
			func(_ context.Context, in workflowCreateInput) (*mcp.CallToolResult, workflowCreateOutput, error) {
				return workflowCreateCore(cwd(), in)
			}),
		nativeBridgeTool("workflow_edit", workflowEditToolDescription,
			func(_ context.Context, in workflowEditInput) (*mcp.CallToolResult, workflowEditOutput, error) {
				return workflowEditCore(cwd(), in)
			}),
		nativeBridgeTool("workflow_delete", workflowDeleteToolDescription,
			func(_ context.Context, in workflowDeleteInput) (*mcp.CallToolResult, workflowDeleteOutput, error) {
				return workflowDeleteCore(cwd(), in)
			}),
		nativeBridgeTool("workflow_copy", workflowCopyToolDescription,
			func(_ context.Context, in workflowCopyInput) (*mcp.CallToolResult, workflowCopyOutput, error) {
				return workflowCopyCore(cwd(), in)
			}),
		nativeBridgeTool("workflow_run", workflowRunToolDescription,
			func(_ context.Context, in workflowRunInput) (*mcp.CallToolResult, workflowRunOutput, error) {
				if env.planningMode.Load() {
					return &mcp.CallToolResult{
						Content: []mcp.Content{
							&mcp.TextContent{
								Text: "Planning mode is ON. Workflows are currently blocked.",
							},
						},
					}, workflowRunOutput{}, nil
				}
				env.markWorkflowsChecked()
				env.markWorkflowRunDispatched()
				return workflowRunCore(cwd(), env.tabID, in)
			}),
		nativeBridgeTool("clear_plans", clearPlansToolDescription,
			func(_ context.Context, in clearPlansInput) (*mcp.CallToolResult, clearPlansOutput, error) {
				return clearPlansCore(cwd(), in)
			}),
	}
}
