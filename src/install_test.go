package bus

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
