package bus

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// statFields returns the fields of /proc/PID/stat after the command name.
func statFields(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	return strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
}

// procStart returns the start time of pid in clock ticks since boot, or 0 when it is gone.
func procStart(pid int) uint64 {
	f := statFields(pid)
	if len(f) < 20 {
		return 0
	}
	start, _ := strconv.ParseUint(f[19], 10, 64)
	return start
}

func procInfo(pid int) (*proc, bool) {
	f := statFields(pid)
	if len(f) < 20 {
		return nil, false
	}
	p := &proc{pid: pid}
	p.ppid, _ = strconv.Atoi(f[1])
	p.start, _ = strconv.ParseUint(f[19], 10, 64)
	comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	p.name = strings.TrimSpace(string(comm))
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		p.args = strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	}
	return p, true
}
