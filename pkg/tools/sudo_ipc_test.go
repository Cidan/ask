package tools

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/engine"
)

const testSudoPassword = "hunter2-SECRET"

const (
	fakeAskPID    = 50
	fakeShellPID  = 100
	fakeSudoPID   = 200
	fakeHelperPID = 300
)

type fakeProc struct {
	info      ProcInfo
	sameExe   bool
	stdout    PipeID
	stdoutErr error
}

// fakeProcTable is a ProcInspector over a scripted process table: whoever
// connects is reported as peer.
type fakeProcTable struct {
	mu      sync.Mutex
	peer    int
	peerUID int
	peerErr error
	procs   map[int]fakeProc
	own     map[PipeID]bool
}

func (f *fakeProcTable) PeerPID(*net.UnixConn) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peer, f.peerUID, f.peerErr
}

func (f *fakeProcTable) Process(pid int) (ProcInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[pid]
	if !ok {
		return ProcInfo{}, os.ErrNotExist
	}
	return p.info, nil
}

func (f *fakeProcTable) SameExecutableAsSelf(pid int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[pid]
	if !ok {
		return false, os.ErrNotExist
	}
	return p.sameExe, nil
}

func (f *fakeProcTable) StdoutPipe(pid int) (PipeID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[pid]
	if !ok {
		return PipeID{}, os.ErrNotExist
	}
	return p.stdout, p.stdoutErr
}

func (f *fakeProcTable) OwnPipes() (map[PipeID]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.own, nil
}

func (f *fakeProcTable) set(pid int, edit func(*fakeProc)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.procs[pid]
	edit(&p)
	f.procs[pid] = p
}

func (f *fakeProcTable) setPeer(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peer = pid
}

// askPassTree scripts the process tree a real `sudo -A` from a bash tool
// command produces: ask (50) → bash tool shell (100) → sudo front end
// (200, real uid the user, effective uid root) → ask as the askpass helper
// (300) writing to sudo's private pipe.
func askPassTree(t *testing.T) *fakeProcTable {
	t.Helper()
	uid := os.Getuid()
	if uid == 0 {
		t.Skip("the askpass checks only trust a sudo started by a non-root user")
	}
	f := &fakeProcTable{
		peer:    fakeHelperPID,
		peerUID: uid,
		procs: map[int]fakeProc{
			fakeAskPID:    {info: ProcInfo{PID: fakeAskPID, PPID: 1, Name: "ask", RUID: uid, EUID: uid}, sameExe: true},
			fakeShellPID:  {info: ProcInfo{PID: fakeShellPID, PPID: fakeAskPID, Name: "bash", RUID: uid, EUID: uid}},
			fakeSudoPID:   {info: ProcInfo{PID: fakeSudoPID, PPID: fakeShellPID, Name: "sudo", RUID: uid, EUID: 0}},
			fakeHelperPID: {info: ProcInfo{PID: fakeHelperPID, PPID: fakeSudoPID, Name: "ask", RUID: uid, EUID: uid}, sameExe: true, stdout: PipeID{Dev: 13, Ino: 9001}},
		},
		own: map[PipeID]bool{{Dev: 13, Ino: 4242}: true},
	}
	prev := SudoProcInspector
	SudoProcInspector = f
	t.Cleanup(func() { SudoProcInspector = prev })
	return f
}

type sudoPromptCall struct {
	tabID int
	req   engine.SudoPasswordRequest
}

type recordingSudoHandler struct {
	engine.HeadlessInteractionHandler
	password string
	hold     chan struct{}
	ctxs     chan context.Context

	mu    sync.Mutex
	calls []sudoPromptCall
}

func newRecordingSudoHandler(password string) *recordingSudoHandler {
	return &recordingSudoHandler{
		HeadlessInteractionHandler: engine.HeadlessInteractionHandler{AutoApproveTools: true},
		password:                   password,
		ctxs:                       make(chan context.Context, 16),
	}
}

func (h *recordingSudoHandler) PromptSudoPassword(ctx context.Context, tabID int, req engine.SudoPasswordRequest) (engine.SudoPasswordResponse, error) {
	h.mu.Lock()
	h.calls = append(h.calls, sudoPromptCall{tabID: tabID, req: req})
	h.mu.Unlock()
	h.ctxs <- ctx
	if h.hold != nil {
		select {
		case <-ctx.Done():
			return engine.SudoPasswordResponse{}, ctx.Err()
		case <-h.hold:
		}
	}
	h.mu.Lock()
	password := h.password
	h.mu.Unlock()
	if password == "" {
		return engine.SudoPasswordResponse{Cancelled: true}, nil
	}
	return engine.SudoPasswordResponse{Password: password}, nil
}

