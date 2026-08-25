package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/Cidan/ask/pkg/engine"
)

// The browser shares the model picker's box, geometry, divider, scroll
// window, fuzzy match, and glamour renderer; only the rows and the
// detail pane are its own.

func (m model) skillsBrowserOverlay(width, height int) string {
	s := m.skillsBrowser
	if s == nil || m.mode != modeSkillsBrowser || width < 24 || height < 8 {
		return ""
	}
	_, _, listW, detailW, innerH := modelPickerGeometry(width, height)
	left := m.renderSkillsBrowserList(s, listW, innerH)
	right := m.renderSkillsBrowserDetail(s, detailW, innerH)
	div := modelPickerDividerStyle.Render("│")
	lines := make([]string, innerH)
	for i := range lines {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines[i] = padRight(clipText(l, listW), listW) + " " + div + " " + padRight(clipText(r, detailW), detailW)
	}
	return modelPickerBoxStyle.Render(strings.Join(lines, "\n"))
}

func skillsLensTabs(s *skillsBrowserState, w int) string {
	render := func(label string, active bool) string {
		if active {
			return themePickerRowStyle.Render(" " + label + " ")
		}
		return dimStyle.Render(" " + label + " ")
	}
	line := render("Installed", s.lens == skillsLensInstalled) + " " + render("Marketplace", s.lens == skillsLensMarketplace)
	return clipText(line, w)
}

