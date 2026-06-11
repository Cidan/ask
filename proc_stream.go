package main

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// Provider-agnostic plumbing between a session's message channel and
// the tea loop. These predate the CLI providers' removal — every
// in-process provider stream rides the same pump.

// nextStreamCmd pulls the next message off a provider stream channel.
// A closed channel yields nil, which Update treats as end-of-stream.
func nextStreamCmd(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// loadHistoryCmd wraps the provider's LoadHistory in a tea.Cmd so
// update.go can schedule the replay asynchronously. vsID tags the
// emitted historyLoadedMsg so the Update gate can match on the VS
// id (cross-provider translation paths fire with sessionID pointing
// at a non-current-provider native id, where the sessionID alone
// can't pair the reply with the tab state).
func loadHistoryCmd(tabID int, p Provider, sessionID, vsID string, opts HistoryOpts, silent bool) tea.Cmd {
	return func() tea.Msg {
		entries, err := p.LoadHistory(sessionID, opts)
		return historyLoadedMsg{
			tabID:            tabID,
			sessionID:        sessionID,
			virtualSessionID: vsID,
			entries:          entries,
			err:              err,
			silent:           silent,
		}
	}
}

// userBarText renders the user-bar line for a sent prompt, annotating
// attached images.
func userBarText(line string, n int) string {
	switch {
	case n == 0:
		return line
	case n == 1 && line == "":
		return "[image attached]"
	case n == 1:
		return line + "  [image attached]"
	case line == "":
		return fmt.Sprintf("[%d images attached]", n)
	default:
		return line + fmt.Sprintf("  [%d images attached]", n)
	}
}

// loadSessionsCmd reads ~/.config/ask/sessions.json and surfaces the
// virtual sessions scoped to workspace. Provider-native sessions
// without a VS entry are hidden — legacy pre-VS sessions simply do
// not appear in the picker. Each returned sessionEntry carries a
// virtualSessionID so the picker's Enter handler can look the VS
// back up and decide how to resume it (direct native-id resume,
// translation, fresh native session) based on the current provider.
func loadSessionsCmd(tabID int, cwd string) tea.Cmd {
	return func() tea.Msg {
		store, err := loadVirtualSessions()
		if err != nil {
			return sessionsLoadedMsg{tabID: tabID, err: err}
		}
		vss := store.listForWorkspace(cwd)
		sessions := make([]sessionEntry, 0, len(vss))
		for _, vs := range vss {
			sessions = append(sessions, sessionEntry{
				id:               vs.ID,
				virtualSessionID: vs.ID,
				cwd:              vs.Workspace,
				preview:          vs.Preview,
				modTime:          vs.UpdatedAt,
			})
		}
		return sessionsLoadedMsg{tabID: tabID, sessions: sessions}
	}
}
