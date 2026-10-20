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

func TestRemoveInstructions(t *testing.T) {
	for name, c := range map[string]struct{ before, after, want string }{
		"middle": {"# Tools\n\nuse rg\n\n", "\n# Later\n\nkeep me\n", "# Tools\n\nuse rg\n\n# Later\n\nkeep me\n"},
		"end":    {"# Tools\n\nuse rg\n\n", "", "# Tools\n\nuse rg\n"},
		"only":   {"", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "AGENTS.md")
			if err := os.WriteFile(path, []byte(c.before+"# Agent bus\n\n- old advice\n"+c.after), 0o644); err != nil {
				t.Fatal(err)
			}
			if !removeInstructions(path) {
				t.Fatal("left the section in the file")
			}
			got, err := os.ReadFile(path)
			if c.want == "" {
				if err == nil {
					t.Fatalf("left %q behind", got)
				}
			} else if err != nil || string(got) != c.want {
				t.Fatalf("got:\n%s\nwant:\n%s (%v)", got, c.want, err)
			}
			if removeInstructions(path) {
				t.Fatal("second run changed the file")
			}
		})
	}
}

func TestUnmergeHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{"permissions":{"allow":["Bash(go test:*)"]},"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/home/u/.local/bin/agent-bus hook","timeout":5}]},{"hooks":[{"type":"command","command":"custom-start-hook"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/home/u/.local/bin/agent-bus hook"}]}],"Stop":[{"hooks":[{"type":"command","command":"custom-stop-hook"},{"type":"command","command":"/home/u/.local/bin/agent-bus hook --rewake","asyncRewake":true,"timeout":604800}]}]},"model":"custom-model"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if !unmergeHooks(path) {
		t.Fatal("did not remove the agent-bus hooks")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseOrdered(data)
	if err != nil {
		t.Fatal(err)
	}
	settings := parsed.(*object)
	if !reflect.DeepEqual(settings.keys, []string{"permissions", "hooks", "model"}) {
		t.Fatalf("settings keys changed: %s", data)
	}
	if !reflect.DeepEqual(settings.get("permissions"), mustJSON(`{"allow":["Bash(go test:*)"]}`)) {
		t.Fatalf("existing settings changed: %s", data)
	}
	hooks := settings.get("hooks").(*object)
	if !reflect.DeepEqual(hooks.keys, []string{"SessionStart", "Stop"}) {
		t.Fatalf("hook events changed: %s", data)
	}
	start := hooks.get("SessionStart").([]any)
	if len(start) != 1 || start[0].(*object).get("hooks").([]any)[0].(*object).get("command") != "custom-start-hook" {
		t.Fatalf("the custom SessionStart hook did not survive: %s", data)
	}
	stop := hooks.get("Stop").([]any)[0].(*object).get("hooks").([]any)
	if len(stop) != 1 || stop[0].(*object).get("command") != "custom-stop-hook" {
		t.Fatalf("the mixed Stop group lost its custom hook: %s", data)
	}
	if unmergeHooks(path) {
		t.Fatal("second run reported a change")
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, data) {
		t.Fatal("second run rewrote the file")
	}
}

func TestUnmergeHooksRemovesEmptySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(`{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"agent-bus hook"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !unmergeHooks(path) {
		t.Fatal("did not remove the agent-bus hooks")
	}
	if fileExists(path) {
		t.Fatal("left an empty settings file behind")
	}
	if unmergeHooks(path) {
		t.Fatal("second run reported a change")
	}
}

func TestRemoveBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-bus")
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	removeBinary(path)
	if fileExists(path) {
		t.Fatal("binary is still there")
	}
	removeBinary(path) // absent: not an error
}

func TestCmdUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	at := func(parts ...string) string { return filepath.Join(append([]string{home}, parts...)...) }
	write := func(path, content string) string {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	bin := write(at(".local", "bin", exeName), "binary")
	web := write(at(".local", "bin", strings.Replace(exeName, "agent-bus", "agent-bus-web", 1)), "web")
	settings := write(at(".claude", "settings.json"), `{"model":"custom","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"custom-hook"},{"type":"command","command":"agent-bus hook"}]}]}}`)
	claude := write(at(".claude", "CLAUDE.md"), "# Tools\n\nuse rg\n\n# Agent bus\n\n- old advice\n")
	codex := write(at(".codex", "hooks.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"agent-bus hook"}]}]}}`)
	pi := write(at(".pi", "agent", "extensions", "agent-bus.ts"), "extension")
	open := write(at(".config", "opencode", "plugins", "agent-bus.js"), "plugin")
	smelt := write(filepath.Join(smeltConfigDir(home), "plugins", "agent-bus.lua"), "plugin")
	config := write(configPath(), `{"auto":false}`)

	cmdUninstall(nil)

	for _, p := range []string{bin, web, codex, pi, open, smelt, config} {
		if fileExists(p) {
			t.Fatal(p + " still exists")
		}
	}
	if isDir(filepath.Dir(config)) {
		t.Fatal("left the settings directory behind")
	}
	got, err := os.ReadFile(settings)
	if err != nil || strings.Contains(string(got), "agent-bus") || !strings.Contains(string(got), "custom-hook") {
		t.Fatalf("Claude settings lost or kept hooks: %s (%v)", got, err)
	}
	if md, err := os.ReadFile(claude); err != nil || string(md) != "# Tools\n\nuse rg\n" {
		t.Fatalf("CLAUDE.md = %q (%v)", md, err)
	}
}

func mustJSON(text string) any {
	v, err := parseOrdered([]byte(text))
	if err != nil {
		panic(err)
	}
	return v
}
