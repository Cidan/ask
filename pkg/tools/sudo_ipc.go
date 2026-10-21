package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/engine"
)

// AskSudoSocketEnv names the socket the askpass helper dials. Besides
// SUDO_ASKPASS it is the only variable the bash tool exports for sudo, and
// it is not a secret: the server authorizes a helper by the process tree
// it sits in, never by anything the helper presents.
const AskSudoSocketEnv = "ASK_SUDO_SOCKET"

// sudoShellWait bounds how long a request waits for the bash-tool shell it
// descends from to be registered. RegisterShell runs right after the shell
// starts, so this only covers that gap.
var sudoShellWait = 2 * time.Second

const sudoAncestryLimit = 64

type sudoAskPassRequest struct {
	Prompt string `json:"prompt"`
}

type sudoAskPassResponse struct {
	Password string `json:"password,omitempty"`
	Error    string `json:"error,omitempty"`
}

// SudoIPCServer answers askpass requests from sudo processes that bash tool
// commands start, routing each to the interaction handler and tab of the
// shell it descends from.
type SudoIPCServer struct {
	SocketPath string
	dir        string
	listener   net.Listener

	mu      sync.Mutex
	closed  bool
	shells  map[int]*sudoShell
	changed chan struct{}
}

type sudoShell struct {
	interaction engine.InteractionHandler
	tabID       int
	// answered holds the sudo pids this shell's prompts already fed a
	// password; sudo asks again only after rejecting it.
	answered map[int]bool
}

var (
	globalSudoServer   *SudoIPCServer
	sudoServerInitLock sync.Mutex
)

// EnsureSudoIPCServer returns the process-wide server, starting it on first
// use. It returns nil where the askpass flow is unsupported or the socket
// cannot be created; the bash tool then exports nothing and sudo -A fails
// with sudo's own error.
func EnsureSudoIPCServer() *SudoIPCServer {
	sudoServerInitLock.Lock()
	defer sudoServerInitLock.Unlock()
	if globalSudoServer != nil && !globalSudoServer.isClosed() {
		return globalSudoServer
	}
	if !sudoAskPassSupported {
		return nil
	}
	s, err := newSudoIPCServer()
	if err != nil {
		return nil
	}
	globalSudoServer = s
	return s
}

// CloseSudoIPCServer stops the process-wide server and removes its socket.
func CloseSudoIPCServer() {
	sudoServerInitLock.Lock()
	defer sudoServerInitLock.Unlock()
	if globalSudoServer != nil {
		globalSudoServer.Close()
		globalSudoServer = nil
	}
}

func newSudoIPCServer() (*SudoIPCServer, error) {
	dir, err := os.MkdirTemp("", "ask-sudo-")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "askpass.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s := &SudoIPCServer{
		SocketPath: path,
		dir:        dir,
		listener:   l,
		shells:     make(map[int]*sudoShell),
		changed:    make(chan struct{}),
	}
	go s.listen()
	return s, nil
}

func (s *SudoIPCServer) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *SudoIPCServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.listener.Close()
	_ = os.RemoveAll(s.dir)
}

// RegisterShell routes askpass requests from sudo processes descended from
// the shell pid to interaction on tabID until the returned func is called.
func (s *SudoIPCServer) RegisterShell(pid int, interaction engine.InteractionHandler, tabID int) func() {
	shell := &sudoShell{interaction: interaction, tabID: tabID, answered: make(map[int]bool)}
	s.mu.Lock()
	s.shells[pid] = shell
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.shells[pid] == shell {
			delete(s.shells, pid)
		}
		s.mu.Unlock()
	}
}

func (s *SudoIPCServer) listen() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.isClosed() {
				return
			}
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *SudoIPCServer) handleConn(conn net.Conn) {
	defer conn.Close()
	reply := func(resp sudoAskPassResponse) {
		_ = json.NewEncoder(conn).Encode(resp)
	}
	refuse := func(reason string) {
		reply(sudoAskPassResponse{Error: "refused: " + reason})
	}

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		refuse("the connection is not a unix socket")
		return
	}
	pid, uid, err := SudoProcInspector.PeerPID(uc)
	if err != nil {
		refuse("cannot identify the connecting process: " + err.Error())
		return
	}
	if uid != os.Getuid() {
		refuse(fmt.Sprintf("the connecting process runs as uid %d, not %d", uid, os.Getuid()))
		return
	}

	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		refuse("no request")
		return
	}
	var req sudoAskPassRequest
	if err := json.Unmarshal(line, &req); err != nil {
		refuse("malformed request")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := r.ReadByte(); err != nil {
				break
			}
		}
		cancel()
	}()

	sudoPID, err := verifyAskPassPeer(pid, uid)
	if err != nil {
		refuse(err.Error())
		return
	}
	shell, err := s.awaitShell(ctx, sudoPID)
	if err != nil {
		refuse(err.Error())
		return
	}

	s.mu.Lock()
	retry := shell.answered[sudoPID]
	s.mu.Unlock()
	resp, err := promptSudoPassword(ctx, shell, engine.SudoPasswordRequest{Prompt: req.Prompt, Retry: retry})
	if err != nil || resp.Cancelled || resp.Password == "" {
		reply(sudoAskPassResponse{Error: "cancelled"})
		return
	}
	s.mu.Lock()
	shell.answered[sudoPID] = true
	s.mu.Unlock()
	reply(sudoAskPassResponse{Password: resp.Password})
}

