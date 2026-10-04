package bus

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const stillActive = 259

var processes map[uint32]windows.ProcessEntry32

// snapshot lists every process once per run: the Toolhelp snapshot is the only
// place Windows keeps parent pids and executable names together.
func snapshot() map[uint32]windows.ProcessEntry32 {
	if processes != nil {
		return processes
	}
	processes = map[uint32]windows.ProcessEntry32{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return processes
	}
	defer windows.CloseHandle(snap)
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		processes[e.ProcessID] = e
	}
	return processes
}

func openProcess(pid int) (windows.Handle, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil || code != stillActive {
		windows.CloseHandle(h)
		return 0, false
	}
	return h, true
}

// procStart returns the creation time of pid as a FILETIME, or 0 when it is gone.
func procStart(pid int) uint64 {
	h, ok := openProcess(pid)
	if !ok {
		return 0
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
}

func procInfo(pid int) (*proc, bool) {
	e, ok := snapshot()[uint32(pid)]
	if !ok {
		return nil, false
	}
	return &proc{
		pid:   pid,
		ppid:  int(e.ParentProcessID),
		start: procStart(pid),
		name:  windows.UTF16ToString(e.ExeFile[:]),
		args:  commandLine(pid),
	}, true
}

// commandLine asks the kernel for the command line of pid (Windows 8.1 and later).
func commandLine(pid int) []string {
	h, ok := openProcess(pid)
	if !ok {
		return nil
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 4096)
	for {
		var n uint32
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &n)
		if err == windows.STATUS_INFO_LENGTH_MISMATCH && int(n) > len(buf) {
			buf = make([]byte, n)
			continue
		}
		if err != nil {
			return nil
		}
		break
	}
	args, err := windows.DecomposeCommandLine((*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String())
	if err != nil {
		return nil
	}
	return args
}
