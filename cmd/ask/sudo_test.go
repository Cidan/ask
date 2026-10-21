package main

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/tools"
)

// sudoAskPassProcs is a tools.ProcInspector that reports every connection
// as ask's askpass child (300) under a sudo front end (200) started by bash
// tool shell 100.
type sudoAskPassProcs struct{ uid int }

func (p sudoAskPassProcs) PeerPID(*net.UnixConn) (int, int, error) { return 300, p.uid, nil }

func (p sudoAskPassProcs) Process(pid int) (tools.ProcInfo, error) {
	switch pid {
	case 300:
		return tools.ProcInfo{PID: 300, PPID: 200, Name: "ask", RUID: p.uid, EUID: p.uid}, nil
	case 200:
		return tools.ProcInfo{PID: 200, PPID: 100, Name: "sudo", RUID: p.uid, EUID: 0}, nil
	case 100:
		return tools.ProcInfo{PID: 100, PPID: 1, Name: "bash", RUID: p.uid, EUID: p.uid}, nil
	}
	return tools.ProcInfo{}, os.ErrNotExist
}

func (p sudoAskPassProcs) SameExecutableAsSelf(pid int) (bool, error) { return pid == 300, nil }

func (p sudoAskPassProcs) StdoutPipe(int) (tools.PipeID, error) {
	return tools.PipeID{Dev: 1, Ino: 2}, nil
}

func (p sudoAskPassProcs) OwnPipes() (map[tools.PipeID]bool, error) { return nil, nil }

func useSudoAskPassProcs(t *testing.T) {
	t.Helper()
	uid := os.Getuid()
	if uid == 0 {
		t.Skip("the askpass checks only trust a sudo started by a non-root user")
	}
	prev := tools.SudoProcInspector
	tools.SudoProcInspector = sudoAskPassProcs{uid: uid}
	t.Cleanup(func() { tools.SudoProcInspector = prev })
}

// runSudoAskPass runs the helper as sudo's askpass child would and returns
// its stdout.
func runSudoAskPass(t *testing.T, socket string) (string, error) {
	t.Helper()
	t.Setenv(askSudoSocketEnv, socket)
	prevArgs := os.Args
	os.Args = []string{"/usr/local/bin/ask", "[sudo] password for root: "}
	defer func() { os.Args = prevArgs }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevOut := os.Stdout
	os.Stdout = w
	runErr := runAskPassHelper()
	os.Stdout = prevOut
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out), runErr
}

