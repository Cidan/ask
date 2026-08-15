package main

import (
	"github.com/Cidan/ask/pkg/engine"
)

type askSkill = engine.Skill

var (
	skillSearchDirs          = engine.SkillSearchDirs
	discoverSkills           = engine.DiscoverSkills
	skillsPromptBlock        = engine.SkillsPromptBlock
	expandSkillInvocation    = engine.ExpandSkillInvocation
	parseMarkdownFrontmatter = engine.ParseMarkdownFrontmatter
)
