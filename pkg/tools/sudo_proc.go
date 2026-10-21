package tools

import (
	"errors"
	"net"
)

var errSudoAskPassUnsupported = errors.New("ask's sudo password prompt is only supported on Linux")

// ProcInfo is what the askpass checks trust about a local process: facts
// the kernel reports and an unprivileged process cannot forge (an
// effective uid of 0 in particular).
type ProcInfo struct {
	PID  int
	PPID int
	Name string
	RUID int
	EUID int
}

// PipeID identifies a pipe independently of which end, or which process,
// holds it.
type PipeID struct {
	Dev uint64
	Ino uint64
}

// ProcInspector reads the process facts the sudo IPC server and the
// askpass entry point decide on.
type ProcInspector interface {
	// PeerPID returns the pid and uid of the process on the other end of
	// conn.
	PeerPID(conn *net.UnixConn) (pid, uid int, err error)
	Process(pid int) (ProcInfo, error)
	// SameExecutableAsSelf reports whether pid runs the very executable
	// image this process runs.
	SameExecutableAsSelf(pid int) (bool, error)
	// StdoutPipe returns the pipe on pid's file descriptor 1, or an error
	// when that descriptor is not a pipe.
	StdoutPipe(pid int) (PipeID, error)
	// OwnPipes returns every pipe this process holds open.
	OwnPipes() (map[PipeID]bool, error)
}

// SudoProcInspector is the ProcInspector the askpass checks use; tests swap
// it for a fake process table.
var SudoProcInspector ProcInspector = procfsInspector{}

// isSudoFrontEnd reports whether p is a sudo front end started by uid:
// real uid uid and effective uid 0, which for an unprivileged uid only the
// setuid sudo binary produces.
func isSudoFrontEnd(p ProcInfo, uid int) bool {
	return p.Name == "sudo" && p.EUID == 0 && p.RUID == uid && uid != 0
}