func (h *recordingSudoHandler) setPassword(pw string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.password = pw
}

func (h *recordingSudoHandler) asked() []sudoPromptCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sudoPromptCall(nil), h.calls...)
}

func newTestSudoServer(t *testing.T) *SudoIPCServer {
	t.Helper()
	s, err := newSudoIPCServer()
	if err != nil {
		t.Fatalf("start sudo server: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func shortSudoWait(t *testing.T) {
	t.Helper()
	prev := sudoShellWait
	sudoShellWait = 50 * time.Millisecond
	t.Cleanup(func() { sudoShellWait = prev })
}

// runAskPassHelperCapturing runs RunAskPassHelper as the askpass child sudo
// execs and returns what it wrote to stdout. ASK_SUDO_SOCKET is set
// explicitly so a test run from inside a live ask session never dials that
// session's socket.
func runAskPassHelperCapturing(t *testing.T, prompt, socket string) (string, error) {
	t.Helper()
	t.Setenv(AskSudoSocketEnv, socket)
	prevArgs := os.Args
	os.Args = []string{"/usr/local/bin/ask", prompt}
	defer func() { os.Args = prevArgs }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevStdout := os.Stdout
	os.Stdout = w
	runErr := RunAskPassHelper()
	os.Stdout = prevStdout
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out), runErr
}

func TestSudoIPCServer_DeliversPasswordToSudosAskPassChild(t *testing.T) {
	askPassTree(t)
	srv := newTestSudoServer(t)
	h := newRecordingSudoHandler(testSudoPassword)
	defer srv.RegisterShell(fakeShellPID, h, 7)()

	out, err := runAskPassHelperCapturing(t, "[sudo] password for root: ", srv.SocketPath)
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	if out != testSudoPassword+"\n" {
		t.Errorf("helper wrote %q to sudo, want the password and a newline", out)
	}
	calls := h.asked()
	if len(calls) != 1 {
		t.Fatalf("modal requests = %d, want 1", len(calls))
	}
	if calls[0].tabID != 7 || calls[0].req.Prompt != "[sudo] password for root: " || calls[0].req.Retry {
		t.Errorf("modal request = %+v, want tab 7, sudo's prompt, no retry", calls[0])
	}
}

func TestSudoIPCServer_RefusesPeersThatAreNotSudosAskPassChild(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fakeProcTable)
	}{
		{"agent runs $SUDO_ASKPASS or ask directly from the shell", func(f *fakeProcTable) {
			f.set(fakeHelperPID, func(p *fakeProc) { p.info.PPID = fakeShellPID })
		}},
		{"another program speaks the protocol", func(f *fakeProcTable) {
			f.set(fakeHelperPID, func(p *fakeProc) { p.sameExe = false })
		}},
		{"parent is named sudo but holds no root euid", func(f *fakeProcTable) {
			f.set(fakeSudoPID, func(p *fakeProc) { p.info.EUID = p.info.RUID })
		}},
		{"sudo was started by another user", func(f *fakeProcTable) {
			f.set(fakeSudoPID, func(p *fakeProc) { p.info.RUID = os.Getuid() + 1 })
		}},
		{"helper stdout is a file or terminal", func(f *fakeProcTable) {
			f.set(fakeHelperPID, func(p *fakeProc) { p.stdoutErr = os.ErrInvalid })
		}},
		{"helper stdout flows back into ask's tool output", func(f *fakeProcTable) {
			f.set(fakeHelperPID, func(p *fakeProc) { p.stdout = PipeID{Dev: 13, Ino: 4242} })
		}},
		{"sudo outside any bash tool command (a daemon that inherited the env)", func(f *fakeProcTable) {
			f.set(fakeSudoPID, func(p *fakeProc) { p.info.PPID = 1 })
		}},
		{"peer runs as another uid", func(f *fakeProcTable) {
			f.mu.Lock()
			f.peerUID = os.Getuid() + 1
			f.mu.Unlock()
		}},
		{"peer credentials unreadable", func(f *fakeProcTable) {
			f.mu.Lock()
			f.peerErr = os.ErrPermission
			f.mu.Unlock()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortSudoWait(t)
			f := askPassTree(t)
			tc.setup(f)
			srv := newTestSudoServer(t)
			h := newRecordingSudoHandler(testSudoPassword)
			defer srv.RegisterShell(fakeShellPID, h, 1)()

			out, err := runAskPassHelperCapturing(t, "[sudo] password for root: ", srv.SocketPath)

			if err == nil || !strings.HasPrefix(err.Error(), "refused: ") {
				t.Errorf("helper error = %v, want a refusal", err)
			}
			if out != "" {
				t.Errorf("helper wrote %q to stdout", out)
			}
			if calls := h.asked(); len(calls) != 0 {
				t.Errorf("password modal opened (%+v)", calls)
			}
		})
	}
}

