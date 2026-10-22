package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/herdr"
)

func TestHerdrStatus_RollsTabsUpIntoOnePaneState(t *testing.T) {
	a := newSidebarTestApp(t, 2)

	if st := a.herdrStatus(); st.State != herdr.Idle || st.Message != "" {
		t.Errorf("idle tabs = %+v, want idle", st)
	}

	a.tabs[1].testBusy = true
	if st := a.herdrStatus(); st.State != herdr.Working {
		t.Errorf("a busy background tab = %v, want working", st.State)
	}

	a.tabs[1].mode = modeApproval
	a.tabs[1].approvalTool = "bash"
	a.tabs[1].tabTitle = "Fix CI"
	if st := a.herdrStatus(); st.State != herdr.Blocked || st.Message != "Fix CI: approve bash" {
		t.Errorf("background tab waiting on an approval = %+v, want blocked naming the tab and the tool", st)
	}

	a.tabs[0].mode = modeSudoPassword
	a.tabs[0].tabTitle = "Deploy"
	if st := a.herdrStatus(); st.State != herdr.Blocked || st.Message != "Deploy: sudo password" {
		t.Errorf("active and background tab both blocked = %+v, want the active tab's reason", st)
	}
}

func TestHerdrStatus_BlockedReasons(t *testing.T) {
	cases := []struct {
		name  string
		setup func(m *model)
		want  string
	}{
		{"approval", func(m *model) { m.mode, m.approvalTool = modeApproval, "edit" }, "approve edit"},
		{"sudo prompt", func(m *model) { m.mode = modeSudoPassword }, "sudo password"},
		{"question", func(m *model) { m.mode = modeAskQuestion }, "answer a question"},
		{"MCP sign-in", func(m *model) { m.mode, m.mcpAuthServer = modeMCPAuth, "linear" }, "authorize linear"},
		{"request queued behind an overlay", func(m *model) {
			m.mode = modeConfig
			m.pendingModals = []tea.Msg{sudoPasswordRequestedMsg{tabID: m.id}}
		}, "waiting for input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newSidebarTestApp(t, 1)
			tc.setup(a.tabs[0])
			if st := a.herdrStatus(); st.State != herdr.Blocked || st.Message != tc.want {
				t.Errorf("status = %+v, want blocked: %q", st, tc.want)
			}
		})
	}
}

func TestHerdrStatus_ResumeAndDisplayFollowTheActiveTab(t *testing.T) {
	a := newSidebarTestApp(t, 2)

	st := a.herdrStatus()
	if st.Resume != nil || st.SessionID != "" || st.Title != "" {
		t.Errorf("fresh tab = %+v, want no session, resume command, or title", st)
	}
	if st.Tokens["tabs"] != "2" || st.Tokens["model"] != a.activeTab().sidebarMeta() {
		t.Errorf("tokens = %v, want the tab count and the active tab's provider/model", st.Tokens)
	}

	a.tabs[0].virtualSessionID = "vs-0a1b"
	a.tabs[0].tabTitle = "Fix CI"
	a.tabs[0].tabTopic = "flaky test"
	st = a.herdrStatus()
	if st.SessionID != "vs-0a1b" || !slices.Equal(st.Resume, []string{"ask", "resume", "vs-0a1b"}) {
		t.Errorf("session = %q resume = %v, want `ask resume vs-0a1b`", st.SessionID, st.Resume)
	}
	if st.Title != "Fix CI · flaky test" {
		t.Errorf("title = %q, want the sidebar title", st.Title)
	}

	a.tabs[0].workflowRun = &workflowRunState{supplanted: &workflowTabSnapshot{virtualSessionID: "vs-chat"}}
	if st := a.herdrStatus(); st.SessionID != "vs-chat" {
		t.Errorf("workflow-supplanted tab resumes %q, want the chat it hands back to", st.SessionID)
	}

	na, _ := a.switchTab(1)
	a = na.(app)
	if st := a.herdrStatus(); st.Resume != nil || st.Title != "" {
		t.Errorf("after switching to a fresh tab = %+v, want its (empty) session and title", st)
	}
}

func TestHerdrPane_AppReportsChangesAndReclaimsAfterResume(t *testing.T) {
	dir, err := os.MkdirTemp("", "herdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "herdr.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	reports := make(chan map[string]any, 32)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct {
					ID     string         `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if json.Unmarshal(line, &req) != nil {
					return
				}
				if req.Method == "pane.report_agent" {
					reports <- req.Params
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"id": req.ID, "result": map[string]any{}})
			}(conn)
		}
	}()
	nextReport := func() map[string]any {
		t.Helper()
		select {
		case r := <-reports:
			return r
		case <-time.After(2 * time.Second):
			t.Fatal("herdr never received a state report")
			return nil
		}
	}

	prev := herdrPane
	herdrPane = herdr.New(path, "w1:p1")
	t.Cleanup(func() {
		herdrPane.Release()
		herdrPane = prev
	})

	a := newSidebarTestApp(t, 1)
	a.tabs[0].virtualSessionID = "vs-1"
	na, _ := a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	a = na.(app)
	if r := nextReport(); r["state"] != "idle" || r["pane_id"] != "w1:p1" || r["agent_session_id"] != "vs-1" {
		t.Errorf("first report = %v, want idle with the tab's session", r)
	}

	a.tabs[0].testBusy = true
	na, _ = a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	a = na.(app)
	if r := nextReport(); r["state"] != "working" {
		t.Errorf("report after a turn started = %v, want working", r)
	}

	na, _ = a.Update(tea.ResumeMsg{})
	a = na.(app)
	if r := nextReport(); r["state"] != "working" || r["resume_argv"] == nil {
		t.Errorf("report after resuming from suspend = %v, want the whole status again", r)
	}
}
