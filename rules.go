package main

import (
	"charm.land/fantasy"
	"github.com/Cidan/ask/pkg/engine"
)

type askRule = engine.Rule
type ruleScope = engine.RuleScope
type contextAwareTool = engine.ContextAwareTool

var (
	ruleSearchScopes      = engine.RuleSearchScopes
	discoverRules         = engine.DiscoverRules
	parseRuleFile         = engine.ParseRuleFile
	parseRuleFrontmatter  = engine.ParseRuleFrontmatter
	parsePathsField       = engine.ParsePathsField
	rulesPromptBlock      = engine.RulesPromptBlock
	wrapContextAwareTools = func(tools []fantasy.AgentTool, cwd string, rules []askRule) []fantasy.AgentTool {
		return engine.WrapContextAwareTools(tools, cwd, rules)
	}
)