func TestSudoIPCServer_RefusesThisTestProcessWithRealProcfs(t *testing.T) {
	shortSudoWait(t)
	srv := newTestSudoServer(t)
	h := newRecordingSudoHandler(testSudoPassword)
	defer srv.RegisterShell(os.Getppid(), h, 1)()

	out, err := runAskPassHelperCapturing(t, "[sudo] password for root: ", srv.SocketPath)

	if err == nil || out != "" {
		t.Errorf("a process that is not sudo's askpass child got (%q, %v), want a refusal and no output", out, err)
	}
	if len(h.asked()) != 0 {
		t.Error("password modal opened for a process that is not sudo's askpass child")
	}
}

func TestSudoIPCServer_RefusesConnectionsThatAreNotUnixSockets(t *testing.T) {
	askPassTree(t)
	srv := newTestSudoServer(t)
	h := newRecordingSudoHandler(testSudoPassword)
	defer srv.RegisterShell(fakeShellPID, h, 1)()

	client, server := net.Pipe()
	defer client.Close()
	go srv.handleConn(server)
	go func() { _ = json.NewEncoder(client).Encode(sudoAskPassRequest{Prompt: "p"}) }()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp sudoAskPassResponse
	_ = json.NewDecoder(client).Decode(&resp)

	if resp.Password != "" || !strings.HasPrefix(resp.Error, "refused: ") {
		t.Errorf("response = %+v, want a refusal", resp)
	}
	if len(h.asked()) != 0 {
		t.Error("password modal opened for a connection whose peer cannot be identified")
	}
}

func TestSudoIPCServer_RoutesEachShellToItsOwnHandler(t *testing.T) {
	f := askPassTree(t)
	uid := os.Getuid()
	f.set(101, func(p *fakeProc) { p.info = ProcInfo{PID: 101, PPID: fakeAskPID, Name: "bash", RUID: uid, EUID: uid} })
	f.set(201, func(p *fakeProc) { p.info = ProcInfo{PID: 201, PPID: 101, Name: "sudo", RUID: uid, EUID: 0} })
	f.set(301, func(p *fakeProc) {
		*p = fakeProc{info: ProcInfo{PID: 301, PPID: 201, Name: "ask", RUID: uid, EUID: uid}, sameExe: true, stdout: PipeID{Dev: 13, Ino: 9002}}
	})
	srv := newTestSudoServer(t)
	tui := newRecordingSudoHandler(testSudoPassword)
	headless := newRecordingSudoHandler("")
	defer srv.RegisterShell(fakeShellPID, tui, 1)()
	defer srv.RegisterShell(101, headless, 2)()

	out, err := runAskPassHelperCapturing(t, "p", srv.SocketPath)
	if err != nil || out != testSudoPassword+"\n" {
		t.Errorf("sudo under the TUI tab's shell got (%q, %v), want the password", out, err)
	}
	f.setPeer(301)
	if _, err := runAskPassHelperCapturing(t, "p", srv.SocketPath); err == nil {
		t.Error("sudo under the headless shell was answered, want its handler's cancel")
	}

	if calls := tui.asked(); len(calls) != 1 || calls[0].tabID != 1 {
		t.Errorf("TUI handler calls = %+v, want one for tab 1", calls)
	}
	if calls := headless.asked(); len(calls) != 1 || calls[0].tabID != 2 {
		t.Errorf("headless handler calls = %+v, want one for tab 2", calls)
	}
}

