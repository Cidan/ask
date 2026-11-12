package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/providers"
)

// configRowKind is the control a /config row is edited with.
type configRowKind int

const (
	// configToggle is on/off; Enter and ←/→ flip it.
	configToggle configRowKind = iota
	// configChoice is a few named values shown side by side; ←/→ step
	// through them, Enter advances.
	configChoice
	// configSelect is a longer list; ←/→ step, Enter opens the list.
	configSelect
	// configText is a free-text value edited inline.
	configText
	// configAction opens another screen or picker.
	configAction
	// configKey is a keybinding; Enter captures the next key pressed.
	configKey
	// configList is a list of values; Enter opens it under the row to add,
	// edit, and remove items.
	configList
)

// configRow is one setting. Rows are rebuilt from the saved config and the
// tab's state on every frame, so a row never holds a stale value.
type configRow struct {
	id    string
	kind  configRowKind
	group string
	label string
	// value is the right-hand text of text, action, and key rows.
	value string
	desc  string
	// options and current are the values of toggle, choice, and select
	// rows; values, when set, are what each option stores.
	options []string
	values  []string
	current int

	// Text rows: the field it edits, what is stored now, and how a new
	// value is written (under the config lock) and applied.
	field   providers.SettingField
	stored  string
	save    func(*askConfig, string)
	onSaved func(model) model
	tag     string

	// List rows: the items, the add row's label, how a new list is
	// written, what an entered item is stored as (or why it is refused),
	// and the completions for an item being typed. onSaved and tag apply
	// as for text rows.
	items    []string
	addLabel string
	saveList func(*askConfig, []string)
	accept   func(string) (string, error)
	complete func(string) []pathMatch

	action Action
}

// configSection is one page of the /config screen.
type configSection struct {
	id    string
	title string
	blurb string
	rows  []configRow
	// warn marks the section in the navigation (⚠): something in it
	// needs the user's attention.
	warn bool
}

func toggleRow(id, label string, on bool, desc string) configRow {
	cur := 1
	if on {
		cur = 0
	}
	return configRow{id: id, kind: configToggle, label: label, options: []string{"on", "off"}, current: cur, desc: desc}
}

func choiceRow(id, label string, options, values []string, current string, desc string) configRow {
	cur := 0
	for i, v := range values {
		if v == current {
			cur = i
			break
		}
	}
	return configRow{id: id, kind: configChoice, label: label, options: options, values: values, current: cur, desc: desc}
}

// configSections builds every page of the screen from the saved config.
func (m model) configSections() []configSection {
	cfg, _ := loadConfig()
	return []configSection{
		m.configAppearanceSection(),
		m.configSessionSection(cfg),
		configProvidersSection(cfg),
		configDecisionsSection(cfg),
		configMemorySection(cfg),
		configWebSearchSection(cfg),
		m.configProjectSection(cfg),
		configKeybindingsSection(),
	}
}

func (m model) configAppearanceSection() configSection {
	themes := make([]string, len(themeRegistry))
	for i, t := range themeRegistry {
		themes[i] = t.name
	}
	theme := configRow{id: "theme", kind: configSelect, label: "Theme", options: themes, values: themes, current: themeIndexByName(m.themeName),
		desc: "The color theme of every screen. ←/→ steps through themes and applies each at once; Enter opens the list to preview them."}
	return configSection{id: "appearance", title: "Appearance", blurb: "How the chat looks", rows: []configRow{
		theme,
		toggleRow("quiet", "Quiet mode", m.quietMode,
			"Hides tool calls and diffs from the chat and shows the assistant's text as it streams. Toggling re-renders the whole history."),
		choiceRow("toolOutput", "Tool output", []string{"full", "short", "off"},
			[]string{string(toolOutputFull), string(toolOutputShort), string(toolOutputOff)}, string(m.toolOutputMode),
			"How much of each tool call the chat shows: full adds every input and the whole result, short keeps one header line, off hides tool calls entirely."),
		toggleRow("renderDiffs", "Render diffs", m.renderDiffs,
			"Shows the red/green diff of every file edit and write inline in the chat."),
		toggleRow("cursorBlink", "Cursor blink", m.cursorBlink,
			"Blinks the cursor in the prompt input."),
	}}
}

