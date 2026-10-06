//go:build linux

package bus

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseCPUList(t *testing.T) {
	for _, list := range []string{"", "1,", "1-0", "a", "1--2", "1-2000000", strconv.FormatInt(int64(^uint(0)>>1), 10)} {
		if _, err := parseCPUList(list); err == nil {
			t.Fatalf("accepted %q", list)
		}
	}
	got, err := parseCPUList("2,4-6")
	if err != nil || len(got) != 4 || !got[2] || !got[4] || !got[5] || !got[6] {
		t.Fatalf("parse CPU list: %v, %v", got, err)
	}
}

func TestPlainCommandSignalExit(t *testing.T) {
	code, err := executeResourceCommand(context.Background(), []string{"/bin/sh", "-c", "kill -TERM $$"}, runLimits{}, "", nil)
	if err != nil || code != 143 {
		t.Fatalf("signal exit=%d err=%v", code, err)
	}
}

func TestResourceBackendMissingDoesNotStartCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("PATH", t.TempDir())
	code, err := executeResourceCommand(context.Background(), []string{"/bin/sh", "-c", "touch \"$1\"", "sh", marker}, runLimits{MaxRAM: 64 << 20}, "", nil)
	if err == nil || code != 0 {
		t.Fatalf("missing backend exit=%d err=%v", code, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran without resource backend: %v", err)
	}
}

func TestResourceScopeExitAndCgroup(t *testing.T) {
	requireUserScope(t, runLimits{TrackMemory: true})
	unit := "agent-bus-test-exit-" + strconv.Itoa(os.Getpid()) + ".scope"
	var observed string
	code, err := executeResourceCommand(context.Background(), []string{"/bin/sh", "-c", "sleep .2; exit 23"}, runLimits{TrackMemory: true}, unit, func(path string) error {
		observed = path
		if !strings.HasPrefix(path, cgroupRoot+"/") {
			t.Errorf("unexpected cgroup path %q", path)
		}
		_, err := os.Stat(filepath.Join(path, "cgroup.procs"))
		return err
	})
	if err != nil || code != 23 || observed == "" {
		t.Fatalf("exit=%d err=%v cgroup=%q", code, err, observed)
	}
}

func TestResourceScopeMemorySettings(t *testing.T) {
	const maxRAM = 64 << 20
	requireUserScope(t, runLimits{MaxRAM: maxRAM})
	unit := "agent-bus-test-memory-" + strconv.Itoa(os.Getpid()) + ".scope"
	code, err := executeResourceCommand(context.Background(), []string{"/bin/sh", "-c", "sleep .2"}, runLimits{MaxRAM: maxRAM}, unit, func(path string) error {
		for name, want := range map[string]string{
			"memory.max":       strconv.Itoa(maxRAM),
			"memory.swap.max":  "0",
			"memory.oom.group": "1",
		} {
			data, err := os.ReadFile(filepath.Join(path, name))
			if err != nil {
				return err
			}
			if got := strings.TrimSpace(string(data)); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		return nil
	})
	if err != nil || code != 0 {
		t.Fatalf("memory scope exit=%d err=%v", code, err)
	}
}

func TestResourceScopeAggregateCPUTime(t *testing.T) {
	requireUserScope(t, runLimits{MaxCPUTime: 150 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unit := "agent-bus-test-cpu-" + strconv.Itoa(os.Getpid()) + ".scope"
	start := time.Now()
	code, err := executeResourceCommand(ctx, []string{"/bin/sh", "-c", "while :; do :; done"}, runLimits{MaxCPUTime: 150 * time.Millisecond}, unit, nil)
	if err != nil || code != 124 {
		t.Fatalf("CPU budget exit=%d err=%v after %v", code, err, time.Since(start))
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("CPU budget stopped too slowly: %v", time.Since(start))
	}
}

func TestResourceScopeCancellationKillsDescendant(t *testing.T) {
	requireUserScope(t, runLimits{TrackMemory: true})
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	unit := "agent-bus-test-cancel-" + strconv.Itoa(os.Getpid()) + ".scope"
	go func() {
		_, err := executeResourceCommand(ctx, []string{"/bin/sh", "-c", "sleep 30 & echo $! > \"$1\"; wait", "sh", pidFile}, runLimits{TrackMemory: true}, unit, nil)
		result <- err
	}()
	var pid int
	deadline := time.After(3 * time.Second)
	for pid == 0 {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if pid > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("child did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("scope did not stop")
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
		t.Fatalf("descendant %d survived cancellation", pid)
	}
}

func requireUserScope(t *testing.T, limits runLimits) {
	t.Helper()
	if err := validateRunPlatform(limits); err != nil {
		t.Skipf("user scope unavailable: %v", err)
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		t.Skipf("user systemd unavailable: %v", err)
	}
}
