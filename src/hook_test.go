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
	for _, reason := range []string{"clear", "resume", "other", "prompt_input_exit", "logout", ""} {
		t.Run(reason, func(t *testing.T) {
			e := setupHookTest(t, deliverHooks)
			runHookTest(t, map[string]string{"hook_event_name": "SessionEnd", "session_id": e.ID, "reason": reason})
			if fileExists(sessionPath(e.ID)) {
				t.Errorf("session presence differs for %s", reason)
			}
			if !fileExists(leftPath(e.ID)) {
				t.Error("ended session missing left marker")
			}
			runHookTest(t, map[string]string{"hook_event_name": "PostToolUse", "session_id": e.ID})
			if fileExists(sessionPath(e.ID)) {
				t.Fatal("late hook rejoined ended session")
			}
			runHookTest(t, map[string]string{"hook_event_name": "SessionStart", "session_id": e.ID})
			if !fileExists(sessionPath(e.ID)) || fileExists(leftPath(e.ID)) {
				t.Fatal("SessionStart did not resume ended session")
			}
		})
	}
}

func TestHookControllerSessionEnd(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	e.Meta = map[string]string{"role": "controller"}
	writeJSON(sessionPath(e.ID), e)
	runHookTest(t, map[string]string{"hook_event_name": "SessionEnd", "session_id": e.ID, "reason": "other"})
	if fileExists(sessionPath(e.ID)) {
		t.Fatal("controller survived SessionEnd")
	}
	if output := runHookTest(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID}); output != "" || fileExists(sessionPath(e.ID)) {
		t.Fatalf("late Stop rejoined or blocked ended controller: %s", output)
	}
}

func setupHookTest(t *testing.T, delivery string) *entry {
	t.Helper()
	oldRoot, oldSess, oldInbox := root, sessDir, inboxDir
	oldLog, oldLogOld, oldWatch, oldPending := eventLog, eventLogOld, watchDir, eventsPending
	root = t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sessDir, inboxDir = filepath.Join(root, "sessions"), filepath.Join(root, "inbox")
	eventLog, eventLogOld, watchDir, eventsPending = filepath.Join(root, "events.jsonl"), filepath.Join(root, "events.1.jsonl"), filepath.Join(root, "watchers"), false
	t.Cleanup(func() { root, sessDir, inboxDir = oldRoot, oldSess, oldInbox })
	t.Cleanup(func() { eventLog, eventLogOld, watchDir, eventsPending = oldLog, oldLogOld, oldWatch, oldPending })
	t.Setenv("AGENT_BUS_ID", "parent")
	setup()
	t.Setenv("AGENT_BUS_AUTO", "1")
	e := &entry{ID: "parent", Name: "parent", Harness: "claude", Delivery: delivery, PID: os.Getpid(), Start: procStart(os.Getpid()), Seen: now(), Registered: now(), Cwd: "/parent-directory"}
	writeJSON(sessionPath(e.ID), e)
	os.MkdirAll(filepath.Join(inboxDir, e.ID), 0o700)
	writeJSON(filepath.Join(inboxDir, e.ID, "1-peer.json"), &message{Time: now(), From: "peer", FromName: "peer", Text: "parent-only message"})
	return e
}

func runHookTest(t *testing.T, payload map[string]string) string {
	return runHookPayloadTest(t, payload)
}

func runHookPayloadTest(t *testing.T, payload any) string {
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
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "BUS_HOOK_TEST_PROCESS=harness", "AGENT_BUS_DIR="+root)
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
		if os.Getenv("BUS_HOOK_TEST_COMMAND") == "shell" {
			cmd = exec.Command("bash", "-c", os.Getenv("BUS_HOOK_TEST_SHELL"))
		}
		cmd.Env = append(os.Environ(), "BUS_HOOK_TEST_PROCESS=hook")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				os.Exit(exit.ExitCode())
			}
			os.Exit(1)
		}
		os.Exit(0)
	case "hook":
		switch os.Getenv("BUS_HOOK_TEST_COMMAND") {
		case "register":
			cmdRegister([]string{"worker"})
			os.Exit(0)
		case "unregister":
			cmdUnregister(nil)
			os.Exit(0)
		}
		if os.Getenv("BUS_HOOK_TEST_COMMAND") == "wait" {
			cmdWait([]string{"--timeout", "60"})
		}
		if os.Getenv("BUS_HOOK_TEST_COMMAND") == "rewake" {
			cmdHook([]string{"--rewake"})
			os.Exit(0)
		}
		cmdHook(nil)
		os.Exit(0)
	}
}

