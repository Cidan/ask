package main

import (
	tea "charm.land/bubbletea/v2"
)

// listNavPrev reports whether msg is the "previous item" key for any
// vertical popup/picker list: ArrowUp or Ctrl+P. Both bindings are
// hardcoded (not routed through the customizable [[KeyMap]]) so a user
// who rebinds away every modifier key still has at least one path to
// move a cursor up — losing list navigation locks them out of /config
// entirely.
func listNavPrev(msg tea.KeyPressMsg) bool {
	if msg.Mod == 0 && msg.Code == tea.KeyUp {
		return true
	}
	return msg.Mod == tea.ModCtrl && msg.Code == 'p'
}

// listNavNext is the down-direction peer of listNavPrev: ArrowDown or
// Ctrl+N. Same hardcoded-on-purpose rationale applies.
func listNavNext(msg tea.KeyPressMsg) bool {
	if msg.Mod == 0 && msg.Code == tea.KeyDown {
		return true
	}
	return msg.Mod == tea.ModCtrl && msg.Code == 'n'
}

// isCtrlListNav reports whether msg is specifically the Ctrl+P / Ctrl+N
// variant of list navigation. The screen-switch dispatcher in
// model.Update uses this to know when to defer to a popover — without
// it, the default Ctrl+P binding (ActionScreenPRs) would yank the user
// to the PR screen mid-pick whenever they tried to nav a popup with
// emacs keys.
func isCtrlListNav(msg tea.KeyPressMsg) bool {
	if msg.Mod != tea.ModCtrl {
		return false
	}
	return msg.Code == 'p' || msg.Code == 'n'
}