func TestSudoAskPass_RoundTripThroughTheTUIHandler(t *testing.T) {
	isolateHome(t)
	useSudoAskPassProcs(t)
	closeSudoIPCServer()
	t.Cleanup(closeSudoIPCServer)
	server := ensureSudoIPCServer()
	if server == nil {
		t.Skip("the askpass flow is not supported on this platform")
	}
	defer server.RegisterShell(100, globalTUIInteractionHandler, 2)()

	answers := []sudoPasswordReply{{password: "first"}, {password: "second"}, {cancelled: true}}
	var mu sync.Mutex
	var got []sudoPasswordRequestedMsg
	prev := agentSendToProgram
	t.Cleanup(func() { agentSendToProgram = prev })
	agentSendToProgram = func(msg tea.Msg) bool {
		req, ok := msg.(sudoPasswordRequestedMsg)
		if !ok {
			return true
		}
		mu.Lock()
		i := len(got)
		got = append(got, req)
		mu.Unlock()
		req.reply <- answers[i]
		return true
	}

	if out, err := runSudoAskPass(t, server.SocketPath); err != nil || out != "first\n" {
		t.Fatalf("first prompt: (%q, %v)", out, err)
	}
	if out, err := runSudoAskPass(t, server.SocketPath); err != nil || out != "second\n" {
		t.Fatalf("second prompt: (%q, %v)", out, err)
	}
	if out, err := runSudoAskPass(t, server.SocketPath); err == nil || out != "" {
		t.Fatalf("cancelled prompt: (%q, %v), want an error and no output", out, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("modal requests = %d, want 3", len(got))
	}
	if got[0].tabID != 2 || got[0].prompt != "[sudo] password for root: " || got[0].retry {
		t.Errorf("first request = %+v, want tab 2, sudo's prompt, no retry", got[0])
	}
	if !got[1].retry || !got[2].retry {
		t.Errorf("repeat prompts from the same sudo should be flagged as retries: %v %v", got[1].retry, got[2].retry)
	}
}

func TestSudoEnv_AgentBashToolExportsOnlyTheHelperAndSocket(t *testing.T) {
	isolateHome(t)
	var captured []string
	prevRunShell := tools.RunShell
	t.Cleanup(func() { tools.RunShell = prevRunShell })
	tools.RunShell = func(dir, command string, extraEnv ...string) (*tools.ShellHandle, error) {
		captured = append([]string(nil), extraEnv...)
		out := make(chan string)
		close(out)
		done := make(chan tools.ShellResult, 1)
		done <- tools.ShellResult{ExitCode: 0}
		return &tools.ShellHandle{Output: out, Done: done, Kill: func() {}}, nil
	}

	env := &agentToolEnv{Cwd: t.TempDir(), TabID: 42, Jobs: newAgentJobManager()}
	if _, err := tools.RunToolWithJSON(testAgentCtx(), agentBashTool(env), `{"command":"echo test","description":"test"}`); err != nil {
		t.Fatalf("tool.Run failed: %v", err)
	}

	if ensureSudoIPCServer() == nil {
		if len(captured) != 0 {
			t.Errorf("exported %v on a platform without the askpass flow", captured)
		}
		return
	}
	var keys []string
	for _, kv := range captured {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	if strings.Join(keys, ",") != "SUDO_ASKPASS,"+askSudoSocketEnv {
		t.Errorf("exported %v, want only SUDO_ASKPASS and %s", captured, askSudoSocketEnv)
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
	if m2.sudoIncorrectAttempt {
		t.Error("a first prompt is labelled incorrect")
	}

	viewStr := m2.viewSudoPassword()
	if !strings.Contains(viewStr, "Sudo Password Required") {
		t.Errorf("view missing title: %s", viewStr)
	}
	if !strings.Contains(viewStr, "[sudo] password for testuser:") {
		t.Errorf("view missing prompt: %s", viewStr)
	}

	m3, _ := m2.updateSudoPassword(tea.KeyPressMsg{Code: tea.KeyEsc})
	m3Model := m3.(model)
	if m3Model.mode != modeInput {
		t.Errorf("mode after Esc = %v, want modeInput", m3Model.mode)
	}
	reply := <-replyChan
	if !reply.cancelled {
		t.Errorf("expected cancelled reply after Esc")
	}

	retry := reqMsg
	retry.retry = true
	m4 := m.startSudoPassword(retry)
	if !m4.sudoIncorrectAttempt {
		t.Errorf("expected sudoIncorrectAttempt = true when sudo asks again")
	}
	viewRePrompt := m4.viewSudoPassword()
	if !strings.Contains(viewRePrompt, "Incorrect password, please try again.") {
		t.Errorf("re-prompt view missing incorrect password notice: %s", viewRePrompt)
	}
}

func typeRunes(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return m
}

func openSudoModal(t *testing.T, m model) (model, chan sudoPasswordReply) {
	t.Helper()
	reply := make(chan sudoPasswordReply, 1)
	m, _ = runUpdate(t, m, sudoPasswordRequestedMsg{tabID: m.id, prompt: "[sudo] password for u: ", reply: reply})
	if m.mode != modeSudoPassword {
		t.Fatalf("precondition: mode = %v, want modeSudoPassword", m.mode)
	}
	return m, reply
}

func assertNoPasswordInChat(t *testing.T, m *model, fragment string) {
	t.Helper()
	if strings.Contains(m.input.Value(), fragment) {
		t.Errorf("password characters are sitting in the chat composer: %q", m.input.Value())
	}
	for _, line := range m.inputHistory {
		if strings.Contains(line, fragment) {
			t.Errorf("password characters were submitted as a chat prompt to the model: %q", line)
		}
	}
	for _, h := range m.history {
		if strings.Contains(h.text, fragment) {
			t.Errorf("password characters were written into the chat transcript")
			break
		}
	}
}

func TestSudoModal_ApprovalRequestWaitsBehindThePasswordPrompt(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, sudoReply := openSudoModal(t, m)

	approval := make(chan approvalReply, 1)
	m, _ = runUpdate(t, m, approvalRequestMsg{
		tabID:     m.id,
		toolName:  "bash",
		input:     map[string]any{"command": "rm -rf build"},
		toolUseID: "parallel-call",
		reply:     approval,
	})
	if m.mode != modeSudoPassword {
		t.Fatalf("mode = %v after the approval arrived, want the password prompt to stay up", m.mode)
	}

	const pw = "yes-hunter2"
	m = typeRunes(t, m, pw)
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	select {
	case r := <-sudoReply:
		if r.password != pw {
			t.Errorf("sudo received %q, want the whole typed password", r.password)
		}
	default:
		t.Fatal("sudo never received the password")
	}
	if len(approval) != 0 {
		t.Errorf("the approval was answered by password keystrokes: %+v", <-approval)
	}
	if m.mode != modeApproval || m.approvalReply != approval {
		t.Fatalf("mode = %v after the password was sent, want the queued approval on screen", m.mode)
	}
	assertNoPasswordInChat(t, &m, "hunter2")

	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if r := <-approval; r.allow {
		t.Error("deny on the queued approval was recorded as allow")
	}
	if m.mode != modeInput || len(m.pendingModals) != 0 {
		t.Errorf("mode = %v, queue = %d after the last modal closed", m.mode, len(m.pendingModals))
	}
}

func TestSudoModal_OtherModalRequestsWaitForIt(t *testing.T) {
	cases := []struct {
		name string
		msg  func(tabID int) tea.Msg
		mode viewMode
	}{
		{"tool approval from a parallel call", func(tabID int) tea.Msg {
			return approvalRequestMsg{tabID: tabID, toolName: "bash", input: map[string]any{"command": "make"}, reply: make(chan approvalReply, 1)}
		}, modeApproval},
		{"MCP OAuth paste prompt", func(tabID int) tea.Msg {
			return mcpAuthPromptRequestedMsg{tabID: tabID, serverName: "linear", authURL: "https://example.invalid/authorize", reply: make(chan mcpAuthReply, 1)}
		}, modeMCPAuth},
		{"session picker from an earlier /resume", func(tabID int) tea.Msg {
			return sessionsLoadedMsg{tabID: tabID, sessions: []sessionEntry{{id: "A"}}}
		}, modeSessionPicker},
		{"ask question", func(tabID int) tea.Msg {
			return askToolRequestMsg{tabID: tabID, questions: []question{{kind: qPickOne, prompt: "q", options: []string{"a", "b"}}}, reply: make(chan askReply, 1)}
		}, modeAskQuestion},
		{"finalized plan", func(tabID int) tea.Msg {
			return finalizedPlanRequestMsg{tabID: tabID, plan: "plan", reply: make(chan finalizedPlanReply, 1)}
		}, modeFinalizedPlan},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, newFakeProvider())
			m, sudoReply := openSudoModal(t, m)

			m, _ = runUpdate(t, m, tc.msg(m.id))

			if m.mode != modeSudoPassword {
				t.Fatalf("mode = %v after the request arrived, want modeSudoPassword", m.mode)
			}
			if !m.needsUserInput() {
				t.Error("tab with a prompt open and one queued is not flagged as waiting for input")
			}
			m = typeRunes(t, m, "pw")
			m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			select {
			case r := <-sudoReply:
				if r.password != "pw" {
					t.Errorf("sudo received %q, want %q", r.password, "pw")
				}
			default:
				t.Error("sudo reply orphaned")
			}
			if m.mode != tc.mode {
				t.Errorf("mode = %v after the password was sent, want the queued %v", m.mode, tc.mode)
			}
		})
	}
}