func (m model) renderSkillsBrowserList(s *skillsBrowserState, w, h int) []string {
	lines := make([]string, 0, h)
	lines = append(lines, skillsLensTabs(s, w))
	lines = append(lines, configPromptStyle.Render("> ")+filterPromptLine(s.query, "Type to search"))
	lines = append(lines, "")

	rowsH := h - 5
	if rowsH < 1 {
		rowsH = 1
	}
	rows := s.rows()
	if len(rows) == 0 {
		lines = append(lines, dimStyle.Render("  (no matches)"))
	}
	start, end := modelPickerWindow(len(rows), s.cursor, rowsH)
	for i := start; i < end; i++ {
		lines = append(lines, renderSkillsBrowserRow(rows[i], w, i == s.cursor))
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	if len(lines) > h-1 {
		lines = lines[:h-1]
	}
	help := "tab lens · ↑↓ · enter · ^n new · ^d delete · mcp: space toggle ^o auth · ^a add mkt · ^r refresh · esc"
	if s.lens == skillsLensMarketplace {
		help = "tab lens · ↑↓ · enter · ^g install · ^d uninstall · ^a add mkt · ^r refresh · esc"
	}
	if s.busy != "" {
		help = s.busy + " · " + help
	}
	lines = append(lines, themePickerHelpStyle.Render(clipText(help, w)))
	return lines
}

func skillsKindGlyph(kind string) string {
	switch kind {
	case "agent":
		return "agent"
	case "workflow":
		return "wf"
	}
	return "skill"
}

func renderSkillsBrowserRow(r skillsRow, width int, selected bool) string {
	switch r.kind {
	case skillsRowBlank:
		return ""
	case skillsRowHeader:
		line := configKeyDimStyle.Bold(true).Render(clipText(r.title, width))
		if r.note != "" {
			line += "  " + dimStyle.Render(clipText("("+r.note+")", width-lipgloss.Width(r.title)-2))
		}
		return line
	case skillsRowNote:
		return dimStyle.Render(clipText("  "+r.title, width))
	}
	var plain, tag string
	switch r.kind {
	case skillsRowItem:
		plain = r.item.name
		tag = skillsKindGlyph(r.item.kind)
		if len(r.item.warnings) > 0 {
			tag += " ⚠"
		}
		if pt := r.item.publishTag(); pt != "" {
			tag += " " + pt
		}
	case skillsRowPlugin:
		plain = r.plugin.entry.Name
		switch {
		case r.plugin.yours:
			tag = "✓ yours"
		case r.plugin.installed && r.plugin.missing:
			tag = "✓ not fetched"
		case r.plugin.installed:
			tag = "✓"
		}
	case skillsRowAction:
		plain = r.title
	case skillsRowMCP:
		plain = r.mcp.name
		state := "on"
		if r.mcp.disabled {
			state = "off"
		}
		tag = r.mcp.sourceLabel() + " · " + r.mcp.transport + " · " + state
		if r.mcp.oauth && mcpServerAuthorized(r.mcp.url) {
			tag += " · ✓ auth"
		}
	}
	if selected {
		line := "▸ " + plain
		if tag != "" {
			line += "  " + tag
		}
		line = clipText(line, width)
		if pad := width - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		return themePickerRowStyle.Render(line)
	}
	if r.kind == skillsRowAction {
		return dimStyle.Render(clipText("  "+plain, width))
	}
	line := "  " + plain
	if tag != "" {
		style := dimStyle
		if strings.HasPrefix(tag, "✓") {
			style = promptStyle
		}
		if strings.Contains(tag, "⚠") {
			style = modelPickerStatusStyle
		}
		avail := width - lipgloss.Width(line) - 2
		if avail > 0 {
			line += "  " + style.Render(clipText(tag, avail))
		}
	}
	return clipText(line, width)
}

func (m model) renderSkillsBrowserDetail(s *skillsBrowserState, w, h int) []string {
	var body []string
	hint := "pgup/pgdn scroll"
	switch {
	case s.editor != nil:
		body = skillsEditorLines(s.editor, w)
		hint = "enter confirm · esc back"
	default:
		row, ok := s.current()
		if ok {
			switch row.kind {
			case skillsRowItem:
				body = s.itemDetailLines(*row.item, w)
				hint = skillsItemHint(s, *row.item)
			case skillsRowPlugin:
				body = s.pluginDetailLines(*row.plugin, w)
				hint = "enter contents · ^g install · ^d uninstall · ^x remove marketplace"
			case skillsRowAction:
				body = skillsActionLines(row, w)
				hint = "enter"
			case skillsRowMCP:
				body = s.mcpDetailLines(*row.mcp, w)
				hint = mcpDetailHint(*row.mcp)
			}
		}
		if body == nil {
			body = skillsLensOverview(s, w)
			hint = "^a add marketplace · ^n new skill · type to search"
		}
	}

	avail := h - 2
	if avail < 1 {
		avail = 1
	}
	maxScroll := len(body) - avail
	if maxScroll < 0 {
		maxScroll = 0
	}
	off := s.detailScroll
	if off > maxScroll {
		off = maxScroll
	}
	end := off + avail
	if end > len(body) {
		end = len(body)
	}
	lines := append([]string(nil), body[off:end]...)
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	if end < len(body) {
		hint = "↓ more · " + hint
	}
	lines = append(lines, themePickerHelpStyle.Render(clipText(hint, w)))
	return lines
}

func skillsItemHint(s *skillsBrowserState, it skillsItem) string {
	if s.lens == skillsLensMarketplace {
		return "enter/^g install · ^d uninstall · esc back"
	}
	switch {
	case it.kind == "workflow" && it.pub != nil:
		return "^p publish update · ^u pull · ctrl+w builder"
	case it.kind == "workflow":
		return "^p publish · ctrl+w builder"
	case it.origin.Scope == engine.OriginPlugin:
		return "enter use · ^d uninstall plugin"
	case it.pub != nil:
		return "enter use · ^e edit · ^d delete · ^p publish update · ^u pull"
	}
	return "enter use · ^e edit · ^d delete · ^p publish · ^n new"
}

func skillsLensOverview(s *skillsBrowserState, w int) []string {
	if s.lens == skillsLensMarketplace {
		lines := []string{themePickerTitleStyle.Render("Marketplace"), ""}
		for _, l := range wordWrap("Plugin marketplaces in the Claude Code format: each plugin bundles skills, agents, and ask workflows. Install one for this user or for the whole project; Enter on a plugin shows what it contains.", w) {
			lines = append(lines, configHelpStyle.Render(l))
		}
		if len(s.marketplaces) == 0 {
			lines = append(lines, "", dimStyle.Render(clipText("No marketplaces yet. Try: anthropics/skills", w)))
		}
		return lines
	}
	lines := []string{themePickerTitleStyle.Render("Installed"), ""}
	for _, l := range wordWrap("Skills are /slash commands and model triggers; agents run through the task tool; workflows run on issues (f) and chats (Ctrl+F). Project items live under .ask/, user items under ~/.config/ask/, plugin items come from the marketplace.", w) {
		lines = append(lines, configHelpStyle.Render(l))
	}
	return lines
}

func skillsActionLines(r skillsRow, w int) []string {
	lines := []string{themePickerTitleStyle.Render(clipText(r.title, w)), ""}
	var text string
	switch r.action {
	case skillsActionAddMarketplace:
		text = "Register a marketplace from a GitHub owner/repo (for example anthropics/skills or anthropics/claude-plugins-official), a git URL, a local directory, or a direct marketplace.json URL."
	case skillsActionImportClaude:
		text = "Copy the marketplaces Claude Code knows and the plugins it has enabled into ask's own store. Nothing under ~/.claude is changed."
	case skillsActionNewMarketplace:
		text = "Create an empty marketplace directory (git-initialised) you can publish skills, agents, and workflows into, then push wherever you like."
	}
	for _, l := range wordWrap(text, w) {
		lines = append(lines, configHelpStyle.Render(l))
	}
	return lines
}

func skillsEditorLines(ed *skillsEditor, w int) []string {
	lines := []string{themePickerTitleStyle.Render(clipText(ed.title, w)), ""}
	for _, l := range wordWrap(ed.help, w) {
		lines = append(lines, configHelpStyle.Render(l))
	}
	lines = append(lines, "")
	switch ed.kind {
	case skillsEditorAddMarketplace, skillsEditorNewMarketplace:
		lines = append(lines, configPromptStyle.Render("> ")+ed.draft+configCaretStyle.Render("▏"))
	case skillsEditorScope, skillsEditorConfirm, skillsEditorPublish:
		var parts []string
		for i, opt := range ed.options {
			if i == ed.cursor {
				parts = append(parts, themePickerRowStyle.Render(" "+opt+" "))
			} else {
				parts = append(parts, dimStyle.Render(" "+opt+" "))
			}
		}
		lines = append(lines, clipText(strings.Join(parts, " "), w))
	}
	return lines
}

func factLine(label, value string, w int) string {
	return configKeyDimStyle.Render(padRight(label, modelPickerFactLabelW)) + clipText(value, w-modelPickerFactLabelW)
}

func (s *skillsBrowserState) itemDetailLines(it skillsItem, w int) []string {
	lines := []string{
		themePickerTitleStyle.Render(clipText(it.name, w)),
		configKeyDimStyle.Render(clipText(skillsKindGlyph(it.kind)+" · "+it.origin.String(), w)),
		"",
	}
	if it.desc != "" {
		lines = append(lines, wordWrap(it.desc, w)...)
		lines = append(lines, "")
	}
	if it.pub != nil {
		v := it.pub.Version
		if v != "" {
			v = " v" + v
		}
		lines = append(lines, factLine("Published", it.pub.Ref().String()+v+" · "+it.sync.String(), w))
	}
	switch it.kind {
	case "skill":
		if it.skill != nil {
			slash := "—"
			if it.skill.UserInvocable {
				slash = "/" + it.skill.Name
			}
			lines = append(lines, factLine("Slash", slash, w))
			model := "yes"
			if it.skill.DisableModelInvocation {
				model = "no (disable-model-invocation)"
			}
			lines = append(lines, factLine("Model-invoked", model, w))
			if it.skill.Command {
				lines = append(lines, factLine("Shape", "single-file command", w))
			} else if files := skillSupportFiles(it.skill.Dir); files != "" {
				lines = append(lines, factLine("Files", files, w))
			}
		}
		lines = append(lines, factLine("Path", shortPath(it.path), w))
		if body := skillsBody(it); body != "" {
			lines = append(lines, "")
			lines = append(lines, s.renderMarkdown(it.path, body, w)...)
		}
	case "agent":
		if it.agent != nil {
			lines = append(lines, factLine("Invoke", "task(agent: \""+it.agent.Name+"\")", w))
			if it.agent.Provider != "" || it.agent.Model != "" {
				lines = append(lines, factLine("Model", providerMeta(it.agent.Provider, it.agent.Model), w))
			}
			tools := "default (read-only)"
			if len(it.agent.Tools) > 0 {
				tools = strings.Join(it.agent.Tools, ", ")
			}
			lines = append(lines, factLine("Tools", tools, w))
		}
		lines = append(lines, factLine("Path", shortPath(it.path), w))
		if body := skillsBody(it); body != "" {
			lines = append(lines, "")
			lines = append(lines, s.renderMarkdown(it.path, body, w)...)
		}
	case "workflow":
		if it.workflow != nil {
			lines = append(lines, factLine("Scope", workflowScopeTag(it.workflow.Scope), w))
			for i, st := range it.workflow.Steps {
				label := fmt.Sprintf("Step %d", i+1)
				if st.Kind == workflowStepKindLoop {
					lines = append(lines, factLine(label, fmt.Sprintf("%s (loop, %d steps)", st.Name, len(st.Steps)), w))
					continue
				}
				lines = append(lines, factLine(label, st.Name+" · "+providerMeta(st.Provider, st.Model), w))
			}
		} else if it.path != "" {
			lines = append(lines, factLine("Path", shortPath(it.path), w))
		}
		for _, warn := range it.warnings {
			lines = append(lines, "")
			for _, l := range wordWrap("⚠ "+warn, w) {
				lines = append(lines, modelPickerStatusStyle.Render(l))
			}
		}
	}
	return lines
}

func (s *skillsBrowserState) mcpDetailLines(r mcpBrowserRow, w int) []string {
	lines := []string{
		themePickerTitleStyle.Render(clipText(r.name, w)),
		configKeyDimStyle.Render(clipText("mcp · "+r.sourceLabel(), w)),
		"",
	}
	state := "enabled"
	if r.disabled {
		state = "disabled"
	}
	lines = append(lines, factLine("State", state, w))
	lines = append(lines, factLine("Transport", r.transport, w))
	lines = append(lines, factLine("Target", r.target, w))
	if r.oauth {
		auth := "not authorized"
		if mcpServerAuthorized(r.url) {
			auth = "authorized (token stored)"
		}
		if st, ok := s.mcpStatus[r.name]; ok {
			switch st.Kind {
			case mcpStatusConnected:
				auth = "connected"
			case mcpStatusNeedsAuth:
				auth = "needs authorization — press ^o"
			case mcpStatusError:
				auth = "error: " + st.Detail
			}
		}
		lines = append(lines, factLine("Auth", auth, w))
	} else if st, ok := s.mcpStatus[r.name]; ok {
		k := "connected"
		if st.Kind == mcpStatusError {
			k = "error: " + st.Detail
		}
		lines = append(lines, factLine("Status", k, w))
	}
	return lines
}

func mcpDetailHint(r mcpBrowserRow) string {
	h := "space toggle on/off"
	if r.oauth {
		h += " · ^o authorize · ^d sign out"
	}
	return h
}

func skillsBody(it skillsItem) string {
	switch {
	case it.skill != nil:
		return strings.TrimSpace(engine.SkillBody(*it.skill))
	case it.agent != nil:
		return strings.TrimSpace(it.agent.Prompt)
	case it.path != "":
		_, body, ok := engine.ParseMarkdownFrontmatter(it.path)
		if ok {
			return strings.TrimSpace(body)
		}
	}
	return ""
}

func skillSupportFiles(dir string) string {
	if dir == "" {
		return ""
	}
	var parts []string
	for _, sub := range []string{"scripts", "references", "assets"} {
		if info, err := os.Stat(filepath.Join(dir, sub)); err == nil && info.IsDir() {
			parts = append(parts, sub+"/")
		}
	}
	return strings.Join(parts, " ")
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func (s *skillsBrowserState) renderMarkdown(key, md string, w int) []string {
	if s.descCache == nil {
		s.descCache = map[string]string{}
	}
	k := key + "\x00" + strconv.Itoa(w) + "\x00" + strconv.Itoa(len(md))
	out, ok := s.descCache[k]
	if !ok {
		out = renderModelDescription(md, w)
		s.descCache[k] = out
	}
	return strings.Split(out, "\n")
}

func (s *skillsBrowserState) pluginDetailLines(p skillsPluginRow, w int) []string {
	e := p.entry
	title := e.Name
	if e.Version != "" {
		title += " " + e.Version
	}
	lines := []string{
		themePickerTitleStyle.Render(clipText(title, w)),
		configKeyDimStyle.Render(clipText(p.ref, w)),
		"",
	}
	if e.Description != "" {
		lines = append(lines, wordWrap(e.Description, w)...)
		lines = append(lines, "")
	}
	status := "not installed"
	switch {
	case p.yours:
		status = "yours — published from your local copy (edit it under Installed, ctrl+p to update)"
	case p.installed:
		status = "installed · " + strings.Join(p.scopes, ", ")
		if p.missing {
			status += " · not fetched on this machine (ctrl+r)"
		}
	}
	lines = append(lines, factLine("Status", status, w))
	if e.Category != "" {
		lines = append(lines, factLine("Category", e.Category, w))
	}
	if e.Author != nil && e.Author.Name != "" {
		lines = append(lines, factLine("Author", e.Author.Name, w))
	}
	lines = append(lines, factLine("Source", e.Source.String(), w))
	if e.Homepage != "" {
		lines = append(lines, factLine("Homepage", e.Homepage, w))
	}
	if len(e.Skills) > 0 {
		lines = append(lines, factLine("Skills", strings.Join(e.Skills, ", "), w))
	}
	if p.dir != "" {
		lines = append(lines, factLine("Contents", "enter to browse", w))
	} else {
		lines = append(lines, factLine("Contents", "fetched on install", w))
	}
	return lines
}
