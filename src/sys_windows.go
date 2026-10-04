package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const exeName = "agent-bus.exe"

func stateDir() string {
	if dir := os.Getenv("AGENT_BUS_DIR"); dir != "" {
		return dir
	}
	if dir, err := os.UserCacheDir(); err == nil { // %LOCALAPPDATA%, private to the user
		return filepath.Join(dir, "agent-bus")
	}
	return filepath.Join(os.TempDir(), "agent-bus")
}

// checkPrivate trusts the per-user ACL that %LOCALAPPDATA% inherits.
func checkPrivate(dir string) error { return nil }

func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

// pipeName is the named pipe of session id. Pipe names are global to the
// machine, so the name carries a hash of the state directory and the user.
func pipeName(id string) *uint16 {
	sum := sha256.Sum256([]byte(os.Getenv("USERNAME") + "\x00" + root))
	name, _ := windows.UTF16PtrFromString(`\\.\pipe\agent-bus-` + hex.EncodeToString(sum[:8]) + "-" + id)
	return name
}

func newPipe(name *uint16) (windows.Handle, error) {
	return windows.CreateNamedPipe(name, windows.PIPE_ACCESS_INBOUND,
		windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES, 0, 0, 0, nil)
}

// listenWake opens the named pipe that senders connect to after delivering to id.
func listenWake(id string) (<-chan struct{}, func(), error) {
	name := pipeName(id)
	h, err := newPipe(name)
	if err != nil {
		return nil, func() {}, err
	}
	wake := make(chan struct{}, 1)
	go func() {
		for {
			if err := windows.ConnectNamedPipe(h, nil); err != nil && err != windows.ERROR_PIPE_CONNECTED {
				windows.CloseHandle(h)
				return
			}
			// Open the next instance before dropping this one so no poke finds the pipe missing.
			next, err := newPipe(name)
			windows.DisconnectNamedPipe(h)
			windows.CloseHandle(h)
			select {
			case wake <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
			h = next
		}
	}()
	// The process exits right after; that closes the pipe.
	return wake, func() {}, nil
}

// poke wakes a session blocked in `agent-bus wait`, if there is one.
func poke(id string) {
	h, err := windows.CreateFile(pipeName(id), windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err == nil {
		windows.CloseHandle(h)
	}
}