func TestSudoRequest_WaitsBehindAnOpenModal(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	approval := make(chan approvalReply, 1)
	m, _ = runUpdate(t, m, approvalRequestMsg{tabID: m.id, toolName: "bash", input: map[string]any{"command": "make"}, reply: approval})
	sudoReply := make(chan sudoPasswordReply, 1)
	m, _ = runUpdate(t, m, sudoPasswordRequestedMsg{tabID: m.id, prompt: "[sudo] password for u: ", reply: sudoReply})

	if m.mode != modeApproval || m.approvalReply != approval {
		t.Fatalf("mode = %v, want the open approval to keep the keyboard", m.mode)
	}
	if !m.needsUserInput() || len(m.pendingModals) != 1 {
		t.Fatalf("sudo request was not queued (queue %d)", len(m.pendingModals))
	}

	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: 'y', Text: "y"})
	if r := <-approval; !r.allow {
		t.Error("approval answer lost")
	}
	if m.mode != modeSudoPassword || m.sudoReply != sudoReply {
		t.Fatalf("mode = %v after the approval closed, want the queued password prompt", m.mode)
	}
	m = typeRunes(t, m, "pw")
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if r := <-sudoReply; r.password != "pw" {
		t.Errorf("sudo received %q, want %q", r.password, "pw")
	}
}

