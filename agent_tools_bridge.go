package main

import (
	"context"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func nativeBridgeTool[In, Out any](name, description string,
	run func(ctx context.Context, in In) (*mcp.CallToolResult, Out, error),
) fantasy.AgentTool {
	return tools.NativeBridgeTool(name, description, run)
}

func flattenNullableTypes(v any) {
	tools.FlattenNullableTypes(v)
}

func mcpResultText(res *mcp.CallToolResult) string {
	return tools.MCPResultText(res)
}

func agentLinearTools(env *agentToolEnv) []fantasy.AgentTool {
	cwd := func() string { return env.Cwd }
	return []fantasy.AgentTool{
		nativeBridgeTool("linear_list_issues", linearListToolDescription,
			func(ctx context.Context, in linearListInput) (*mcp.CallToolResult, linearListOutput, error) {
				return linearListCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_get_issue", linearGetToolDescription,
			func(ctx context.Context, in linearGetInput) (*mcp.CallToolResult, linearGetOutput, error) {
				return linearGetCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_update_issue", linearUpdateToolDescription,
			func(ctx context.Context, in linearUpdateInput) (*mcp.CallToolResult, linearUpdateOutput, error) {
				return linearUpdateCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_create_comment", linearCreateCommentToolDescription,
			func(ctx context.Context, in linearCommentInput) (*mcp.CallToolResult, linearCommentOutput, error) {
				return linearCreateCommentCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_create_issue", linearCreateIssueToolDescription,
			func(ctx context.Context, in linearCreateInput) (*mcp.CallToolResult, linearCreateOutput, error) {
				return linearCreateIssueCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_delete_issue", linearDeleteIssueToolDescription,
			func(ctx context.Context, in linearDeleteInput) (*mcp.CallToolResult, linearDeleteOutput, error) {
				return linearDeleteIssueCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_list_teams", linearListTeamsToolDescription,
			func(ctx context.Context, in linearListTeamsInput) (*mcp.CallToolResult, linearListTeamsOutput, error) {
				return linearListTeamsCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_list_users", linearListUsersToolDescription,
			func(ctx context.Context, in linearListUsersInput) (*mcp.CallToolResult, linearListUsersOutput, error) {
				return linearListUsersCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_list_labels", linearListLabelsToolDescription,
			func(ctx context.Context, in linearListLabelsInput) (*mcp.CallToolResult, linearListLabelsOutput, error) {
				return linearListLabelsCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_list_states", linearListStatesToolDescription,
			func(ctx context.Context, in linearListStatesInput) (*mcp.CallToolResult, linearListStatesOutput, error) {
				return linearListStatesCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_list_projects", linearListProjectsToolDescription,
			func(ctx context.Context, in linearListProjectsInput) (*mcp.CallToolResult, linearListProjectsOutput, error) {
				return linearListProjectsCore(ctx, cwd(), in)
			}),
		nativeBridgeTool("linear_list_cycles", linearListCyclesToolDescription,
			func(ctx context.Context, in linearListCyclesInput) (*mcp.CallToolResult, linearListCyclesOutput, error) {
				return linearListCyclesCore(ctx, cwd(), in)
			}),
	}
}