func (m model) configSessionSection(cfg askConfig) configSection {
	names := make([]string, len(providerRegistry))
	ids := make([]string, len(providerRegistry))
	for i, p := range providerRegistry {
		names[i], ids[i] = p.DisplayName(), p.ID()
	}
	def := cfg.Provider
	if p := providerByID(def); p != nil {
		def = p.ID()
	}
	worktreeDesc := "Runs every new tab's session in its own git worktree under .claude/worktrees/, so parallel agents never edit the same checkout."
	if pv := loadProjectConfig(cfg, m.cwd).Worktree; pv != nil {
		worktreeDesc += " This project overrides it (" + onOff(*pv) + ") under Project."
	}
	return configSection{id: "session", title: "Session", blurb: "How the agent runs", rows: []configRow{
		choiceRow("provider", "Default provider", names, ids, def,
			"The provider new tabs start on. Ctrl+M switches the current tab; this only changes what the next tab (Ctrl+T) gets."),
		toggleRow("skipAllPermissions", "Skip all permissions", m.skipAllPermissions,
			"Runs every tool without asking first — bash, edits, writes, fetches. Changing it restarts this tab's session."),
		toggleRow("worktree", "Worktree", cfg.UI.Worktree != nil && *cfg.UI.Worktree, worktreeDesc),
		toggleRow("autoCompact", "Auto-compact", cfg.AutoCompact == nil || *cfg.AutoCompact,
			"Once the conversation reaches 90% of the model's context window, the oldest turns are dropped from what is sent so the session keeps going. The transcript keeps every turn."),
		toggleRow("deslop", "Deslop output", cfg.Deslop.Enabled != nil && *cfg.Deslop.Enabled,
			"Rewrites the assistant's replies for display with a second model, stripping filler and grandiose phrasing. The transcript keeps the original."),
		{id: "deslopModel", kind: configAction, label: "Deslop model", value: deslopConfigSummary(cfg),
			desc: "The model that rewrites replies when Deslop output is on. Enter opens the model picker."},
	}}
}

func configProvidersSection(cfg askConfig) configSection {
	sec := configSection{id: "providers", title: "Providers", blurb: "Credentials and endpoints for each model provider"}
	for _, p := range providers.All() {
		pc := cfg.ProviderConfig(p.ID())
		group := p.DisplayName() + providerStatusSuffix(p.Configured(pc))
		if !p.Configured(pc) && p.ID() == cfg.Provider {
			sec.warn = true
		}
		id := p.ID()
		for _, f := range p.Settings() {
			sec.rows = append(sec.rows, settingRow("provider:"+id+":"+f.Key, group, f, pc.Field(f.Key), id,
				func(c *askConfig, v string) { c.SetProviderConfig(id, c.ProviderConfig(id).WithField(f.Key, v)) },
				func(m model) model {
					if m.provider != nil && m.provider.ID() == id {
						m.killProc()
					}
					return m
				}))
		}
	}
	return sec
}

func providerStatusSuffix(configured bool) string {
	if configured {
		return " · ✓ ready"
	}
	return " · ⚠ not configured"
}

// settingRow is a text row over a declared setting field.
func settingRow(id, group string, f providers.SettingField, stored, tag string, save func(*askConfig, string), onSaved func(model) model) configRow {
	desc := strings.TrimSuffix(strings.TrimSpace(f.Hint), "; enter to save")
	if f.EnvKey != "" {
		desc += ". Falls back to $" + f.EnvKey + " when unset"
	}
	if f.Default != "" {
		desc += "; defaults to " + f.Default
	}
	desc += "."
	return configRow{id: id, kind: configText, group: group, label: f.Title, value: fieldSummary(f, stored), desc: desc,
		field: f, stored: stored, save: save, onSaved: onSaved, tag: tag}
}

