package main

import (
	"github.com/Cidan/ask/pkg/engine"
)

type loadedContextDoc = engine.LoadedContextDoc

var (
	extractContextLinks     = engine.ExtractContextLinks
	stripFencedCodeBlocks   = engine.StripFencedCodeBlocks
	resolveContextLink      = engine.ResolveContextLink
	loadContextLinks        = engine.LoadContextLinks
	contextLinksPromptBlock = engine.ContextLinksPromptBlock
	ruleLinkedDocs          = engine.RuleLinkedDocs
)