func TestSudoIPCServer_FlagsRetryOnlyForTheSameSudo(t *testing.T) {
	f := askPassTree(t)
	uid := os.Getuid()
	srv := newTestSudoServer(t)
	h := newRecordingSudoHandler(testSudoPassword)
	defer srv.RegisterShell(fakeShellPID, h, 1)()

	for i := 0; i < 2; i++ {
		if _, err := runAskPassHelperCapturing(t, "p", srv.SocketPath); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	f.set(202, func(p *fakeProc) { p.info = ProcInfo{PID: 202, PPID: fakeShellPID, Name: "sudo", RUID: uid, EUID: 0} })
	f.set(fakeHelperPID, func(p *fakeProc) { p.info.PPID = 202 })
	if _, err := runAskPassHelperCapturing(t, "p", srv.SocketPath); err != nil {
		t.Fatalf("request from a second sudo: %v", err)
	}

	calls := h.asked()
	if len(calls) != 3 {
		t.Fatalf("modal requests = %d, want 3", len(calls))
	}
	if calls[0].req.Retry || !calls[1].req.Retry || calls[2].req.Retry {
		t.Errorf("retry flags = %v %v %v, want false true false", calls[0].req.Retry, calls[1].req.Retry, calls[2].req.Retry)
	}
}

type requestOnlySudoHandler struct {
	engine.HeadlessInteractionHandler
	prompts chan string
}

func (h requestOnlySudoHandler) RequestSudoPassword(ctx context.Context, tabID int, prompt string) (engine.SudoPasswordResponse, error) {
	h.prompts <- prompt
	return engine.SudoPasswordResponse{Password: testSudoPassword}, nil
}

func TestSudoIPCServer_FallsBackToRequestSudoPassword(t *testing.T) {
	askPassTree(t)
	srv := newTestSudoServer(t)
	h := requestOnlySudoHandler{prompts: make(chan string, 1)}
	defer srv.RegisterShell(fakeShellPID, h, 1)()

	out, err := runAskPassHelperCapturing(t, "[sudo] password for root: ", srv.SocketPath)
	if err != nil || out != testSudoPassword+"\n" {
		t.Fatalf("helper got (%q, %v), want the password from RequestSudoPassword", out, err)
	}
	if got := <-h.prompts; got != "[sudo] password for root: " {
		t.Errorf("prompt = %q", got)
	}
}

func TestSudoIPCServer_CancelledRequestIsNotARetry(t *testing.T) {
	askPassTree(t)
	srv := newTestSudoServer(t)
	h := newRecordingSudoHandler("")
	defer srv.RegisterShell(fakeShellPID, h, 1)()

	_, _ = runAskPassHelperCapturing(t, "p", srv.SocketPath)
	h.setPassword(testSudoPassword)
	_, _ = runAskPassHelperCapturing(t, "p", srv.SocketPath)

	if calls := h.asked(); len(calls) != 2 || calls[1].req.Retry {
		t.Errorf("calls = %+v, want the request after a cancel to be a fresh prompt", calls)
	}
}

func TestSudoIPCServer_WaitsForTheShellToBeRegistered(t *testing.T) {
	askPassTree(t)
	srv := newTestSudoServer(t)
	h := newRecordingSudoHandler(testSudoPassword)
	unregister := make(chan func(), 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		unregister <- srv.RegisterShell(fakeShellPID, h, 1)
	}()
	t.Cleanup(func() { (<-unregister)() })

	out, err := runAskPassHelperCapturing(t, "p", srv.SocketPath)
	if err != nil || out != testSudoPassword+"\n" {
		t.Errorf("request racing the shell's registration got (%q, %v), want the password", out, err)
	}
}

func TestSudoIPCServer_PromptAndPasswordPassThroughVerbatim(t *testing.T) {
	askPassTree(t)
	srv := newTestSudoServer(t)
	const pw = "  hunter2 \t:x"
	h := newRecordingSudoHandler(pw)
	defer srv.RegisterShell(fakeShellPID, h, 4)()

	const prompt = "[sudo] password:\nTABID:7\n\nTOKEN:x"
	out, err := runAskPassHelperCapturing(t, prompt, srv.SocketPath)
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	if out != pw+"\n" {
		t.Errorf("helper wrote %q to sudo, want %q", out, pw+"\n")
	}
	if calls := h.asked(); len(calls) != 1 || calls[0].req.Prompt != prompt || calls[0].tabID != 4 {
		t.Errorf("modal request = %+v, want the verbatim prompt on the registered tab 4", calls)
	}
}

func TestSudoIPCServer_CancelsPromptWhenHelperHangsUp(t *testing.T) {
	askPassTree(t)
	srv := newTestSudoServer(t)
	h := newRecordingSudoHandler(testSudoPassword)
	h.hold = make(chan struct{})
	t.Cleanup(func() { close(h.hold) })
	defer srv.RegisterShell(fakeShellPID, h, 1)()

	conn, err := net.Dial("unix", srv.SocketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := json.NewEncoder(conn).Encode(sudoAskPassRequest{Prompt: "p"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var ctx context.Context
	select {
	case ctx = <-h.ctxs:
	case <-time.After(2 * time.Second):
		t.Fatal("password modal was never requested")
	}
	_ = conn.Close()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Error("the helper hung up (sudo finished, was killed, or timed out) but the prompt was never cancelled")
	}
}

func TestAskPassHelper_FailurePathsWriteNothingToStdout(t *testing.T) {
	staleSocket := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "s")
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = l.Close()
		return path
	}
	regularFile := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name     string
		password string
		socket   func(t *testing.T, srv *SudoIPCServer) string
	}{
		{"socket env empty", testSudoPassword, func(*testing.T, *SudoIPCServer) string { return "" }},
		{"socket path missing", testSudoPassword, func(t *testing.T, _ *SudoIPCServer) string { return filepath.Join(t.TempDir(), "gone") }},
		{"stale socket from an exited ask", testSudoPassword, func(t *testing.T, _ *SudoIPCServer) string { return staleSocket(t) }},
		{"socket path is a regular file", testSudoPassword, func(t *testing.T, _ *SudoIPCServer) string { return regularFile(t) }},
		{"user cancels the prompt", "", func(_ *testing.T, srv *SudoIPCServer) string { return srv.SocketPath }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			askPassTree(t)
			srv := newTestSudoServer(t)
			defer srv.RegisterShell(fakeShellPID, newRecordingSudoHandler(tc.password), 1)()

			out, err := runAskPassHelperCapturing(t, "p", tc.socket(t, srv))

			if err == nil {
				t.Error("helper succeeded, want an error so sudo sees a failed askpass")
			}
			if out != "" {
				t.Errorf("helper wrote %q to stdout on a failure path; sudo would take it as the password", out)
			}
		})
	}
}

