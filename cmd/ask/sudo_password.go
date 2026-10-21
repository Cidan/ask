package main

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

const (
	sudoBoxWidth = 80
)

func (m model) startSudoPassword(msg sudoPasswordRequestedMsg) model {
	(&m).clearSelection()

	ti := textinput.New()
	ti.Prompt = "Password: "
	ti.EchoMode = textinput.EchoPassword
	ti.Focus()

	m.mode = modeSudoPassword
	m.sudoPrompt = msg.prompt
	m.sudoReply = msg.reply
	m.sudoInput = ti
	m.sudoIncorrectAttempt = msg.retry
	return m
}

func (m model) clearSudoPassword() model {
	m.mode = modeInput
	m.sudoPrompt = ""
	m.sudoReply = nil
	m.sudoInput.Reset()
	m.sudoIncorrectAttempt = false
	return m
}

// dismissSudoPassword withdraws the request answered on reply: it closes
// the prompt when that request is on screen, else drops it from the queue.
func (m model) dismissSudoPassword(reply chan sudoPasswordReply) model {
	if m.mode == modeSudoPassword && m.sudoReply == reply {
		return m.clearSudoPassword()
	}
	kept := make([]tea.Msg, 0, len(m.pendingModals))
	for _, msg := range m.pendingModals {
		if req, ok := msg.(sudoPasswordRequestedMsg); ok && req.reply == reply {
			continue
		}
		kept = append(kept, msg)
	}
	m.pendingModals = kept
	return m
}

func (m model) pasteSudoPassword(content string) (model, tea.Cmd) {
	var cmd tea.Cmd
	m.sudoInput, cmd = m.sudoInput.Update(tea.PasteMsg{Content: strings.TrimRight(content, "\r\n")})
	return m, cmd
}

func (m model) sendSudoPassword(password string, cancelled bool) model {
	if m.sudoReply != nil {
		m.sudoReply <- sudoPasswordReply{
			password:  password,
			cancelled: cancelled,
		}
	}
	return m.clearSudoPassword()
}

func (m model) updateSudoPassword(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		return m.sendSudoPassword("", true), nil
	case msg.Code == tea.KeyEnter:
		pass := m.sudoInput.Value()
		return m.sendSudoPassword(pass, false), nil
	}

	var cmd tea.Cmd
	m.sudoInput, cmd = m.sudoInput.Update(msg)
	return m, cmd
}

func (m model) viewSudoPassword() string {
	innerW := sudoBoxWidth - 6
	if innerW > m.width-6 {
		innerW = m.width - 6
	}
	if innerW < 30 {
		innerW = 30
	}

	title := approvalTitleStyle.Render("Sudo Password Required")
	prompt := m.sudoPrompt
	if prompt == "" {
		prompt = "[sudo] password required"
	}

	headline := approvalToolStyle.Render(prompt)

	lines := []string{title, "", headline}

	if m.sudoIncorrectAttempt {
		lines = append(lines, errStyle.Render("Incorrect password, please try again."))
	}

	lines = append(lines, "", m.sudoInput.View(), "")
	help := askHelpStyle.Render("enter confirm · esc/ctrl+c cancel")
	lines = append(lines, help)

	content := strings.Join(lines, "\n")
	return approvalBoxStyle.Width(innerW).Render(content)
}
