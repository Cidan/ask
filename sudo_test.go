package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"charm.land/bubbletea/v2"
	"charm.land/fantasy"
)

func TestSudoIPCServer_HandshakeAndTokenValidation(t *testing.T) {
	isolateHome(t)

	server := ensureSudoIPCServer()
	if server == nil {
		t.Fatalf("ensureSudoIPCServer returned nil")
	}

	// Case 1: Connect with invalid token -> expect ERROR: invalid token
	conn, err := net.Dial("unix", server.socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	req := "TOKEN:bad-token\nPROMPT:test\n\n"
	_, _ = conn.Write([]byte(req))
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected response from server")
	}
	if !strings.HasPrefix(scanner.Text(), "ERROR:") {
		t.Errorf("expected ERROR response for invalid token, got: %s", scanner.Text())
	}
	conn.Close()

	// Case 2: Connect with valid token but no tea program -> expect ERROR: no active UI program
	conn, err = net.Dial("unix", server.socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	req = fmt.Sprintf("TOKEN:%s\nPROMPT:test\n\n", server.token)
	_, _ = conn.Write([]byte(req))
	scanner = bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected response from server")
	}
	if !strings.HasPrefix(scanner.Text(), "ERROR:") {
		t.Errorf("expected ERROR for missing tea program, got: %s", scanner.Text())
	}
	conn.Close()
}

func TestSudoIPCServer_HandshakeSuccessAndCancel(t *testing.T) {
	isolateHome(t)

	server := ensureSudoIPCServer()
	if server == nil {
		t.Fatalf("ensureSudoIPCServer returned nil")
	}

	prev := agentSendToProgram
	t.Cleanup(func() { agentSendToProgram = prev })
	msgCh := make(chan sudoPasswordRequestedMsg, 1)
	agentSendToProgram = func(msg tea.Msg) bool {
		if req, ok := msg.(sudoPasswordRequestedMsg); ok {
			msgCh <- req
			return true
		}
		return false
	}

	conn, err := net.Dial("unix", server.socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("TOKEN:%s\nTABID:1\nPROMPT:[sudo] password:\n\n", server.token)
	_, _ = conn.Write([]byte(req))

	var reqMsg sudoPasswordRequestedMsg
	select {
	case reqMsg = <-msgCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for sudoPasswordRequestedMsg")
	}

	if reqMsg.reply == nil {
		t.Fatalf("program did not receive sudoPasswordRequestedMsg")
	}
	if reqMsg.tabID != 1 {
		t.Errorf("tabID = %d, want 1", reqMsg.tabID)
	}

	// Send cancellation reply
	reqMsg.reply <- sudoPasswordReply{cancelled: true}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected response from server")
	}
	if scanner.Text() != "ERROR: cancelled" {
		t.Errorf("got %q, want ERROR: cancelled", scanner.Text())
	}
}

func TestSudoIPCServer_HandshakeSuccessPassword(t *testing.T) {
	isolateHome(t)

	server := ensureSudoIPCServer()
	if server == nil {
		t.Fatalf("ensureSudoIPCServer returned nil")
	}

	prev := agentSendToProgram
	t.Cleanup(func() { agentSendToProgram = prev })
	msgCh := make(chan sudoPasswordRequestedMsg, 1)
	agentSendToProgram = func(msg tea.Msg) bool {
		if req, ok := msg.(sudoPasswordRequestedMsg); ok {
			msgCh <- req
			return true
		}
		return false
	}

	conn, err := net.Dial("unix", server.socketPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("TOKEN:%s\nTABID:2\nPROMPT:[sudo] password:\n\n", server.token)
	_, _ = conn.Write([]byte(req))

	var reqMsg sudoPasswordRequestedMsg
	select {
	case reqMsg = <-msgCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for sudoPasswordRequestedMsg")
	}

	if reqMsg.reply == nil {
		t.Fatalf("program did not receive sudoPasswordRequestedMsg")
	}

	// Send password reply
	reqMsg.reply <- sudoPasswordReply{password: "supersecret"}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("expected response from server")
	}
	if scanner.Text() != "PASSWORD:supersecret" {
		t.Errorf("got %q, want PASSWORD:supersecret", scanner.Text())
	}
}

type testSudoHandlerModel struct {
	onMsg func(tea.Msg)
}

func (m testSudoHandlerModel) Init() tea.Cmd { return nil }
func (m testSudoHandlerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.onMsg != nil {
		m.onMsg(msg)
	}
	return m, nil
}
func (m testSudoHandlerModel) View() tea.View { return tea.View{Content: ""} }

