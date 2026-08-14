package main

import (
	"github.com/Cidan/ask/pkg/tools"
)

type sudoIPCServer struct {
	inner      *tools.SudoIPCServer
	socketPath string
	token      string
}

func (s *sudoIPCServer) close() {
	if s.inner != nil {
		s.inner.Close()
	}
}

func ensureSudoIPCServer() *sudoIPCServer {
	srv := tools.EnsureSudoIPCServer(globalTUIInteractionHandler)
	if srv == nil {
		return nil
	}
	return &sudoIPCServer{
		inner:      srv,
		socketPath: srv.SocketPath,
		token:      srv.Token,
	}
}

func runAskPassHelper() error {
	return tools.RunAskPassHelper()
}
