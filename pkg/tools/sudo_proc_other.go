//go:build !linux

package tools

import (
	"net"
	"os"
)

const sudoAskPassSupported = false

type procfsInspector struct{}

func (procfsInspector) PeerPID(*net.UnixConn) (int, int, error) {
	return 0, 0, errSudoAskPassUnsupported
}

func (procfsInspector) Process(int) (ProcInfo, error) {
	return ProcInfo{}, errSudoAskPassUnsupported
}

func (procfsInspector) SameExecutableAsSelf(int) (bool, error) {
	return false, errSudoAskPassUnsupported
}

func (procfsInspector) StdoutPipe(int) (PipeID, error) {
	return PipeID{}, errSudoAskPassUnsupported
}

func (procfsInspector) OwnPipes() (map[PipeID]bool, error) {
	return nil, errSudoAskPassUnsupported
}

func askPassExecutable() string {
	exe, _ := os.Executable()
	return exe
}