func TestSudoEnv_AgentBashToolAndAskPassHelper(t *testing.T) {
	isolateHome(t)

	server := ensureSudoIPCServer()
	if server == nil {
		t.Fatalf("ensureSudoIPCServer returned nil")
	}

	var capturedExtraEnv []string
	prevRunShell := agentRunShell
	t.Cleanup(func() { agentRunShell = prevRunShell })
	agentRunShell = func(dir, command string, extraEnv ...string) (*shellHandle, error) {
		capturedExtraEnv = extraEnv
		out := make(chan string)
		close(out)
		done := make(chan shellResult, 1)
		done <- shellResult{ExitCode: 0}
		return &shellHandle{Output: out, Done: done, Kill: func() {}}, nil
	}

	env := &agentToolEnv{
		Cwd:   t.TempDir(),
		TabID: 42,
		Jobs:  newAgentJobManager(),
	}

	tool := agentBashTool(env)
	ctx := context.Background()
	_, err := tool.Run(ctx, fantasy.ToolCall{ID: "t", Name: "bash", Input: `{"command": "echo test", "description": "test"}`})
	if err != nil {
		t.Fatalf("tool.Run failed: %v", err)
	}

	hasSudoAskpass := false
	hasSocket := false
	hasToken := false
	hasTabID := false

	for _, e := range capturedExtraEnv {
		if strings.HasPrefix(e, "SUDO_ASKPASS=") {
			hasSudoAskpass = true
		}
		if strings.HasPrefix(e, "ASK_SUDO_SOCKET=") {
			hasSocket = true
		}
		if strings.HasPrefix(e, "ASK_SUDO_TOKEN=") {
			hasToken = true
		}
		if e == "ASK_SUDO_TABID=42" {
			hasTabID = true
		}
	}

	if !hasSudoAskpass || !hasSocket || !hasToken || !hasTabID {
		t.Errorf("agentBashTool extraEnv missing expected variables: %v", capturedExtraEnv)
	}

	// Test runAskPassHelper when env vars are configured
	t.Setenv("ASK_SUDO_SOCKET", server.socketPath)
	t.Setenv("ASK_SUDO_TOKEN", server.token)
	t.Setenv("ASK_SUDO_TABID", "42")

	prevSend := agentSendToProgram
	t.Cleanup(func() { agentSendToProgram = prevSend })
	agentSendToProgram = func(msg tea.Msg) bool {
		if req, ok := msg.(sudoPasswordRequestedMsg); ok {
			go func() {
				req.reply <- sudoPasswordReply{password: "pass123"}
			}()
			return true
		}
		return false
	}

	if err := runAskPassHelper(); err != nil {
		t.Errorf("runAskPassHelper failed: %v", err)
	}
}

func TestSudoPasswordModal_UpdateAndView(t *testing.T) {
	m := newTestModel(t, newFakeProvider())

	replyChan := make(chan sudoPasswordReply, 1)
	reqMsg := sudoPasswordRequestedMsg{
		tabID:  1,
		prompt: "[sudo] password for testuser:",
		reply:  replyChan,
	}

	m2 := m.startSudoPassword(reqMsg)
	if m2.mode != modeSudoPassword {
		t.Fatalf("mode = %v, want modeSudoPassword", m2.mode)
	}
	if m2.sudoPrompt != "[sudo] password for testuser:" {
		t.Errorf("sudoPrompt = %q, want [sudo] password for testuser:", m2.sudoPrompt)
	}

	viewStr := m2.viewSudoPassword()
	if !strings.Contains(viewStr, "Sudo Password Required") {
		t.Errorf("view missing title: %s", viewStr)
	}
	if !strings.Contains(viewStr, "[sudo] password for testuser:") {
		t.Errorf("view missing prompt: %s", viewStr)
	}

	// Test Esc key cancels password modal
	m3, _ := m2.updateSudoPassword(tea.KeyPressMsg{Code: tea.KeyEsc})
	m3Model := m3.(model)
	if m3Model.mode != modeInput {
		t.Errorf("mode after Esc = %v, want modeInput", m3Model.mode)
	}
	reply := <-replyChan
	if !reply.cancelled {
		t.Errorf("expected cancelled reply after Esc")
	}

	// Test re-prompt sets incorrectAttempt flag
	m4 := m.startSudoPassword(reqMsg)  // first prompt
	m5 := m4.startSudoPassword(reqMsg) // re-prompt while in modeSudoPassword
	if !m5.sudoIncorrectAttempt {
		t.Errorf("expected sudoIncorrectAttempt = true on re-prompt")
	}
	viewRePrompt := m5.viewSudoPassword()
	if !strings.Contains(viewRePrompt, "Incorrect password, please try again.") {
		t.Errorf("re-prompt view missing incorrect password notice: %s", viewRePrompt)
	}
}