func configDecisionsSection(cfg askConfig) configSection {
	sec := configSection{id: "decisions", title: "Decisions", blurb: "Decision models: fast, calibrated yes/no, choice, and score answers"}
	labels := []string{"Automatic"}
	values := []string{""}
	for _, p := range providers.DecisionProviders() {
		configured := p.Configured(cfg.ProviderConfig(p.ID()))
		for _, dm := range p.DecisionModels() {
			label := p.DisplayName() + " · " + dm.DisplayName
			if !configured {
				label += " (no key)"
			}
			labels = append(labels, label)
			values = append(values, p.ID()+"/"+dm.ID)
		}
	}
	current := ""
	if cfg.Decisions.Provider != "" {
		current = cfg.Decisions.Provider + "/" + cfg.Decisions.Model
		if cfg.Decisions.Model == "" {
			if p, ok := providers.GetDecisionProvider(cfg.Decisions.Provider); ok {
				current = cfg.Decisions.Provider + "/" + providers.DecisionModelFor(p, "").ID
			}
		}
	}
	model := configRow{id: "decisionModel", kind: configSelect, label: "Decision model", options: labels, values: values}
	for i, v := range values {
		if v == current {
			model.current = i
		}
	}
	target, ok := providers.ResolveDecisionTarget(cfg)
	if ok {
		model.desc = "Answers the decide tool, the memory extraction gate, and the recall filter. Now: " + target.Label() + "."
	} else {
		model.desc = "Answers the decide tool, the memory extraction gate, and the recall filter. No decision model can run yet — add an API key below."
	}
	model.desc += " Automatic picks the first provider with a key."
	sec.rows = append(sec.rows, model)
	for _, p := range providers.DecisionProviders() {
		if _, isLLM := providers.Get(p.ID()); isLLM {
			continue
		}
		id := p.ID()
		pc := cfg.ProviderConfig(id)
		group := p.DisplayName() + providerStatusSuffix(p.Configured(pc))
		for _, f := range p.Settings() {
			sec.rows = append(sec.rows, settingRow("decision:"+id+":"+f.Key, group, f, pc.Field(f.Key), id,
				func(c *askConfig, v string) { c.SetProviderConfig(id, c.ProviderConfig(id).WithField(f.Key, v)) }, nil))
		}
	}
	return sec
}

func configMemorySection(cfg askConfig) configSection {
	return configSection{id: "memory", title: "Memory", blurb: "Long-term memory across sessions", rows: []configRow{
		{id: "memoryModel", kind: configAction, label: "Extraction model", value: memoryConfigSummary(cfg),
			desc: "The model that reads each finished turn and files what is worth remembering. Enter opens the model picker; Automatic uses the session's provider and its cheapest model."},
		toggleRow("memoryGate", "Skip empty turns", config.MemoryExtractGateEnabled(cfg),
			"Before extraction, the decision model judges whether the turn holds anything durable; turns with nothing to remember skip the extraction call. Needs a decision model."),
		toggleRow("memoryFilter", "Filter recalled memory", config.MemoryRecallFilterEnabled(cfg),
			"Each turn, the decision model reads the memories similarity recall found and drops the ones that do not bear on the prompt, so only relevant memory reaches the model. Needs a decision model."),
		{id: "memoryBrowse", kind: configAction, label: "Browse memories", value: "/memory",
			desc: "Opens the memory browser: every concept this project and the global scope hold, with reinforce, demote, and forget."},
	}}
}

func configWebSearchSection(cfg askConfig) configSection {
	f := webSearchFields[0]
	return configSection{id: "websearch", title: "Web search", blurb: "The web_search tool", rows: []configRow{
		settingRow("braveApiKey", "", f, cfg.WebSearch.BraveAPIKey, "web search",
			func(c *askConfig, v string) { c.WebSearch.BraveAPIKey = v }, nil),
	}}
}