func TestIsAskPassInvocation(t *testing.T) {
	const askpass = "/home/u/go/bin/ask"
	cases := []struct {
		name   string
		argv   []string
		socket string
		parent func(f *fakeProcTable)
		want   bool
	}{
		{"sudo execs the helper", []string{askpass, "[sudo] password for root: "}, "/tmp/s", nil, true},
		{"agent runs ask --help", []string{"ask", "--help"}, "/tmp/s", func(f *fakeProcTable) {
			f.set(fakeHelperPID, func(p *fakeProc) { p.info.PPID = fakeShellPID })
		}, false},
		{"agent runs \"$SUDO_ASKPASS\" directly", []string{askpass, "Password:"}, "/tmp/s", func(f *fakeProcTable) {
			f.set(fakeHelperPID, func(p *fakeProc) { p.info.PPID = fakeShellPID })
		}, false},
		{"user starts ask in a shell that inherited the env", []string{"ask"}, "/tmp/s", nil, false},
		{"argv0 is not SUDO_ASKPASS", []string{"ask", "[sudo] password for root: "}, "/tmp/s", nil, false},
		{"no socket variable", []string{askpass, "[sudo] password for root: "}, "", nil, false},
		{"parent named sudo without root euid", []string{askpass, "p"}, "/tmp/s", func(f *fakeProcTable) {
			f.set(fakeSudoPID, func(p *fakeProc) { p.info.EUID = p.info.RUID })
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := askPassTree(t)
			if tc.parent != nil {
				tc.parent(f)
			}
			parentPID := f.procs[fakeHelperPID].info.PPID
			f.set(os.Getppid(), func(p *fakeProc) { *p = f.procs[parentPID] })
			t.Setenv(AskSudoSocketEnv, tc.socket)
			t.Setenv("SUDO_ASKPASS", askpass)
			prev := os.Args
			os.Args = tc.argv
			defer func() { os.Args = prev }()

			if got := IsAskPassInvocation(); got != tc.want {
				t.Errorf("IsAskPassInvocation() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsAskPassInvocation_FalseForThisTestProcessWithRealProcfs(t *testing.T) {
	t.Setenv(AskSudoSocketEnv, "/tmp/s")
	t.Setenv("SUDO_ASKPASS", "/usr/local/bin/ask")
	prev := os.Args
	os.Args = []string{"/usr/local/bin/ask", "[sudo] password for root: "}
	defer func() { os.Args = prev }()

	if IsAskPassInvocation() {
		t.Error("a process whose parent is not sudo was treated as sudo's askpass helper")
	}
}

func TestBashTool_RegistersShellForTheCommandsLifetime(t *testing.T) {
	askPassTree(t)
	CloseSudoIPCServer()
	t.Cleanup(CloseSudoIPCServer)
	h := newRecordingSudoHandler(testSudoPassword)
	env := NewToolEnv(t.TempDir(), 9, true, nil, h)

	release := make(chan struct{})
	started := make(chan []string, 1)
	prevRun := RunShell
	t.Cleanup(func() { RunShell = prevRun })
	RunShell = func(dir, command string, extraEnv ...string) (*ShellHandle, error) {
		out := make(chan string)
		done := make(chan ShellResult, 1)
		go func() {
			<-release
			close(out)
			done <- ShellResult{}
		}()
		started <- extraEnv
		return &ShellHandle{Pid: fakeShellPID, Output: out, Done: done, Kill: func() {}}, nil
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = RunToolWithJSON(engine.NewStandaloneAgentContext(context.Background()), BashTool(env), `{"command":"sudo -A true","description":"d"}`)
	}()
	extraEnv := <-started
	var socket string
	for _, kv := range extraEnv {
		if v, ok := strings.CutPrefix(kv, AskSudoSocketEnv+"="); ok {
			socket = v
		}
	}
	if socket == "" {
		t.Fatalf("%s not exported: %v", AskSudoSocketEnv, extraEnv)
	}

	out, err := runAskPassHelperCapturing(t, "p", socket)
	if err != nil || out != testSudoPassword+"\n" {
		t.Fatalf("sudo under the running command got (%q, %v), want the password", out, err)
	}
	if calls := h.asked(); len(calls) != 1 || calls[0].tabID != 9 {
		t.Errorf("modal requests = %+v, want one for the env's tab 9", calls)
	}

	close(release)
	<-finished
	shortSudoWait(t)
	if _, err := runAskPassHelperCapturing(t, "p", socket); err == nil {
		t.Error("sudo descended from a finished command's shell was still answered")
	}
}

func TestBashTool_SudoEnvCarriesNoSecrets(t *testing.T) {
	env, _ := newTestToolEnv(t)
	prevRun := RunShell
	t.Cleanup(func() { RunShell = prevRun })
	var exported []string
	RunShell = func(dir, command string, extraEnv ...string) (*ShellHandle, error) {
		exported = append([]string(nil), extraEnv...)
		out := make(chan string)
		close(out)
		done := make(chan ShellResult, 1)
		done <- ShellResult{}
		return &ShellHandle{Output: out, Done: done, Kill: func() {}}, nil
	}

	runTool(t, BashTool(env), BashParams{Command: "env"})

	if !sudoAskPassSupported {
		if len(exported) != 0 {
			t.Errorf("exported %v on a platform without the askpass flow", exported)
		}
		return
	}
	keys := map[string]string{}
	for _, kv := range exported {
		k, v, _ := strings.Cut(kv, "=")
		keys[k] = v
	}
	if len(keys) != 2 || keys["SUDO_ASKPASS"] == "" || keys[AskSudoSocketEnv] == "" {
		t.Fatalf("exported %v, want exactly SUDO_ASKPASS and %s", exported, AskSudoSocketEnv)
	}
	askpass, err := os.Stat(keys["SUDO_ASKPASS"])
	if err != nil {
		t.Fatalf("SUDO_ASKPASS %q: %v", keys["SUDO_ASKPASS"], err)
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(askpass, self) {
		t.Errorf("SUDO_ASKPASS %q is not the running executable", keys["SUDO_ASKPASS"])
	}
}

func TestSudoIPCServer_SocketLivesInAPrivateDirectory(t *testing.T) {
	srv := newTestSudoServer(t)
	fi, err := os.Stat(filepath.Dir(srv.SocketPath))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("socket directory mode = %v, want 0700", fi.Mode().Perm())
	}
	srv.Close()
	if _, err := os.Stat(filepath.Dir(srv.SocketPath)); !os.IsNotExist(err) {
		t.Errorf("socket directory survived Close: %v", err)
	}
}
