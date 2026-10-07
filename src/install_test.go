package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestSmeltConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", filepath.Join(home, "roaming"))
	want := filepath.Join(home, ".config", "smelt")
	if runtime.GOOS == "windows" {
		want = filepath.Join(home, "roaming", "smelt")
	}
	if got := smeltConfigDir(home); got != want {
		t.Fatalf("default config = %q, want %q", got, want)
	}
	xdg := filepath.Join(home, "custom-config")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got := smeltConfigDir(home); got != filepath.Join(xdg, "smelt") {
		t.Fatalf("XDG config = %q", got)
	}
}

func TestInstallSmelt(t *testing.T) {
	config := filepath.Join(t.TempDir(), "smelt")
	installSmelt(config)
	if isDir(config) {
		t.Fatal("created config for an absent harness")
	}
	if err := os.MkdirAll(filepath.Join(config, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(config, "AGENTS.md")
	plugin := filepath.Join(config, "plugins", "agent-bus.lua")
	if err := os.WriteFile(instructions, []byte("# My rules\n\nUse rg.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin, []byte("-- old integration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installSmelt(config)
	want, err := integrations.ReadFile("integrations/smelt/agent-bus.lua")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(plugin)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("installed plugin differs from embedded plugin: %v", err)
	}
	first, err := os.ReadFile(instructions)
	if err != nil || !bytes.HasPrefix(first, []byte("# My rules\n\nUse rg.\n")) || bytes.Count(first, []byte("# Agent bus\n")) != 1 {
		t.Fatalf("existing instructions lost or bus section missing: %s (%v)", first, err)
	}
	installSmelt(config)
	second, err := os.ReadFile(instructions)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("reinstall changed instructions: %v", err)
	}
}

func TestHarnessOfSmelt(t *testing.T) {
	for _, p := range []proc{
		{name: "smelt", args: []string{"smelt"}},
		{name: "smelt.exe", args: []string{`C:\\bin\\smelt.exe`}},
		{name: "smelt-release", args: []string{"/usr/local/bin/smelt"}},
	} {
		if got := harnessOf(&p); got != "smelt" {
			t.Fatalf("harnessOf(%+v) = %q", p, got)
		}
	}
}

func TestSmeltSessionIdentity(t *testing.T) {
	setupHookTest(t, deliverHooks)
	t.Setenv("AGENT_BUS_ID", "")
	t.Setenv("SMELT_SESSION_ID", "smelt-session")
	t.Setenv("CODEX_THREAD_ID", "outer-codex-session")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHookProcess$")
	cmd.Args[0] = "smelt"
	cmd.Env = append(os.Environ(), "BUS_HOOK_TEST_PROCESS=harness", "BUS_HOOK_TEST_COMMAND=register", "AGENT_BUS_DIR="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("register from Smelt: %v\n%s", err, out)
	}
	e, ok := load[entry](sessionPath("smelt-session"))
	if !ok {
		t.Fatal("Smelt session was not registered")
	}
	if e.Harness != "smelt" || e.SessionID != "smelt-session" {
		t.Fatalf("wrong Smelt registration: %+v", e)
	}
	if fileExists(sessionPath("outer-codex-session")) {
		t.Fatal("Smelt used the outer Codex session identity")
	}
}

func TestMergeHooksSubagentIsolation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		isolation bool
	}{
		{"Claude", true},
		{"Codex", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			original := []byte(`{"permissions":{"allow":["Bash(go test:*)"]},"hooks":{"PreToolUse":[{"matcher":"Read","hooks":[{"type":"command","command":"custom-read-hook"}]}],"SessionStart":[{"hooks":[{"type":"command","command":"custom-start-hook"}]}]},"model":"custom-model"}`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			mergeHooks(path, "/tmp/bin with spaces/agent-bus", tc.isolation)
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parseOrdered(first)
			if err != nil {
				t.Fatal(err)
			}
			settings := parsed.(*object)
			before, _ := parseOrdered(original)
			old := before.(*object)
			if !reflect.DeepEqual(settings.keys, old.keys) || !reflect.DeepEqual(settings.get("permissions"), old.get("permissions")) || settings.get("model") != old.get("model") {
				t.Fatalf("existing settings changed: %s", first)
			}
			hooks := settings.get("hooks").(*object)
			oldHooks := old.get("hooks").(*object)
			for _, event := range oldHooks.keys {
				if !reflect.DeepEqual(hooks.get(event).([]any)[0], oldHooks.get(event).([]any)[0]) {
					t.Fatalf("existing %s hook changed", event)
				}
			}
			pre := hooks.get("PreToolUse").([]any)
			if tc.isolation {
				if len(pre) != 2 || pre[1].(*object).get("matcher") != "Bash" || !hasBusHook(pre[1:]) {
					t.Fatalf("missing Claude Bash isolation hook: %s", first)
				}
				command := pre[1].(*object).get("hooks").([]any)[0].(*object).get("command")
				if command != `"/tmp/bin with spaces/agent-bus" hook` {
					t.Fatalf("unexpected command: %v", command)
				}
			} else if len(pre) != 1 {
				t.Fatalf("added a Codex PreToolUse hook: %s", first)
			}
			mergeHooks(path, "/tmp/bin with spaces/agent-bus", tc.isolation)
			second, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("second merge changed settings")
			}
		})
	}
}

func TestMergeHooksCodexHasNoPreToolUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	mergeHooks(path, "/tmp/agent-bus", false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseOrdered(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.(*object).get("hooks").(*object).get("PreToolUse") != nil {
		t.Fatalf("added a Codex PreToolUse hook: %s", data)
	}
}

func TestMergeHooksUpgradesStopWatcher(t *testing.T) {
	for _, claude := range []bool{true, false} {
		name := "Codex"
		if claude {
			name = "Claude"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			original := []byte(`{"model":"custom","hooks":{"Stop":[{"hooks":[{"type":"command","command":"custom-stop-hook"},{"type":"command","command":"/old/agent-bus hook","timeout":5}]}]}}`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			mergeHooks(path, "/new/agent-bus", claude)
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parseOrdered(first)
			if err != nil {
				t.Fatal(err)
			}
			settings := parsed.(*object)
			stop := settings.get("hooks").(*object).get("Stop").([]any)
			before, _ := parseOrdered(original)
			oldStop := before.(*object).get("hooks").(*object).get("Stop").([]any)
			if settings.get("model") != "custom" || !reflect.DeepEqual(stop[0], oldStop[0]) {
				t.Fatalf("existing settings or Stop hooks changed: %s", first)
			}
			if claude {
				if len(stop) != 2 || !hasBusHookWithArgs(stop[1:], "--rewake") {
					t.Fatalf("missing Claude Stop watcher: %s", first)
				}
				watcher := stop[1].(*object).get("hooks").([]any)[0].(*object)
				if watcher.get("asyncRewake") != true || watcher.get("timeout").(json.Number).String() != "604800" {
					t.Fatalf("incorrect watcher settings: %s", first)
				}
			} else if len(stop) != 1 || hasBusHookWithArgs(stop, "--rewake") {
				t.Fatalf("added Codex Stop watcher: %s", first)
			}
			mergeHooks(path, "/new/agent-bus", claude)
			second, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("second merge changed settings")
			}
		})
	}
}

func TestAddInstructionsReplacesSection(t *testing.T) {
	want, _ := integrations.ReadFile("integrations/instructions.md")
	for name, c := range map[string]struct{ before, after string }{
		"middle": {"# Tools\n\nuse rg\n\n", "\n# Later\n\nkeep me\n"},
		"end":    {"# Tools\n\nuse rg\n\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "AGENTS.md")
			os.WriteFile(path, []byte(c.before+"# Agent bus\n\n- old advice\n"+c.after), 0o644)
			addInstructions(path)
			got, _ := os.ReadFile(path)
			expect := c.before + string(want) + c.after
			if c.after != "" {
				expect = c.before + strings.TrimRight(string(want), "\n") + "\n\n" + strings.TrimPrefix(c.after, "\n")
			}
			if string(got) != expect {
				t.Fatalf("got:\n%s\nwant:\n%s", got, expect)
			}
			addInstructions(path)
			if again, _ := os.ReadFile(path); string(again) != string(got) {
				t.Fatal("second install changed the file")
			}
		})
	}
}