func (m model) configProjectSection(cfg askConfig) configSection {
	pc := loadProjectConfig(cfg, m.cwd)
	sec := configSection{id: "project", title: "Project · " + shortCwdOf(projectRoot(m.cwd)), blurb: "Settings for this project only"}
	auth := choiceRow("githubAuth", "GitHub auth", []string{"personal access token", "gh CLI"}, []string{"", githubAuthGH}, pc.MCP.GitHub.Auth,
		"Where GitHub issues, PRs, and the GitHub MCP server get their credential: the PAT below, or the gh CLI's own login. Changing it restarts this tab's session.")
	auth.group = "GitHub"
	sec.rows = append(sec.rows, auth)
	for _, key := range []string{"githubMCPEndpoint", "githubMCPToken", "linearGraphQLEndpoint", "linearAPIKey", "linearTeamKey"} {
		spec := projectFieldSpecs[key]
		group := "GitHub"
		if strings.HasPrefix(key, "linear") {
			group = "Linear"
		}
		f := providers.SettingField{Key: key, Title: spec.title, Hint: spec.helpHint, Secret: spec.masked}
		row := settingRow("project:"+key, group, f, spec.load(cfg, m.cwd), "project",
			func(c *askConfig, v string) {
				p := loadProjectConfig(*c, m.cwd)
				spec.save(&p, v)
				*c = upsertProjectConfig(*c, m.cwd, p)
			},
			func(m model) model {
				m.killProc()
				return m
			})
		if row.stored == "" && !f.Secret {
			row.value = "(default)"
			if key == "linearTeamKey" {
				row.value = "(not set)"
			}
		}
		sec.rows = append(sec.rows, row)
	}

	names := make([]string, len(issueProviderRegistry))
	ids := make([]string, len(issueProviderRegistry))
	for i, p := range issueProviderRegistry {
		names[i], ids[i] = p.DisplayName(), p.ID()
	}
	issues := choiceRow("issueProvider", "Issue provider", names, ids, pc.Issues.Provider,
		"Where the Issues screen (Ctrl+I) reads from. Providers without credentials above are skipped.")
	issues.group = "Work"
	worktreeOpts := []string{"inherit (" + onOff(cfg.UI.Worktree != nil && *cfg.UI.Worktree) + ")", "on", "off"}
	wtCur := "inherit"
	if pc.Worktree != nil {
		wtCur = onOff(*pc.Worktree)
	}
	worktree := choiceRow("projectWorktree", "Worktree", worktreeOpts, []string{"inherit", "on", "off"}, wtCur,
		"Overrides the global Worktree setting for this project: inherit follows Session → Worktree.")
	worktree.group = "Work"
	root := m.cwd
	links := configRow{id: "projectLinks", kind: configList, group: "Work", label: "Link targets", tag: "project",
		value: linkTargetsSummary(pc.Links), items: pc.Links, addLabel: "+ Add target",
		desc: "Symlinks that " + hintOr(keyHintFor(ActionWorktreeLink), "the link key") +
			" points at this tab's worktree, and back at the project root on the next press or once cleanup removes that worktree. Enter opens the list; paths complete from the filesystem as you type.",
		saveList: func(c *askConfig, items []string) {
			p := loadProjectConfig(*c, root)
			p.Links = items
			*c = upsertProjectConfig(*c, root, p)
		},
		accept:   func(v string) (string, error) { return acceptLinkTarget(v, root) },
		complete: func(q string) []pathMatch { return linkTargetMatches(q, root) },
		onSaved: func(m model) model {
			m.refreshLinked()
			return m
		}}
	wfDesc := "(none)"
	switch n := len(pc.Workflows.Items); {
	case n == 1:
		wfDesc = "1 workflow"
	case n > 1:
		wfDesc = fmt.Sprintf("%d workflows", n)
	}
	sec.rows = append(sec.rows, issues, worktree, links, configRow{id: "workflows", kind: configAction, group: "Work", label: "Workflows", value: wfDesc,
		desc: "Opens the workflows builder (Ctrl+W) for this project's pipelines."})
	return sec
}

func configKeybindingsSection() configSection {
	km := currentKeyMap()
	sec := configSection{id: "keybindings", title: "Keybindings", blurb: "Enter rebinds · Ctrl+R restores the default · Delete unbinds"}
	for _, g := range actionGroups {
		for _, item := range g.Items {
			def := displayBinding(defaultKeyBindings[item.Action])
			sec.rows = append(sec.rows, configRow{id: "key:" + string(item.Action), kind: configKey, group: g.Heading, label: item.Label,
				value: displayBinding(km.Binding(item.Action)), action: item.Action,
				desc: item.Label + " — default " + def + "."})
		}
	}
	return sec
}

// fieldSummary is a text row's value: the stored value (masked for
// secrets), else where the value would come from — env or default.
func fieldSummary(f providers.SettingField, stored string) string {
	switch {
	case stored != "" && f.Secret:
		return "configured"
	case stored != "":
		return stored
	case f.EnvKey != "" && os.Getenv(f.EnvKey) != "":
		return "from $" + f.EnvKey
	case f.Default != "":
		return f.Default + " (default)"
	}
	return "(not set)"
}
