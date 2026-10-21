//go:build linux

package tools

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const sudoAskPassSupported = true

type procfsInspector struct{}

func (procfsInspector) PeerPID(conn *net.UnixConn) (int, int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if credErr != nil {
		return 0, 0, credErr
	}
	return int(cred.Pid), int(cred.Uid), nil
}

func (procfsInspector) Process(pid int) (ProcInfo, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return ProcInfo{}, err
	}
	defer f.Close()
	info := ProcInfo{PID: pid, PPID: -1, RUID: -1, EUID: -1}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(val)
		switch key {
		case "Name":
			info.Name = strings.TrimSpace(val)
		case "PPid":
			if len(fields) > 0 {
				info.PPID, _ = strconv.Atoi(fields[0])
			}
		case "Uid":
			if len(fields) >= 2 {
				info.RUID, _ = strconv.Atoi(fields[0])
				info.EUID, _ = strconv.Atoi(fields[1])
			}
		}
	}
	if err := sc.Err(); err != nil {
		return ProcInfo{}, err
	}
	if info.PPID < 0 || info.RUID < 0 || info.EUID < 0 {
		return ProcInfo{}, fmt.Errorf("/proc/%d/status: missing PPid or Uid", pid)
	}
	return info, nil
}

func (procfsInspector) SameExecutableAsSelf(pid int) (bool, error) {
	peer, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false, err
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return false, err
	}
	return os.SameFile(peer, self), nil
}

func (procfsInspector) StdoutPipe(pid int) (PipeID, error) {
	id, isPipe, err := pipeAt(fmt.Sprintf("/proc/%d/fd/1", pid))
	if err != nil {
		return PipeID{}, err
	}
	if !isPipe {
		return PipeID{}, fmt.Errorf("stdout of pid %d is not a pipe", pid)
	}
	return id, nil
}

func (procfsInspector) OwnPipes() (map[PipeID]bool, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	pipes := make(map[PipeID]bool)
	for _, e := range entries {
		id, isPipe, err := pipeAt(filepath.Join("/proc/self/fd", e.Name()))
		if err == nil && isPipe {
			pipes[id] = true
		}
	}
	return pipes, nil
}

func pipeAt(fdPath string) (PipeID, bool, error) {
	fi, err := os.Stat(fdPath)
	if err != nil {
		return PipeID{}, false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return PipeID{}, false, fmt.Errorf("%s: no stat data", fdPath)
	}
	return PipeID{Dev: uint64(st.Dev), Ino: st.Ino}, fi.Mode()&os.ModeNamedPipe != 0, nil
}

// askPassExecutable is the SUDO_ASKPASS path: the installed binary while it
// is still the image this process runs, else /proc/<pid>/exe, which keeps
// resolving to the running image after a reinstall replaces the file.
func askPassExecutable() string {
	running := fmt.Sprintf("/proc/%d/exe", os.Getpid())
	exe, err := os.Executable()
	if err != nil {
		return running
	}
	onDisk, err := os.Stat(exe)
	if err != nil {
		return running
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil || !os.SameFile(onDisk, self) {
		return running
	}
	return exe
}
