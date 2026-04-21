package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)


const (
	approvalBoxWidth      = 90
	approvalChoiceDeny    = 0
	approvalChoiceAllow   = 1
	approvalChoiceAlways  = 2
	approvalChoiceCount   = 3
	approvalSummaryMaxLns = 2
)

func (m model) startApproval(msg approvalRequestMsg) model {
	m.mode = modeApproval
	m.approvalTool = msg.toolName
	m.approvalInput = msg.input
	m.approvalReply = msg.reply
	m.approvalChoice = 0
	return m
}

func (m model) clearApproval() model {
	m.mode = modeInput
	m.approvalTool = ""
	m.approvalInput = nil
	m.approvalReply = nil
	m.approvalChoice = 0
	return m
}

func (m model) sendApproval(choice int) model {
	if m.approvalReply != nil {
		reply := approvalReply{allow: choice != approvalChoiceDeny}
		if choice == approvalChoiceAlways {
			rule := permissionRuleFor(m.approvalTool, m.approvalInput)
			reply.remember = &rule
		}
		m.approvalReply <- reply
	}
	return m.clearApproval()
}

func (m model) updateApproval(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, tea.Quit
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c':
		m = m.sendApproval(approvalChoiceDeny)
		m.killProc()
		m.appendHistory(outputStyle.Render(dimStyle.Render("✗ cancelled")))
		return m, nil
	case msg.Code == tea.KeyEsc, msg.Code == 'n' && msg.Mod == 0:
		return m.sendApproval(approvalChoiceDeny), nil
	case msg.Code == 'y' && msg.Mod == 0:
		return m.sendApproval(approvalChoiceAllow), nil
	case msg.Code == 'a' && msg.Mod == 0:
		return m.sendApproval(approvalChoiceAlways), nil
	case msg.Code == tea.KeyLeft, msg.Code == 'h' && msg.Mod == 0:
		if m.approvalChoice > 0 {
			m.approvalChoice--
		}
		return m, nil
	case msg.Code == tea.KeyRight, msg.Code == 'l' && msg.Mod == 0:
		if m.approvalChoice < approvalChoiceCount-1 {
			m.approvalChoice++
		}
		return m, nil
	case msg.Code == tea.KeyTab:
		m.approvalChoice = (m.approvalChoice + 1) % approvalChoiceCount
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.sendApproval(m.approvalChoice), nil
	}
	return m, nil
}

func (m model) viewApproval() string {
	innerW := approvalBoxWidth - 6
	if innerW > m.width-6 {
		innerW = m.width - 6
	}
	if innerW < 24 {
		innerW = 24
	}

	title := approvalTitleStyle.Render("Approval required")
	headline := approvalHeadline(m.approvalTool)
	summary := approvalSummary(m.approvalTool, m.approvalInput, innerW)

	deny := approvalBtnStyle.Render("Deny")
	allow := approvalBtnStyle.Render("Allow")
	always := approvalBtnStyle.Render("Always allow")
	switch m.approvalChoice {
	case approvalChoiceDeny:
		deny = approvalDenyActiveStyle.Render("Deny")
	case approvalChoiceAllow:
		allow = approvalAllowActiveStyle.Render("Allow")
	case approvalChoiceAlways:
		always = approvalAlwaysActiveStyle.Render("Always allow")
	}
	buttons := lipgloss.JoinHorizontal(lipgloss.Top, deny, "  ", allow, "  ", always)

	help := askHelpStyle.Render("←→ switch · enter confirm · y allow · a always · n/esc deny · ctrl+c cancel turn")

	lines := []string{title, "", headline}
	if summary != "" {
		lines = append(lines, summary)
	}
	lines = append(lines, "", buttons, "", help)
	content := strings.Join(lines, "\n")
	return approvalBoxStyle.Width(innerW).Render(content)
}

// approvalHeadline is the single short line telling the user what Claude is
// trying to do, e.g. "Claude wants to use Edit". The rest of the UI never
// dumps tool arguments — just this headline plus a one-line summary.
func approvalHeadline(tool string) string {
	if tool == "" {
		return "Claude wants to use a tool"
	}
	return "Claude wants to use " + approvalToolStyle.Render(tool)
}

// approvalSummary renders the narrowest useful one-liner identifying the
// target of the tool call. For file tools this is the absolute file path; for
// Bash it's the command (wrapped to at most two lines); other tools get a
// tool-specific hint or an empty string when there is nothing worth showing.
func approvalSummary(tool string, input map[string]any, width int) string {
	if width < 10 {
		width = 10
	}
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "Read":
		if p, _ := input["file_path"].(string); p != "" {
			return approvalSummaryStyle.Render(truncateFromLeft(p, width))
		}
	case "Bash":
		if c, _ := input["command"].(string); c != "" {
			return approvalSummaryStyle.Render(firstLinesClamped(c, width, approvalSummaryMaxLns))
		}
	case "Glob", "Grep":
		if p, _ := input["pattern"].(string); p != "" {
			return approvalSummaryStyle.Render(truncate(p, width))
		}
	case "WebFetch":
		if u, _ := input["url"].(string); u != "" {
			return approvalSummaryStyle.Render(truncate(u, width))
		}
	case "WebSearch":
		if q, _ := input["query"].(string); q != "" {
			return approvalSummaryStyle.Render(truncate(q, width))
		}
	case "Task":
		if a, _ := input["subagent_type"].(string); a != "" {
			return approvalSummaryStyle.Render(truncate("subagent: "+a, width))
		}
	}
	if len(input) == 0 {
		return dimStyle.Render("(no arguments)")
	}
	return dimStyle.Render("(arguments hidden)")
}

// truncateFromLeft keeps the tail of s (useful for paths) and prepends an
// ellipsis when the string would otherwise exceed width.
func truncateFromLeft(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	if width <= 1 {
		return string(runes[len(runes)-width:])
	}
	tail := runes[len(runes)-(width-1):]
	return "…" + string(tail)
}

// firstLinesClamped returns up to maxLines lines from s, each width-truncated,
// with a trailing ellipsis line when overflow happens.
func firstLinesClamped(s string, width, maxLines int) string {
	if maxLines < 1 {
		maxLines = 1
	}
	lines := strings.Split(s, "\n")
	overflow := len(lines) > maxLines
	if overflow {
		lines = lines[:maxLines]
	}
	for i, ln := range lines {
		if lipgloss.Width(ln) > width {
			lines[i] = truncate(ln, width)
		}
	}
	out := strings.Join(lines, "\n")
	if overflow {
		out += "\n…"
	}
	return out
}
