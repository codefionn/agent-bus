package bus

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/sys/unix"
)

func kinfo(pid int) *unix.KinfoProc {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return nil
	}
	return k
}

func startOf(k *unix.KinfoProc) uint64 {
	return uint64(k.Proc.P_starttime.Sec)*1_000_000 + uint64(k.Proc.P_starttime.Usec)
}

// procStart returns the start time of pid in microseconds since the epoch, or 0 when it is gone.
func procStart(pid int) uint64 {
	if k := kinfo(pid); k != nil {
		return startOf(k)
	}
	return 0
}

func procInfo(pid int) (*proc, bool) {
	k := kinfo(pid)
	if k == nil {
		return nil, false
	}
	comm := k.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return &proc{pid: pid, ppid: int(k.Eproc.Ppid), start: startOf(k), name: string(comm), args: procArgs(pid)}, true
}

// procArgs reads argv through kern.procargs2: argc, the executable path, NUL
// padding, then argc NUL-terminated arguments followed by the environment.
func procArgs(pid int) []string {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(buf) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(buf))
	buf = buf[4:]
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[i:]
	}
	buf = bytes.TrimLeft(buf, "\x00")
	var args []string
	for len(args) < argc && len(buf) > 0 {
		i := bytes.IndexByte(buf, 0)
		if i < 0 {
			i = len(buf)
		}
		args = append(args, string(buf[:i]))
		buf = buf[min(i+1, len(buf)):]
	}
	return args
}
