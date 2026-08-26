package main

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func newMCPAuthModel(t *testing.T) (model, chan mcpAuthReply) {
	t.Helper()
	m := newTestModel(t, newFakeProvider())
	reply := make(chan mcpAuthReply, 1)
	m = m.startMCPAuth(mcpAuthPromptRequestedMsg{
		tabID:      m.id,
		serverName: "slack",
		authURL:    "https://as.example/authorize?state=S",
		reply:      reply,
	})
	return m, reply
}

func TestStartMCPAuth_SetsModeAndFields(t *testing.T) {
	m, _ := newMCPAuthModel(t)
	if m.mode != modeMCPAuth {
		t.Errorf("mode=%v want modeMCPAuth", m.mode)
	}
	if m.mcpAuthServer != "slack" || m.mcpAuthURL == "" {
		t.Errorf("fields not populated: server=%q url=%q", m.mcpAuthServer, m.mcpAuthURL)
	}
	if m.mcpAuthReply == nil {
		t.Error("reply channel should be wired up")
	}
}

func TestClearMCPAuth_RestoresInputMode(t *testing.T) {
	m, _ := newMCPAuthModel(t)
	m.mcpAuthInput = "junk"
	got := m.clearMCPAuth()
	if got.mode != modeInput || got.mcpAuthServer != "" || got.mcpAuthURL != "" || got.mcpAuthInput != "" || got.mcpAuthReply != nil {
		t.Errorf("clear left residue: %+v", got)
	}
}

func TestUpdateMCPAuth_EnterSubmitsPastedRedirect(t *testing.T) {
	m, reply := newMCPAuthModel(t)
	m.mcpAuthInput = "http://127.0.0.1:9/callback?code=C&state=S"
	mm, _ := m.updateMCPAuth(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := mm.(model)
	select {
	case r := <-reply:
		if r.cancelled || r.redirect != "http://127.0.0.1:9/callback?code=C&state=S" {
			t.Errorf("reply=%+v", r)
		}
	default:
		t.Fatal("enter must deliver the pasted redirect")
	}
	if got.mode != modeInput {
		t.Errorf("mode after submit=%v want modeInput", got.mode)
	}
}

func TestUpdateMCPAuth_EnterEmptyIsNoop(t *testing.T) {
	m, reply := newMCPAuthModel(t)
	mm, _ := m.updateMCPAuth(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := mm.(model)
	select {
	case r := <-reply:
		t.Fatalf("empty enter must not submit; got %+v", r)
	default:
	}
	if got.mode != modeMCPAuth {
		t.Errorf("empty enter must keep the modal open; mode=%v", got.mode)
	}
}

func TestUpdateMCPAuth_EscCancels(t *testing.T) {
	m, reply := newMCPAuthModel(t)
	mm, _ := m.updateMCPAuth(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := mm.(model)
	select {
	case r := <-reply:
		if !r.cancelled {
			t.Errorf("esc must cancel; got %+v", r)
		}
	default:
		t.Fatal("esc must deliver a cancel reply")
	}
	if got.mode != modeInput {
		t.Errorf("mode after esc=%v want modeInput", got.mode)
	}
}

func TestUpdateMCPAuth_PasteAppendsViaDispatcher(t *testing.T) {
	m, _ := newMCPAuthModel(t)
	got, _ := runUpdate(t, m, tea.PasteMsg{Content: "http://127.0.0.1/callback?code=C"})
	if got.mcpAuthInput != "http://127.0.0.1/callback?code=C" {
		t.Errorf("paste must append to the input buffer: %q", got.mcpAuthInput)
	}
	if got.mode != modeMCPAuth {
		t.Errorf("paste must not close the modal; mode=%v", got.mode)
	}
}

func TestUpdateMCPAuth_DismissClearsWithoutReply(t *testing.T) {
	m, reply := newMCPAuthModel(t)
	got, _ := runUpdate(t, m, mcpAuthDismissMsg{tabID: m.id})
	if got.mode != modeInput {
		t.Errorf("dismiss must close the modal; mode=%v", got.mode)
	}
	select {
	case r := <-reply:
		t.Fatalf("dismiss must not answer the prompter; got %+v", r)
	default:
	}
}

func TestMCPAuthPrompter_ReturnsPastedRedirect(t *testing.T) {
	prev := agentSendToProgram
	t.Cleanup(func() { agentSendToProgram = prev })
	var captured mcpAuthPromptRequestedMsg
	agentSendToProgram = func(msg tea.Msg) bool {
		if req, ok := msg.(mcpAuthPromptRequestedMsg); ok {
			captured = req
			req.reply <- mcpAuthReply{redirect: "http://x/callback?code=C&state=S"}
		}
		return true
	}
	p := mcpAuthPrompter(7, "slack")
	got, err := p(context.Background(), "https://as/authorize?state=S")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://x/callback?code=C&state=S" {
		t.Errorf("prompter returned %q", got)
	}
	if captured.tabID != 7 || captured.serverName != "slack" || captured.authURL == "" {
		t.Errorf("request not populated: %+v", captured)
	}
}

func TestMCPAuthPrompter_CtxCancelDismissesAndErrors(t *testing.T) {
	prev := agentSendToProgram
	t.Cleanup(func() { agentSendToProgram = prev })
	dismissed := make(chan int, 1)
	agentSendToProgram = func(msg tea.Msg) bool {
		switch mm := msg.(type) {
		case mcpAuthPromptRequestedMsg:
			// never reply: force the ctx-cancel path
		case mcpAuthDismissMsg:
			dismissed <- mm.tabID
		}
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := mcpAuthPrompter(3, "slack")
	done := make(chan error, 1)
	go func() {
		_, err := p(ctx, "https://as/authorize")
		done <- err
	}()
	cancel()
	select {
	case tabID := <-dismissed:
		if tabID != 3 {
			t.Errorf("dismiss tabID=%d want 3", tabID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel must dismiss the modal")
	}
	if err := <-done; err == nil {
		t.Error("prompter must return the ctx error on cancel")
	}
}