func TestHookSubagentBashIsolation(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quotedExe := "'" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "'"
	command := "BUS_HOOK_TEST_COMMAND=register " + quotedExe + " -test.run=^TestHookProcess$ >/dev/null; BUS_HOOK_TEST_COMMAND=unregister " + quotedExe + " -test.run=^TestHookProcess$ >/dev/null"
	output := runHookPayloadTest(t, map[string]any{"hook_event_name": "PreToolUse", "session_id": e.ID, "agent_id": "worker123", "agent_type": "Explore", "tool_name": "Bash", "tool_input": map[string]any{"command": command, "timeout": 1234, "description": "worker cleanup", "run_in_background": true}})
	var result struct {
		HookSpecificOutput struct {
			UpdatedInput       map[string]any `json:"updatedInput"`
			PermissionDecision string         `json:"permissionDecision"`
			AdditionalContext  string         `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if output != "" {
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatal(err)
		}
	}
	if rewritten, ok := result.HookSpecificOutput.UpdatedInput["command"].(string); ok {
		command = rewritten
	}
	cmd := exec.Command(exe, "-test.run=^TestHookProcess$")
	cmd.Args[0] = "claude"
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "BUS_HOOK_TEST_PROCESS=harness", "BUS_HOOK_TEST_COMMAND=shell", "BUS_HOOK_TEST_SHELL="+command, "AGENT_BUS_DIR="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker shell: %v %s", err, output)
	}
	parent, exists := load[entry](sessionPath(e.ID))
	if !exists || parent.Name != e.Name || len(inboxFiles(e.ID)) != 1 || fileExists(leftPath(e.ID)) {
		t.Fatal("worker register/unregister altered parent session or inbox")
	}
	if result.HookSpecificOutput.PermissionDecision != "" {
		t.Error("rewrite bypassed normal permission checks")
	}
	if !strings.Contains(result.HookSpecificOutput.AdditionalContext, "agent-bus register NAME") {
		t.Error("unregistered worker missing own-registration guidance")
	}
	input := result.HookSpecificOutput.UpdatedInput
	if input["timeout"] != float64(1234) || input["description"] != "worker cleanup" || input["run_in_background"] != true {
		t.Errorf("tool input fields lost: %v", input)
	}
	left, _ := os.ReadDir(filepath.Join(root, "left"))
	if len(left) != 1 || left[0].Name() == e.ID {
		t.Errorf("worker did not unregister its own identity: %v", left)
	}
}

func TestHookSubagentBashIdentity(t *testing.T) {
	e := setupHookTest(t, deliverHooks)
	os.RemoveAll(filepath.Join(inboxDir, e.ID))
	lastContext := ""
	rewrite := func(agentID, transcript, tool string) string {
		t.Helper()
		output := runHookPayloadTest(t, map[string]any{"hook_event_name": "PreToolUse", "session_id": e.ID, "agent_id": agentID, "transcript_path": transcript, "tool_name": tool, "tool_input": map[string]any{"command": "printf hello"}})
		if output == "" {
			return ""
		}
		var result struct {
			HookSpecificOutput struct {
				Event              string         `json:"hookEventName"`
				UpdatedInput       map[string]any `json:"updatedInput"`
				PermissionDecision string         `json:"permissionDecision"`
				AdditionalContext  string         `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatal(err)
		}
		if result.HookSpecificOutput.Event != "PreToolUse" || result.HookSpecificOutput.PermissionDecision != "" {
			t.Fatalf("unexpected hook output: %s", output)
		}
		lastContext = result.HookSpecificOutput.AdditionalContext
		command, _ := result.HookSpecificOutput.UpdatedInput["command"].(string)
		if !strings.HasSuffix(command, "\nprintf hello") || !strings.HasPrefix(command, "export AGENT_BUS_ID='claude-subagent-") {
			t.Fatalf("unsafe or changed command: %s", command)
		}
		return command
	}
	first := rewrite("worker123", "", "Bash")
	if first == "" || first != rewrite("worker123", "", "Bash") {
		t.Fatal("worker identity not stable")
	}
	childID := strings.Split(first, "'")[1]
	child := *e
	child.ID = childID
	writeJSON(sessionPath(childID), &child)
	if rewrite("worker123", "", "Bash") != first || lastContext != "" {
		t.Fatal("registered worker got unregistered guidance or changed identity")
	}
	if first == rewrite("worker456", "", "Bash") {
		t.Fatal("siblings share identity")
	}
	fallback := rewrite("", "/parent/subagents/workflows/wf123/agent-abc.jsonl", "Bash")
	if fallback == "" || fallback == rewrite("", "/parent/subagents/workflows/wf456/agent-abc.jsonl", "Bash") {
		t.Fatal("workflow transcript fallback is not isolated")
	}
	if rewrite("worker123", "", "Read") != "" || rewrite("", "/parent.jsonl", "Bash") != "" {
		t.Fatal("rewrote ordinary or non-Bash tool")
	}
	t.Setenv("AGENT_BUS_ID", "custom-parent'$(danger)")
	if first == rewrite("worker123", "", "Bash") {
		t.Fatal("inherited custom parent identity ignored")
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
				if tc.unread && tc.delivery != deliverManual && !strings.Contains(result.Reason, "agent-bus inbox") {
					t.Error("Stop omitted inbox notice")
				}
			} else if output != "" {
				t.Errorf("unexpected Stop output: %s", output)
			}
			if tc.unread && len(inboxFiles(e.ID)) != 1 {
				t.Error("Stop consumed inbox instead of leaving messages for context delivery")
			}
		})
	}
}

