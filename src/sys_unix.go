//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const exeName = "agent-bus"

func stateDir() string {
	if dir := os.Getenv("AGENT_BUS_DIR"); dir != "" {
		return dir
	}
	return fmt.Sprintf("/tmp/agent-bus-%d", os.Getuid())
}

// checkPrivate refuses a state directory that other users could read or plant files in.
func checkPrivate(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be owned by you with mode 0700", dir)
	}
	return nil
}

func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

// wakePath is the socket of session id. Socket paths are limited to about 100
// bytes, so a long state directory moves the socket to a hashed name in the
// temporary directory.
func wakePath(id string) string {
	path := filepath.Join(root, "wake", id+".sock")
	if len(path) <= 100 {
		return path
	}
	sum := sha256.Sum256([]byte(root + "\x00" + id))
	return filepath.Join(os.TempDir(), fmt.Sprintf("agent-bus-%d-%s.sock", os.Getuid(), hex.EncodeToString(sum[:8])))
}

// listenWake opens the Unix socket that senders connect to after delivering to id.
func listenWake(id string) (<-chan struct{}, func(), error) {
	path := wakePath(id)
	os.Mkdir(filepath.Dir(path), 0o700)
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, func() {}, err
	}
	wake := make(chan struct{}, 1)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	return wake, func() { l.Close() }, nil
}

// poke wakes a session blocked in `agent-bus wait`, if there is one.
func poke(id string) {
	if c, err := net.DialTimeout("unix", wakePath(id), 200*time.Millisecond); err == nil {
		c.Close()
	}
}
