package main

import (
	tea "charm.land/bubbletea/v2"
)

func listNavPrev(msg tea.KeyPressMsg) bool {
	if msg.Mod == 0 && msg.Code == tea.KeyUp {
		return true
	}
	return msg.Mod == tea.ModCtrl && msg.Code == 'p'
}

func listNavNext(msg tea.KeyPressMsg) bool {
	if msg.Mod == 0 && msg.Code == tea.KeyDown {
		return true
	}
	return msg.Mod == tea.ModCtrl && msg.Code == 'n'
}

func isCtrlListNav(msg tea.KeyPressMsg) bool {
	if msg.Mod != tea.ModCtrl {
		return false
	}
	return msg.Code == 'p' || msg.Code == 'n'
}

// listNavWrap applies a +1 / -1 cursor step with wrap-around at the
// list boundary. delta must be in {-1, +1}; count is len(items).
// count <= 0 returns 0 so callers can use the helper unconditionally
// on a possibly-empty list. Used by every popup picker so the cursor
// rolls from the last entry to the first (and vice-versa) instead of
// clamping at the edge.
func listNavWrap(cursor, delta, count int) int {
	if count <= 0 {
		return 0
	}
	return (cursor + delta + count) % count
}