func TestHookStopKeepsMessageContentOutOfBlockReason(t *testing.T) {
	for _, untrusted := range []string{"", "unverified outside content"} {
		t.Run(untrusted, func(t *testing.T) {
			e := setupHookTest(t, deliverHooks)
			os.RemoveAll(filepath.Join(inboxDir, e.ID))
			locked(func() {
				deliver(&entry{ID: "fixture-peer", Name: "fixture-peer"}, []*entry{e}, nil, "review this finding", untrusted)
			})
			output := runHookTest(t, map[string]string{"hook_event_name": "Stop", "session_id": e.ID})
			var result struct{ Decision, Reason string }
			if err := json.Unmarshal([]byte(output), &result); err != nil || result.Decision != "block" || !strings.Contains(result.Reason, "agent-bus inbox") {
				t.Fatalf("Stop omitted inbox notice: %s", output)
			}
			if strings.Contains(result.Reason, "review this finding") || strings.Contains(result.Reason, "unverified outside content") || strings.Contains(result.Reason, "<untrusted-") || strings.Contains(result.Reason, trustNote) {
				t.Fatalf("Stop put message data in its block reason: %s", output)
			}
			if len(inboxFiles(e.ID)) != 1 {
				t.Fatal("Stop consumed queued message")
			}
			context := runHookTest(t, map[string]string{"hook_event_name": "PreToolUse", "session_id": e.ID})
			if !strings.Contains(context, "review this finding") || (untrusted != "" && !strings.Contains(context, untrusted)) || len(inboxFiles(e.ID)) != 0 {
				t.Fatalf("next hook did not deliver message as context: %s", context)
			}
		})
	}
}
