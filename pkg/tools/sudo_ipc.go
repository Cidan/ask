package tools

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/Cidan/ask/pkg/engine"
)

type SudoIPCServer struct {
	mu          sync.Mutex
	listener    net.Listener
	SocketPath  string
	Token       string
	closed      bool
	Interaction engine.InteractionHandler
}

var (
	globalSudoServer   *SudoIPCServer
	sudoServerInitLock sync.Mutex
)

func generateRandomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("ask-token-%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}

// EnsureSudoIPCServer creates or returns the singleton SudoIPCServer.
func EnsureSudoIPCServer(interaction engine.InteractionHandler) *SudoIPCServer {
	sudoServerInitLock.Lock()
	defer sudoServerInitLock.Unlock()

	if globalSudoServer != nil && !globalSudoServer.closed {
		if interaction != nil {
			globalSudoServer.Interaction = interaction
		}
		return globalSudoServer
	}

	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("ask-sudo-%d.sock", os.Getpid()))
	_ = os.Remove(socketPath)

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil
	}
	_ = os.Chmod(socketPath, 0600)

	server := &SudoIPCServer{
		listener:    l,
		SocketPath:  socketPath,
		Token:       generateRandomToken(),
		Interaction: interaction,
	}

	globalSudoServer = server
	go server.listen()

	return server
}

func (s *SudoIPCServer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.SocketPath)
}

func (s *SudoIPCServer) listen() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		go s.handleConn(conn)
	}
}

func verifyPeerUID(fd int) error {
	if runtime.GOOS == "linux" {
		ucred, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err == nil {
			if ucred.Uid != uint32(os.Getuid()) {
				return fmt.Errorf("peer UID %d does not match process UID %d", ucred.Uid, os.Getuid())
			}
		}
	}
	return nil
}

func (s *SudoIPCServer) checkPeerCreds(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return nil
	}
	var credErr error
	_ = raw.Control(func(fd uintptr) {
		credErr = verifyPeerUID(int(fd))
	})
	return credErr
}

func (s *SudoIPCServer) handleConn(conn net.Conn) {
	defer conn.Close()

	if err := s.checkPeerCreds(conn); err != nil {
		_, _ = conn.Write([]byte("ERROR: peer credential verification failed\n"))
		return
	}

	reader := bufio.NewReader(conn)
	headers := make(map[string]string)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			headers[parts[0]] = parts[1]
		}
	}

	token := headers["TOKEN"]
	if token == "" || token != s.Token {
		_, _ = conn.Write([]byte("ERROR: invalid token\n"))
		return
	}

	tabID := 0
	if tidStr, ok := headers["TABID"]; ok {
		if tid, err := strconv.Atoi(tidStr); err == nil {
			tabID = tid
		}
	}

	prompt := headers["PROMPT"]
	if prompt == "" {
		prompt = "[sudo] password required"
	}

	handler := s.Interaction
	if handler == nil {
		_, _ = conn.Write([]byte("ERROR: cancelled\n"))
		return
	}

	resp, err := handler.RequestSudoPassword(context.Background(), tabID, prompt)
	if err != nil || resp.Cancelled || resp.Password == "" {
		_, _ = conn.Write([]byte("ERROR: cancelled\n"))
		return
	}

	_, _ = conn.Write([]byte("PASSWORD:" + resp.Password + "\n"))
}

// RunAskPassHelper connects to the IPC server and reads the password.
func RunAskPassHelper() error {
	socketPath := os.Getenv("ASK_SUDO_SOCKET")
	token := os.Getenv("ASK_SUDO_TOKEN")
	if socketPath == "" || token == "" {
		return fmt.Errorf("missing ASK_SUDO_SOCKET or ASK_SUDO_TOKEN")
	}

	tabID := os.Getenv("ASK_SUDO_TABID")

	prompt := "Password:"
	if len(os.Args) > 1 && os.Args[1] != "" {
		prompt = os.Args[1]
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("dial sudo socket: %w", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("TOKEN:%s\nTABID:%s\nPROMPT:%s\n\n", token, tabID, prompt)
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("write sudo request: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read sudo response: %w", err)
	}

	resp = strings.TrimSpace(resp)
	if strings.HasPrefix(resp, "PASSWORD:") {
		pwd := strings.TrimPrefix(resp, "PASSWORD:")
		fmt.Println(pwd)
		return nil
	}

	return fmt.Errorf("sudo auth failed: %s", resp)
}
