package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func rewakeCommand(t *testing.T, payload map[string]string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHookProcess$")
	cmd.Args[0] = "claude"
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "BUS_HOOK_TEST_PROCESS=harness", "BUS_HOOK_TEST_COMMAND=rewake", "AGENT_BUS_DIR="+root)
	cmd.Stdin = bytes.NewReader(data)
	output := new(bytes.Buffer)
	cmd.Stdout, cmd.Stderr = output, output
	return cmd, output
}

func startRewake(t *testing.T, e *entry) (*exec.Cmd, *bytes.Buffer, *rewakeClaim) {
	t.Helper()
	cmd, output := rewakeCommand(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if claim, ok := load[rewakeClaim](rewakePath(e.ID)); ok && claim.Registered == e.Registered {
			poke(claim.Endpoint)
		}
		cmd.Process.Kill()
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if claim, ok := load[rewakeClaim](rewakePath(e.ID)); ok && claim.Registered == e.Registered {
			if process, ok := procInfo(claim.PID); ok && process.ppid == cmd.Process.Pid {
				return cmd, output, claim
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("rewake failed to claim watcher")
	return nil, nil, nil
}

func finishRewake(t *testing.T, cmd *exec.Cmd, output *bytes.Buffer, code int) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		actual := 0
		if exit, ok := err.(*exec.ExitError); ok {
			actual = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if actual != code {
			t.Fatalf("rewake exit %d, want %d; output %s", actual, code, output)
		}
	case <-time.After(time.Second):
		t.Fatal("rewake did not exit promptly")
	}
}

func TestHookRewakeIdleDeliveryAndDuplicates(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	os.RemoveAll(filepath.Join(inboxDir, e.ID))
	cmd, output, claim := startRewake(t, e)
	duplicate, duplicateOutput := rewakeCommand(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID})
	if err := duplicate.Start(); err != nil {
		t.Fatal(err)
	}
	finishRewake(t, duplicate, duplicateOutput, 0)
	current, _ := load[rewakeClaim](rewakePath(e.ID))
	if current == nil || !sameRewake(current, claim) {
		t.Fatal("duplicate replaced live watcher")
	}
	locked(func() {
		deliver(&entry{ID: "fixture-peer", Name: "fixture-peer"}, []*entry{e}, nil, "idle fixture message", "")
	})
	finishRewake(t, cmd, output, 2)
	if len(inboxFiles(e.ID)) != 1 {
		t.Fatal("idle rewake consumed queued message")
	}
	if fileExists(rewakePath(e.ID)) || fileExists(filepath.Join(watchDir, claim.Endpoint)) {
		t.Fatal("rewake claim or watch left after notification")
	}
}

func TestHookRewakeSessionEnd(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	os.RemoveAll(filepath.Join(inboxDir, e.ID))
	cmd, output, claim := startRewake(t, e)
	runHookTest(t, map[string]string{"hook_event_name": "SessionEnd", "session_id": e.ID, "reason": "other"})
	finishRewake(t, cmd, output, 0)
	if output.String() != "" || fileExists(rewakePath(e.ID)) || fileExists(filepath.Join(watchDir, claim.Endpoint)) {
		t.Fatal("ended watcher output or state remained")
	}
}

func TestHookRewakeReplacesPreviousRegistration(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	os.RemoveAll(filepath.Join(inboxDir, e.ID))
	oldCmd, oldOutput, oldClaim := startRewake(t, e)
	e.Registered += 1
	locked(func() { writeJSON(sessionPath(e.ID), e) })
	cmd, output, claim := startRewake(t, e)
	finishRewake(t, oldCmd, oldOutput, 0)
	if claim.Endpoint == oldClaim.Endpoint {
		t.Fatal("resumed registration reused old watcher endpoint")
	}
	current, _ := load[rewakeClaim](rewakePath(e.ID))
	if current == nil || !sameRewake(current, claim) || !fileExists(filepath.Join(watchDir, claim.Endpoint)) {
		t.Fatal("old watcher cleanup removed resumed watcher")
	}
	locked(func() { drop(e.ID) })
	finishRewake(t, cmd, output, 0)
}

func TestHookRewakeReplacesStaleClaim(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	os.RemoveAll(filepath.Join(inboxDir, e.ID))
	os.MkdirAll(filepath.Dir(rewakePath(e.ID)), 0o700)
	stale := &rewakeClaim{PID: 0, Start: 0, Registered: e.Registered, Endpoint: "stale-rewake"}
	writeJSON(rewakePath(e.ID), stale)
	watch(stale.Endpoint)
	cmd, output, claim := startRewake(t, e)
	if claim.Endpoint == stale.Endpoint || fileExists(filepath.Join(watchDir, stale.Endpoint)) {
		t.Fatal("stale watcher claim remained")
	}
	locked(func() { drop(e.ID) })
	finishRewake(t, cmd, output, 0)
}

func TestHookRewakeIgnoresManualAndSubagents(t *testing.T) {
	for _, tc := range []struct{ name, delivery, agentID string }{{"manual", deliverManual, ""}, {"subagent", deliverHooks, "worker123"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupHookTest(t, tc.delivery)
			cmd, output := rewakeCommand(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID, "agent_id": tc.agentID})
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			finishRewake(t, cmd, output, 0)
			if output.String() != "" || len(inboxFiles(e.ID)) != 1 || fileExists(rewakePath(e.ID)) {
				t.Fatal("ignored rewake changed inbox or emitted output")
			}
		})
	}
}

func TestHookRewakeIgnoresOtherEvents(t *testing.T) {
	for _, event := range []string{"UserPromptSubmit", "SessionEnd", "PreToolUse", ""} {
		t.Run(event, func(t *testing.T) {
			e := setupHookTest(t, deliverHooks)
			cmd, output := rewakeCommand(t, map[string]string{"hook_event_name": event, "session_id": e.ID})
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			finishRewake(t, cmd, output, 0)
			if output.String() != "" || len(inboxFiles(e.ID)) != 1 || fileExists(rewakePath(e.ID)) || !fileExists(sessionPath(e.ID)) {
				t.Fatal("invalid rewake event altered session or inbox")
			}
		})
	}
}

func TestHookRewakeKeepsUnreadMessage(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	cmd, output := rewakeCommand(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID})
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
		t.Fatalf("rewake exit = %v, want 2; output %s", err, output)
	}
	if len(inboxFiles(e.ID)) != 1 {
		t.Fatal("rewake consumed unread message")
	}
	if output.String() == "" {
		t.Fatal("rewake omitted wake notice")
	}
	if result := runHookTest(t, map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": e.ID}); result == "" || len(inboxFiles(e.ID)) != 0 {
		t.Fatalf("normal hook did not deliver after rewake: %s", result)
	}
}
