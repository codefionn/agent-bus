package bus

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookSubagentLeavesParentInbox(t *testing.T) {
	for _, fields := range []map[string]string{
		{"agent_id": "worker123", "agent_type": "Explore"},
		{"transcript_path": "/home/user/.claude/projects/project/parent/subagents/agent-worker123.jsonl"},
		{"transcript_path": "/home/user/.claude/projects/project/parent/subagents/workflows/wf123/agent-worker123.jsonl"},
		{"transcript_path": `C:\Users\user\.claude\projects\project\parent\subagents\agent-worker123.jsonl`},
	} {
		t.Run(strings.Join([]string{fields["agent_id"], fields["transcript_path"]}, "-"), func(t *testing.T) {
			e := setupHookTest(t, deliverHooks)
			fields["hook_event_name"] = "PostToolUse"
			fields["session_id"] = e.ID
			fields["cwd"] = "/worker-directory"
			if output := runHookTest(t, fields); output != "" {
				t.Errorf("subagent received parent output: %s", output)
			}
			if len(inboxFiles(e.ID)) != 1 {
				t.Error("subagent consumed parent's unread message")
			}
			current, _ := load[entry](sessionPath(e.ID))
			if current.Cwd != e.Cwd {
				t.Error("subagent changed parent's directory")
			}
			output := runHookTest(t, map[string]string{"hook_event_name": "PostToolUse", "session_id": e.ID, "agent_type": "custom-main-agent", "transcript_path": "/home/user/.claude/projects/project/parent.jsonl"})
			if !strings.Contains(output, "parent-only message") || len(inboxFiles(e.ID)) != 0 {
				t.Errorf("main agent did not receive message: %s", output)
			}
		})
	}
}

func TestHookManualDeliveryKeepsInbox(t *testing.T) {
	e := setupHookTest(t, deliverManual)
	if output := runHookTest(t, map[string]string{"hook_event_name": "PostToolUse", "session_id": e.ID}); output != "" || len(inboxFiles(e.ID)) != 1 {
		t.Errorf("manual delivery changed: %s", output)
	}
}

func TestHookSubagentStopLeavesControllerInbox(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	e.Meta = map[string]string{"role": "controller"}
	writeJSON(sessionPath(e.ID), e)
	output := runHookTest(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID, "agent_id": "worker123", "agent_type": "Explore"})
	if output != "" || len(inboxFiles(e.ID)) != 1 {
		t.Errorf("subagent Stop intercepted controller: %s", output)
	}
}

func TestHookRetiredControllerCanStop(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	e.Meta = map[string]string{"role": "controller"}
	writeJSON(sessionPath(e.ID), e)
	os.RemoveAll(filepath.Join(inboxDir, e.ID))
	if runHookTest(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID}) == "" {
		t.Fatal("active controller allowed to stop")
	}
	delete(e.Meta, "role")
	writeJSON(sessionPath(e.ID), e)
	if output := runHookTest(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID}); output != "" {
		t.Errorf("retired controller blocked: %s", output)
	}
}

func TestHookSessionEnd(t *testing.T) {
	for _, reason := range []string{"clear", "resume", "prompt_input_exit"} {
		t.Run(reason, func(t *testing.T) {
			e := setupHookTest(t, deliverHooks)
			runHookTest(t, map[string]string{"hook_event_name": "SessionEnd", "session_id": e.ID, "reason": reason})
			wantPresent := reason == "prompt_input_exit"
			if fileExists(sessionPath(e.ID)) != wantPresent {
				t.Errorf("session presence differs for %s", reason)
			}
		})
	}
}

func setupHookTest(t *testing.T, delivery string) *entry {
	t.Helper()
	oldRoot, oldSess, oldInbox := root, sessDir, inboxDir
	root = t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sessDir, inboxDir = filepath.Join(root, "sessions"), filepath.Join(root, "inbox")
	t.Cleanup(func() { root, sessDir, inboxDir = oldRoot, oldSess, oldInbox })
	t.Setenv("AGENT_BUS_ID", "parent")
	setup()
	e := &entry{ID: "parent", Name: "parent", Delivery: delivery, PID: os.Getpid(), Start: procStart(os.Getpid()), Seen: now(), Cwd: "/parent-directory"}
	writeJSON(sessionPath(e.ID), e)
	os.MkdirAll(filepath.Join(inboxDir, e.ID), 0o700)
	writeJSON(filepath.Join(inboxDir, e.ID, "1-peer.json"), &message{Time: now(), From: "peer", FromName: "peer", Text: "parent-only message"})
	return e
}

func runHookTest(t *testing.T, payload map[string]string) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHookProcess$")
	cmd.Args[0] = "claude"
	cmd.Env = append(os.Environ(), "BUS_HOOK_TEST_PROCESS=harness", "AGENT_BUS_DIR="+root)
	cmd.Stdin = strings.NewReader(string(data))
	result, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	return string(result)
}

// A real harness ancestor makes these tests independent of the process that
// runs go test. The wrapper's argv[0] is recognized as Claude by findHarness.
func TestHookProcess(t *testing.T) {
	switch os.Getenv("BUS_HOOK_TEST_PROCESS") {
	case "harness":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestHookProcess$")
		cmd.Env = append(os.Environ(), "BUS_HOOK_TEST_PROCESS=hook")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "hook":
		cmdHook(nil)
		os.Exit(0)
	}
}

func TestHookStop(t *testing.T) {
	for _, tc := range []struct {
		name, delivery            string
		controller, unread, block bool
	}{
		{"messages", deliverHooks, false, true, true},
		{"controller", deliverHooks, true, false, true},
		{"controller-messages", deliverHooks, true, true, true},
		{"ordinary-idle", deliverHooks, false, false, false},
		{"manual", deliverManual, false, true, false},
		{"manual-controller", deliverManual, true, true, true},
		{"manual-controller-idle", deliverManual, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupHookTest(t, tc.delivery)
			if tc.controller {
				e.Meta = map[string]string{"role": "controller"}
				writeJSON(sessionPath(e.ID), e)
			}
			if !tc.unread {
				os.RemoveAll(filepath.Join(inboxDir, e.ID))
			}
			output := runHookTest(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID})
			if tc.block {
				var result struct{ Decision, Reason string }
				if err := json.Unmarshal([]byte(output), &result); err != nil || result.Decision != "block" || result.Reason == "" {
					t.Fatalf("Stop did not block with a reason: %s", output)
				}
				if tc.unread && tc.delivery != deliverManual && !strings.Contains(result.Reason, "parent-only message") {
					t.Error("Stop omitted pending message")
				}
			} else if output != "" {
				t.Errorf("unexpected Stop output: %s", output)
			}
			if tc.delivery == deliverManual && tc.unread && len(inboxFiles(e.ID)) != 1 {
				t.Error("Stop consumed manual inbox")
			}
		})
	}
}
