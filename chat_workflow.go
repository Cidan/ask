package main

import (
	tea "charm.land/bubbletea/v2"
)

// dispatchChatWorkflow is the chat-screen analogue of
// dispatchIssueWorkflow. Pressing Ctrl+F on a chat tab snapshots the
// current history into a workflowSource, then either toasts (no
// chat / no workflows) or pops the same picker the issues screen
// uses on `f`. Selecting a workflow spawns a workflow tab whose
// step prompt embeds the chat transcript verbatim.
//
// All gating happens here (busy/empty/already-on-workflow-tab) so
// the keypress dispatcher in update.go stays thin.
func (m model) dispatchChatWorkflow() (tea.Model, tea.Cmd) {
	if m.workflowRun != nil {
		return m, nil
	}
	if m.busy {
		return m, m.toast.show("can't run a workflow while a turn is in flight")
	}
	source := chatWorkflowSource(m.id, m.history)
	if len(source.ChatTranscript) == 0 {
		return m, m.toast.show("no chat to send")
	}
	items := projectWorkflows(m.cwd)
	if len(items) == 0 {
		return m, m.toast.show("no workflows configured · ctrl+w opens the builder")
	}
	m = m.openWorkflowPicker(items, source)
	return m, nil
}
