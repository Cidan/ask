package main

import (
	"context"

	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/tools"
)

type agentFetchParams = tools.FetchParams
type agentWebSearchParams = tools.WebSearchParams
type braveResult = tools.BraveResult

var (
	agentFetchClient  = tools.FetchClient
	braveSearchClient = tools.BraveSearchClient
)

func agentFetchTool(env *agentToolEnv) fantasy.AgentTool     { return tools.FetchTool(env) }
func agentWebSearchTool(env *agentToolEnv) fantasy.AgentTool { return tools.WebSearchTool(env) }

func htmlToText(src string) string        { return tools.HTMLToText(src) }
func collapseBlankLines(s string) string  { return tools.CollapseBlankLines(s) }
func braveSearch(ctx context.Context, apiKey, query string, count int) ([]braveResult, error) {
	return tools.BraveSearch(ctx, apiKey, query, count)
}