// verifyAskPassPeer checks that pid is this ask binary running as sudo's
// askpass child, with its stdout on a pipe that does not lead back into
// this process, and returns the sudo front end's pid.
func verifyAskPassPeer(pid, uid int) (int, error) {
	same, err := SudoProcInspector.SameExecutableAsSelf(pid)
	if err != nil || !same {
		return 0, errors.New("the connecting process is not this ask binary")
	}
	helper, err := SudoProcInspector.Process(pid)
	if err != nil {
		return 0, fmt.Errorf("cannot inspect the connecting process: %w", err)
	}
	parent, err := SudoProcInspector.Process(helper.PPID)
	if err != nil || !isSudoFrontEnd(parent, uid) {
		return 0, errors.New("the askpass helper was not started by sudo")
	}
	out, err := SudoProcInspector.StdoutPipe(pid)
	if err != nil {
		return 0, errors.New("the askpass helper's stdout is not a pipe to sudo")
	}
	own, err := SudoProcInspector.OwnPipes()
	if err != nil {
		return 0, fmt.Errorf("cannot list ask's own pipes: %w", err)
	}
	if own[out] {
		return 0, errors.New("the askpass helper's stdout leads back into ask")
	}
	return parent.PID, nil
}

func (s *SudoIPCServer) awaitShell(ctx context.Context, sudoPID int) (*sudoShell, error) {
	timer := time.NewTimer(sudoShellWait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		changed := s.changed
		s.mu.Unlock()
		if shell := s.registeredAncestor(sudoPID); shell != nil {
			return shell, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil, errors.New("sudo was not started by an ask bash tool command")
		case <-ctx.Done():
			return nil, errors.New("the askpass helper hung up")
		}
	}
}

func (s *SudoIPCServer) registeredAncestor(sudoPID int) *sudoShell {
	sudo, err := SudoProcInspector.Process(sudoPID)
	if err != nil {
		return nil
	}
	pid := sudo.PPID
	for i := 0; i < sudoAncestryLimit && pid > 1; i++ {
		s.mu.Lock()
		shell := s.shells[pid]
		s.mu.Unlock()
		if shell != nil {
			return shell
		}
		info, err := SudoProcInspector.Process(pid)
		if err != nil {
			return nil
		}
		pid = info.PPID
	}
	return nil
}

func promptSudoPassword(ctx context.Context, shell *sudoShell, req engine.SudoPasswordRequest) (engine.SudoPasswordResponse, error) {
	if shell.interaction == nil {
		return engine.SudoPasswordResponse{Cancelled: true}, nil
	}
	if p, ok := shell.interaction.(engine.SudoPasswordPrompter); ok {
		return p.PromptSudoPassword(ctx, shell.tabID, req)
	}
	return shell.interaction.RequestSudoPassword(ctx, shell.tabID, req.Prompt)
}

// IsAskPassInvocation reports whether sudo exec'd this process as the
// askpass helper of an ask bash-tool command: the socket variable is set,
// argv is exactly SUDO_ASKPASS plus sudo's prompt, and the parent is a sudo
// front end run by this user. Any other start of the binary with that
// environment (the agent running ask, a shell that inherited the
// variables) is a normal ask launch.
func IsAskPassInvocation() bool {
	if os.Getenv(AskSudoSocketEnv) == "" || len(os.Args) != 2 || os.Args[0] != os.Getenv("SUDO_ASKPASS") {
		return false
	}
	parent, err := SudoProcInspector.Process(os.Getppid())
	return err == nil && isSudoFrontEnd(parent, os.Getuid())
}

// RunAskPassHelper asks the server for sudo's password and writes it to
// stdout, which sudo reads through its askpass pipe.
func RunAskPassHelper() error {
	socket := os.Getenv(AskSudoSocketEnv)
	if socket == "" {
		return fmt.Errorf("%s is not set", AskSudoSocketEnv)
	}
	var prompt string
	if len(os.Args) > 1 {
		prompt = os.Args[1]
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("dial sudo socket: %w", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(sudoAskPassRequest{Prompt: prompt}); err != nil {
		return fmt.Errorf("write sudo request: %w", err)
	}
	var resp sudoAskPassResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("read sudo response: %w", err)
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if resp.Password == "" {
		return errors.New("empty response")
	}
	_, err = io.WriteString(os.Stdout, resp.Password+"\n")
	return err
}