func TestSudoRequest_WaitsBehindAnInlineConfirm(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m.closeTabConfirming = true
	reply := make(chan sudoPasswordReply, 1)
	m, _ = runUpdate(t, m, sudoPasswordRequestedMsg{tabID: m.id, prompt: "p", reply: reply})
	if m.mode == modeSudoPassword || len(m.pendingModals) != 1 {
		t.Fatalf("mode = %v, queue = %d: the prompt opened under the close-tab confirm", m.mode, len(m.pendingModals))
	}

	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.mode != modeSudoPassword || m.sudoReply != reply {
		t.Errorf("mode = %v after the confirm closed, want the queued password prompt", m.mode)
	}
}

func TestSudoModal_SidebarFocusYieldsToTheOpenPrompt(t *testing.T) {
	a := newSidebarTestApp(t, 2)
	na, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	a = na.(app)
	if !a.sidebarFocus {
		t.Fatal("precondition: Tab should focus the sidebar on an idle chat tab")
	}

	sudoReply := make(chan sudoPasswordReply, 1)
	na, _ = a.Update(sudoPasswordRequestedMsg{tabID: a.activeTab().id, prompt: "[sudo] password for u: ", reply: sudoReply})
	a = na.(app)
	sudoTab := a.active

	for _, r := range "kitten99" {
		na, _ = a.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		a = na.(app)
	}
	na, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = na.(app)

	if a.active != sudoTab {
		t.Errorf("a password keystroke ('k') moved focus from the tab with the sudo prompt (%d) to tab %d", sudoTab, a.active)
	}
	if a.sidebarFocus {
		t.Error("sidebar kept focus while a prompt owned the keyboard")
	}
	select {
	case r := <-sudoReply:
		if r.password != "kitten99" {
			t.Errorf("sudo received %q, want %q", r.password, "kitten99")
		}
	default:
		t.Error("sudo never received the password")
	}
	for i := range a.tabs {
		assertNoPasswordInChat(t, a.tabs[i], "itten99")
	}
}

