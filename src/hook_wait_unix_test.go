//go:build !windows

package bus

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestHookSessionEndWakesWaiter(t *testing.T) {
	for _, reason := range []string{"other", "prompt_input_exit"} {
		t.Run(reason, func(t *testing.T) {
			e := setupHookTest(t, deliverHooks)
			os.RemoveAll(filepath.Join(inboxDir, e.ID))
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestHookProcess$")
			cmd.Args[0] = "claude"
			cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "BUS_HOOK_TEST_PROCESS=harness", "BUS_HOOK_TEST_COMMAND=wait", "AGENT_BUS_DIR="+root)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			deadline := time.Now().Add(2 * time.Second)
			for !fileExists(wakePath(e.ID)) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if !fileExists(wakePath(e.ID)) {
				t.Fatal("waiter failed to listen")
			}
			// Owner remains alive throughout SessionEnd, reproducing shutdown
			// before the harness process exits.
			start := time.Now()
			runHookTest(t, map[string]string{"hook_event_name": "SessionEnd", "session_id": e.ID, "reason": reason})
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != exitUnsubscribed {
					t.Fatalf("wait exit = %v, want 3", err)
				}
				if time.Since(start) > time.Second {
					t.Error("waiter exit took over one second")
				}
			case <-time.After(time.Second):
				t.Fatal("waiter stayed blocked after SessionEnd")
			}
		})
	}
}
