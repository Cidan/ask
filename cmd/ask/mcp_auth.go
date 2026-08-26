package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/Cidan/ask/pkg/tools"
)

const mcpAuthBoxWidth = 92

// errMCPAuthCancelled is returned by the prompter when the user dismisses the
// authorize modal (Esc), so tools.AuthorizeMCPServer surfaces a clean cancel
// rather than a timeout.
var errMCPAuthCancelled = errors.New("authorization cancelled")

// mcpAuthPrompter builds the tools.MCPAuthPrompter the Ctrl+S browser hands to
// tools.AuthorizeMCPServer. It routes the authorization URL to tabID's modal
// and blocks the authorizing goroutine until the user pastes the redirect URL
// or cancels. When the loopback callback resolves first, ctx is cancelled and
// the prompter dismisses the modal.
func mcpAuthPrompter(tabID int, serverName string) tools.MCPAuthPrompter {
	return func(ctx context.Context, authURL string) (string, error) {
		reply := make(chan mcpAuthReply, 1)
		if !agentSendToProgram(mcpAuthPromptRequestedMsg{
			tabID:      tabID,
			serverName: serverName,
			authURL:    authURL,
			reply:      reply,
		}) {
			return "", fmt.Errorf("auth UI not ready")
		}
		select {
		case r := <-reply:
			if r.cancelled {
				return "", errMCPAuthCancelled
			}
			return r.redirect, nil
		case <-ctx.Done():
			agentSendToProgram(mcpAuthDismissMsg{tabID: tabID})
			return "", ctx.Err()
		}
	}
}

func (m model) startMCPAuth(msg mcpAuthPromptRequestedMsg) model {
	(&m).clearSelection()
	m.mode = modeMCPAuth
	m.mcpAuthServer = msg.serverName
	m.mcpAuthURL = msg.authURL
	m.mcpAuthInput = ""
	m.mcpAuthReply = msg.reply
	return m
}

func (m model) clearMCPAuth() model {
	m.mode = modeInput
	m.mcpAuthServer = ""
	m.mcpAuthURL = ""
	m.mcpAuthInput = ""
	m.mcpAuthReply = nil
	return m
}

// sendMCPAuth answers the blocked prompter and closes the modal.
func (m model) sendMCPAuth(redirect string, cancelled bool) model {
	if m.mcpAuthReply != nil {
		m.mcpAuthReply <- mcpAuthReply{redirect: redirect, cancelled: cancelled}
	}
	return m.clearMCPAuth()
}

func (m model) updateMCPAuth(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c', msg.Code == tea.KeyEsc:
		return m.sendMCPAuth("", true), nil
	case msg.Code == tea.KeyEnter:
		val := strings.TrimSpace(m.mcpAuthInput)
		if val == "" {
			return m, nil
		}
		return m.sendMCPAuth(val, false), nil
	case msg.Code == tea.KeyBackspace:
		if r := []rune(m.mcpAuthInput); len(r) > 0 {
			m.mcpAuthInput = string(r[:len(r)-1])
		}
		return m, nil
	case msg.Mod == 0 && msg.Text == "c":
		return m, copyTextCmd(m.toast, m.mcpAuthURL)
	case msg.Mod == 0 && msg.Text == "o":
		url := m.mcpAuthURL
		return m, func() tea.Msg {
			_ = tools.MCPOAuthOpenBrowser(url)
			return nil
		}
	case configTextInputKey(msg):
		m.mcpAuthInput += msg.Text
		return m, nil
	}
	return m, nil
}

// applyMCPAuthPaste appends a bracketed paste (typically the redirected
// callback URL copied from the browser's address bar) to the input buffer.
func (m model) applyMCPAuthPaste(content string) (tea.Model, tea.Cmd) {
	m.mcpAuthInput += content
	return m, nil
}

func (m model) viewMCPAuth() string {
	innerW := mcpAuthBoxWidth - 6
	if innerW > m.width-6 {
		innerW = m.width - 6
	}
	if innerW < 40 {
		innerW = 40
	}
	wrap := lipgloss.NewStyle().Width(innerW)

	title := approvalTitleStyle.Render("Authorize MCP Server")
	server := approvalToolStyle.Render(m.mcpAuthServer)

	lines := []string{
		title,
		"",
		server,
		"",
		askHelpStyle.Render("Open this URL in a browser (press c to copy — works over SSH):"),
		wrap.Render(m.mcpAuthURL),
		"",
		askHelpStyle.Render("Then paste the redirected callback URL back here and press enter:"),
	}
	if strings.TrimSpace(m.mcpAuthInput) == "" {
		lines = append(lines, askHelpStyle.Render("(waiting — paste the callback URL, or finish in a local browser)"))
	} else {
		lines = append(lines, wrap.Render(m.mcpAuthInput))
	}
	lines = append(lines,
		"",
		askHelpStyle.Render("c copy link · o open here · paste + enter submit · esc cancel"),
	)
	return approvalBoxStyle.Width(innerW).Render(strings.Join(lines, "\n"))
}
