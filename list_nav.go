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