func TestSudoModal_ConcurrentRequestsAreAnsweredInOrder(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, first := openSudoModal(t, m)
	second := make(chan sudoPasswordReply, 1)
	m, _ = runUpdate(t, m, sudoPasswordRequestedMsg{tabID: m.id, prompt: "[sudo] password for u: ", reply: second})

	if m.sudoReply != first || m.sudoIncorrectAttempt {
		t.Fatalf("second request replaced or relabelled the open prompt (incorrect=%v)", m.sudoIncorrectAttempt)
	}

	m = typeRunes(t, m, "pw1")
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if r := <-first; r.password != "pw1" {
		t.Errorf("first sudo received %q, want pw1", r.password)
	}
	if m.mode != modeSudoPassword || m.sudoReply != second || m.sudoInput.Value() != "" {
		t.Fatalf("second prompt not opened fresh after the first was answered")
	}
	m = typeRunes(t, m, "pw2")
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if r := <-second; r.password != "pw2" {
		t.Errorf("second sudo received %q, want pw2", r.password)
	}
}

func TestSudoModal_PasteReachesPasswordField(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, _ = openSudoModal(t, m)

	m, _ = runUpdate(t, m, tea.PasteMsg{Content: "hunter2\n"})

	if got := m.sudoInput.Value(); got != "hunter2" {
		t.Errorf("sudo field holds %d chars after pasting a 7-char password with a trailing newline", len(got))
	}
	assertNoPasswordInChat(t, &m, "hunter2")
}

func TestSudoModal_PasswordNotRetainedAfterSubmit(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, reply := openSudoModal(t, m)
	m = typeRunes(t, m, "hunter2")
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(reply) != 1 {
		t.Fatal("precondition: Enter should answer the sudo request")
	}

	if got := m.sudoInput.Value(); got != "" {
		t.Errorf("model still holds the submitted sudo password (%d chars) after the prompt closed", len(got))
	}
}

func TestTUISudoPrompt_DismissedWhenRequesterGoesAway(t *testing.T) {
	var mu sync.Mutex
	var sent []tea.Msg
	prev := agentSendToProgram
	t.Cleanup(func() { agentSendToProgram = prev })
	agentSendToProgram = func(msg tea.Msg) bool {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, msg)
		return true
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = globalTUIInteractionHandler.RequestSudoPassword(ctx, 3, "[sudo] password for u: ")
	}()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) == 1
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RequestSudoPassword did not return after its context was cancelled")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("messages = %d, want the request and its dismissal", len(sent))
	}
	req := sent[0].(sudoPasswordRequestedMsg)
	dismiss, ok := sent[1].(sudoPasswordDismissMsg)
	if !ok || dismiss.tabID != 3 || dismiss.reply != req.reply {
		t.Errorf("second message = %#v, want a dismissal of the same request on tab 3", sent[1])
	}
}

func TestSudoModal_DismissClosesPromptOrDropsQueuedRequest(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, first := openSudoModal(t, m)
	second := make(chan sudoPasswordReply, 1)
	third := make(chan sudoPasswordReply, 1)
	m, _ = runUpdate(t, m, sudoPasswordRequestedMsg{tabID: m.id, prompt: "p", reply: second})
	m, _ = runUpdate(t, m, sudoPasswordRequestedMsg{tabID: m.id, prompt: "p", reply: third})

	m, _ = runUpdate(t, m, sudoPasswordDismissMsg{tabID: m.id, reply: second})
	if m.sudoReply != first || len(m.pendingModals) != 1 {
		t.Fatalf("dismissing a queued request disturbed the open prompt or kept the request (queue %d)", len(m.pendingModals))
	}

	m = typeRunes(t, m, "half")
	m, _ = runUpdate(t, m, sudoPasswordDismissMsg{tabID: m.id, reply: first})
	if len(first) != 0 {
		t.Error("a dismissed prompt was answered")
	}
	if m.mode != modeSudoPassword || m.sudoReply != third || m.sudoInput.Value() != "" {
		t.Fatalf("after dismissing the open prompt, want the next queued prompt opened empty")
	}

	m, _ = runUpdate(t, m, sudoPasswordDismissMsg{tabID: m.id, reply: third})
	if m.mode != modeInput || len(m.pendingModals) != 0 {
		t.Errorf("mode = %v, queue = %d after every request was withdrawn", m.mode, len(m.pendingModals))
	}
}

