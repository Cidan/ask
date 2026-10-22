package main

import (
	"strconv"

	"github.com/Cidan/ask/pkg/herdr"
)

// herdrPane reports the app's status to herdr when ask runs inside a herdr
// pane; nil everywhere else (every herdr.Client method is nil-safe).
var herdrPane *herdr.Client

// herdrStatus is what herdr shows for the pane ask runs in. herdr tracks
// one agent per pane, so the tabs roll up: blocked while any tab waits on
// the user (the active tab's reason first), working while any tab runs a
// turn or workflow, idle otherwise. The active tab supplies the session
// herdr resumes after a restart and the display metadata.
func (a app) herdrStatus() herdr.Status {
	active := a.activeTab()
	st := herdr.Status{State: herdr.Idle}
	blocked := active
	reason := active.herdrBlockedReason()
	for _, t := range a.tabs {
		if reason != "" {
			break
		}
		blocked, reason = t, t.herdrBlockedReason()
	}
	switch {
	case reason != "":
		st.State = herdr.Blocked
		st.Message = reason
		if len(a.tabs) > 1 {
			st.Message = blocked.sidebarTitle() + ": " + reason
		}
	default:
		for _, t := range a.tabs {
			if t.busy() {
				st.State = herdr.Working
				break
			}
		}
	}
	if id := active.resumableSessionID(); id != "" {
		st.SessionID = id
		st.Resume = []string{"ask", "resume", id}
	}
	if active.tabTitle != "" {
		st.Title = active.sidebarTitle()
	}
	st.Tokens = map[string]string{"tabs": strconv.Itoa(len(a.tabs))}
	if meta := active.sidebarMeta(); meta != "" {
		st.Tokens["model"] = meta
	}
	return st
}

// herdrBlockedReason says what the tab is waiting on the user for, or ""
// when it is not waiting (needsUserInput).
func (m *model) herdrBlockedReason() string {
	switch m.mode {
	case modeApproval:
		return "approve " + nonEmpty(m.approvalTool, "a tool call")
	case modeSudoPassword:
		return "sudo password"
	case modeAskQuestion:
		return "answer a question"
	case modeMCPAuth:
		return "authorize " + nonEmpty(m.mcpAuthServer, "an MCP server")
	}
	if m.needsUserInput() {
		return "waiting for input"
	}
	return ""
}

// resumableSessionID is the virtual session `ask resume` reopens for this
// tab: its own, or for a workflow that supplanted the tab, the chat the
// workflow will hand back to.
func (m *model) resumableSessionID() string {
	if r := m.workflowRun; r != nil && r.supplanted != nil && r.supplanted.virtualSessionID != "" {
		return r.supplanted.virtualSessionID
	}
	return m.virtualSessionID
}