func TestSudoModal_MCPAuthDismissDropsQueuedPrompt(t *testing.T) {
	m := newTestModel(t, newFakeProvider())
	m, sudoReply := openSudoModal(t, m)
	m, _ = runUpdate(t, m, mcpAuthPromptRequestedMsg{tabID: m.id, serverName: "linear", authURL: "https://example.invalid", reply: make(chan mcpAuthReply, 1)})

	m, _ = runUpdate(t, m, mcpAuthDismissMsg{tabID: m.id})
	if m.mode != modeSudoPassword || len(m.pendingModals) != 0 {
		t.Fatalf("mode = %v, queue = %d: the loopback-resolved MCP prompt should leave the queue", m.mode, len(m.pendingModals))
	}
	m = typeRunes(t, m, "pw")
	m, _ = runUpdate(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	<-sudoReply
	if m.mode != modeInput {
		t.Errorf("mode = %v, want no stale MCP prompt after the password was sent", m.mode)
	}
}

func TestSudoModal_UnreachableTabsCancelInsteadOfHanging(t *testing.T) {
	t.Run("unknown tab id", func(t *testing.T) {
		a := newSidebarTestApp(t, 2)
		reply := make(chan sudoPasswordReply, 1)
		_, _ = a.Update(sudoPasswordRequestedMsg{tabID: 0, prompt: "p", reply: reply})
		if len(reply) != 1 {
			t.Fatal("request for a tab that does not exist was left unanswered")
		}
		if r := <-reply; !r.cancelled {
			t.Errorf("reply = %+v, want cancelled", r)
		}
	})
	t.Run("workflow tab", func(t *testing.T) {
		m := newTestModel(t, newFakeProvider())
		m.workflowRun = &workflowRunState{}
		reply := make(chan sudoPasswordReply, 1)
		m, _ = runUpdate(t, m, sudoPasswordRequestedMsg{tabID: m.id, prompt: "p", reply: reply})
		if len(reply) != 1 {
			t.Fatal("request on a workflow tab was left unanswered")
		}
		if r := <-reply; !r.cancelled {
			t.Errorf("reply = %+v, want cancelled", r)
		}
		if m.mode == modeSudoPassword {
			t.Error("workflow tab opened a password prompt")
		}
	})
	t.Run("tab closed with a prompt open and requests queued", func(t *testing.T) {
		a := newSidebarTestApp(t, 2)
		reply := make(chan sudoPasswordReply, 1)
		queuedSudo := make(chan sudoPasswordReply, 1)
		queuedApproval := make(chan approvalReply, 1)
		id := a.activeTab().id
		for _, msg := range []tea.Msg{
			sudoPasswordRequestedMsg{tabID: id, prompt: "p", reply: reply},
			sudoPasswordRequestedMsg{tabID: id, prompt: "p", reply: queuedSudo},
			approvalRequestMsg{tabID: id, toolName: "bash", input: map[string]any{"command": "make"}, reply: queuedApproval},
		} {
			na, _ := a.Update(msg)
			a = na.(app)
		}
		_, cmd := a.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'd'})
		for _, msg := range drainBatch(t, cmd) {
			na, _ := a.Update(msg)
			a = na.(app)
		}
		for name, n := range map[string]int{"open prompt": len(reply), "queued sudo": len(queuedSudo), "queued approval": len(queuedApproval)} {
			if n != 1 {
				t.Errorf("%s left unanswered after the tab closed", name)
			}
		}
		if r := <-reply; !r.cancelled {
			t.Errorf("open prompt reply = %+v, want cancelled", r)
		}
		if r := <-queuedSudo; !r.cancelled {
			t.Errorf("queued sudo reply = %+v, want cancelled", r)
		}
		if r := <-queuedApproval; r.allow {
			t.Error("queued approval was allowed on teardown")
		}
	})
}
