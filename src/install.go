package bus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	agentbus "agent-bus"
)

var integrations = agentbus.Integrations

// cmdInstall copies this binary to ~/.local/bin and wires it into every
// harness found: Claude Code and Codex hooks, a pi extension, opencode and Smelt
// plugins, and the "Agent bus" section in each harness's global instructions.
// Safe to rerun.
func cmdInstall(args []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		die(1, "%v", err)
	}
	for _, a := range args {
		switch a {
		case "--auto", "--manual":
			on := a == "--auto"
			c := loadConfig()
			c.Auto = &on
			if err := saveConfig(c); err != nil {
				die(1, "%v", err)
			}
		default:
			die(2, "usage: agent-bus install [--auto | --manual]")
		}
	}
	self, err := os.Executable()
	if err != nil {
		die(1, "%v", err)
	}
	self = realpath(self)
	binDir := filepath.Join(home, ".local", "bin")
	bin := filepath.Join(binDir, exeName)
	if err := installBinary(self, bin); err != nil {
		die(1, "install %s: %v", bin, err)
	}
	fmt.Println("installed " + bin)
	// agent-bus-web travels along when it was built next to agent-bus.
	web := strings.Replace(exeName, "agent-bus", "agent-bus-web", 1)
	if src := filepath.Join(filepath.Dir(self), web); fileExists(src) {
		if err := installBinary(src, filepath.Join(binDir, web)); err != nil {
			die(1, "install %s: %v", web, err)
		}
		fmt.Println("installed " + filepath.Join(binDir, web))
	}

	at := func(parts ...string) string { return filepath.Join(append([]string{home}, parts...)...) }
	if isDir(at(".claude")) {
		mergeHooks(at(".claude", "settings.json"), bin, true)
		fmt.Println("installed Claude Code hooks in " + at(".claude", "settings.json"))
		addInstructions(at(".claude", "CLAUDE.md"))
	}
	if isDir(at(".codex")) {
		mergeHooks(at(".codex", "hooks.json"), bin, false)
		fmt.Println("installed Codex hooks in " + at(".codex", "hooks.json"))
		addInstructions(at(".codex", "AGENTS.md"))
	}
	if isDir(at(".pi", "agent")) {
		copyIntegration("integrations/pi/agent-bus.ts", at(".pi", "agent", "extensions", "agent-bus.ts"))
		fmt.Println("installed pi extension")
		addInstructions(at(".pi", "agent", "AGENTS.md"))
	}
	if isDir(at(".config", "opencode")) {
		copyIntegration("integrations/opencode/agent-bus.js", at(".config", "opencode", "plugins", "agent-bus.js"))
		fmt.Println("installed opencode plugin")
		addInstructions(at(".config", "opencode", "AGENTS.md"))
	}
	installSmelt(smeltConfigDir(home))
}

// Smelt uses XDG_CONFIG_HOME on every platform, with APPDATA as the Windows fallback.
func smeltConfigDir(home string) string {
	if config := os.Getenv("XDG_CONFIG_HOME"); config != "" {
		return filepath.Join(config, "smelt")
	}
	if runtime.GOOS == "windows" {
		if config := os.Getenv("APPDATA"); config != "" {
			return filepath.Join(config, "smelt")
		}
		return filepath.Join(home, "AppData", "Roaming", "smelt")
	}
	return filepath.Join(home, ".config", "smelt")
}

func installSmelt(config string) {
	if !isDir(config) {
		return
	}
	copyIntegration("integrations/smelt/agent-bus.lua", filepath.Join(config, "plugins", "agent-bus.lua"))
	fmt.Println("installed Smelt plugin")
	addInstructions(filepath.Join(config, "AGENTS.md"))
}

// cmdUninstall removes the installed binaries and unwires every harness:
// Claude Code and Codex hooks, the pi extension, opencode and Smelt plugins, and
// the "Agent bus" section in each harness's global instructions. The rest of
// each file is left alone. Safe to rerun.
func cmdUninstall(args []string) {
	if len(args) > 0 {
		die(2, "usage: agent-bus uninstall")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		die(1, "%v", err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	removeBinary(filepath.Join(binDir, exeName))
	// agent-bus-web travelled along when it was built next to agent-bus.
	removeBinary(filepath.Join(binDir, strings.Replace(exeName, "agent-bus", "agent-bus-web", 1)))

	at := func(parts ...string) string { return filepath.Join(append([]string{home}, parts...)...) }
	if isDir(at(".claude")) {
		if unmergeHooks(at(".claude", "settings.json")) {
			fmt.Println("removed Claude Code hooks from " + at(".claude", "settings.json"))
		}
		removeInstructions(at(".claude", "CLAUDE.md"))
	}
	if isDir(at(".codex")) {
		if unmergeHooks(at(".codex", "hooks.json")) {
			fmt.Println("removed Codex hooks from " + at(".codex", "hooks.json"))
		}
		removeInstructions(at(".codex", "AGENTS.md"))
	}
	if isDir(at(".pi", "agent")) {
		removeFile(at(".pi", "agent", "extensions", "agent-bus.ts"), "pi extension")
		removeInstructions(at(".pi", "agent", "AGENTS.md"))
	}
	if isDir(at(".config", "opencode")) {
		removeFile(at(".config", "opencode", "plugins", "agent-bus.js"), "opencode plugin")
		removeInstructions(at(".config", "opencode", "AGENTS.md"))
	}
	uninstallSmelt(smeltConfigDir(home))
	if dir := filepath.Dir(configPath()); isDir(dir) {
		if err := os.RemoveAll(dir); err != nil {
			die(1, "remove %s: %v", dir, err)
		}
		fmt.Println("removed settings in " + dir)
	}
}

func uninstallSmelt(config string) {
	if !isDir(config) {
		return
	}
	removeFile(filepath.Join(config, "plugins", "agent-bus.lua"), "Smelt plugin")
	removeInstructions(filepath.Join(config, "AGENTS.md"))
}

// removeBinary deletes an installed binary and reports what went away. Windows
// cannot delete a running executable but can rename it, so one that is still
// running moves aside to be deleted after it exits.
func removeBinary(path string) {
	if !fileExists(path) {
		return
	}
	if err := os.Remove(path); err != nil {
		if runtime.GOOS != "windows" {
			die(1, "remove %s: %v", path, err)
		}
		os.Remove(path + ".old")
		if err := os.Rename(path, path+".old"); err != nil {
			die(1, "remove %s: %v", path, err)
		}
		fmt.Println("moved " + path + " to " + path + ".old, delete it after agent-bus exits")
		return
	}
	fmt.Println("removed " + path)
}

// removeFile deletes a file install copied in and reports the label it went by.
func removeFile(path, label string) {
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			die(1, "remove %s: %v", path, err)
		}
		return
	}
	fmt.Println("removed " + label)
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// installBinary copies the executable self to bin.
func installBinary(self, bin string) error {
	if samePath(self, realpath(bin)) {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	os.MkdirAll(filepath.Dir(bin), 0o755)
	tmp := bin + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// Replaces an old symlink too. Windows cannot replace a running executable
	// but can rename it, so move the old one aside first.
	if err = os.Rename(tmp, bin); err != nil && runtime.GOOS == "windows" {
		os.Remove(bin + ".old")
		if os.Rename(bin, bin+".old") == nil {
			err = os.Rename(tmp, bin)
			os.Remove(bin + ".old")
		}
	}
	return err
}

func copyIntegration(name, dst string) {
	data, _ := integrations.ReadFile(name)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.Remove(dst) // an older install linked it into the repository
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		die(1, "%v", err)
	}
}

// agentBusSection matches the installed "# Agent bus" section up to the next
// top-level heading, so reinstalling replaces it with the current text.
var agentBusSection = regexp.MustCompile(`(?ms)^# Agent bus\r?\n.*?(?:^# |\z)`)

func addInstructions(path string) {
	os.MkdirAll(filepath.Dir(path), 0o755)
	old, _ := os.ReadFile(path)
	text, _ := integrations.ReadFile("integrations/instructions.md")
	if loc := agentBusSection.FindIndex(old); loc != nil {
		end := loc[1]
		if bytes.HasSuffix(old[:end], []byte("# ")) {
			end -= 2 // keep the next section's heading
		}
		section := text
		if end < len(old) {
			section = append(bytes.TrimRight(section, "\n"), '\n', '\n')
		}
		updated := slices.Concat(old[:loc[0]], section, old[end:])
		if bytes.Equal(updated, old) {
			return
		}
		if err := os.WriteFile(path, updated, 0o644); err != nil {
			die(1, "%v", err)
		}
		fmt.Println("updated instructions in " + path)
		return
	}
	if len(old) > 0 {
		text = append([]byte("\n"), text...)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		die(1, "%v", err)
	}
	defer f.Close()
	f.Write(text)
	fmt.Println("instructions in " + path)
}

// removeInstructions drops the installed "# Agent bus" section and reports
// whether it changed anything. The rest of the file is left alone; a file that
// held nothing else goes away.
func removeInstructions(path string) bool {
	old, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	loc := agentBusSection.FindIndex(old)
	if loc == nil {
		return false
	}
	end := loc[1]
	if bytes.HasSuffix(old[:end], []byte("# ")) {
		end -= 2 // keep the next section's heading
	}
	head, tail := old[:loc[0]], old[end:]
	if len(bytes.TrimSpace(tail)) == 0 {
		tail = nil // the section ran to the end of the file
		head = bytes.TrimRight(head, "\r\n")
		if len(head) > 0 {
			head = append(head, '\n')
		}
	}
	updated := slices.Concat(head, tail)
	if len(bytes.TrimSpace(updated)) == 0 {
		if err := os.Remove(path); err != nil {
			die(1, "remove %s: %v", path, err)
		}
		fmt.Println("removed instructions in " + path)
		return true
	}
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		die(1, "%v", err)
	}
	fmt.Println("removed instructions in " + path)
	return true
}

var busHook = regexp.MustCompile(`agent-bus(\.exe)?"?\s+hook\b`)

// mergeHooks adds missing agent-bus hooks. Keys keep their order.
func mergeHooks(path, bin string, withClaudeHooks bool) {
	command := bin
	if runtime.GOOS == "windows" || strings.ContainsAny(bin, " \t") {
		command = `"` + filepath.ToSlash(bin) + `"`
	}
	escaped, _ := json.Marshal(command)
	tmpl, _ := integrations.ReadFile("integrations/hooks.json")
	tmpl = bytes.ReplaceAll(tmpl, []byte("AGENT_BUS"), escaped[1:len(escaped)-1])
	add, err := parseOrdered(tmpl)
	if err != nil {
		die(1, "hooks template: %v", err)
	}

	cur := any(&object{})
	if data, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(data)) > 0 {
		if cur, err = parseOrdered(data); err != nil {
			die(1, "%s: %v", path, err)
		}
	}
	settings, ok := cur.(*object)
	if !ok {
		die(1, "%s: not a JSON object", path)
	}
	hooks, _ := settings.get("hooks").(*object)
	if hooks == nil {
		hooks = &object{}
	}
	changed := false
	addHooks := add.(*object).get("hooks").(*object)
	for _, event := range addHooks.keys {
		// Claude and Codex use different input-rewrite contracts.
		if event == "PreToolUse" && !withClaudeHooks {
			continue
		}
		existing, _ := hooks.get(event).([]any)
		for _, group := range addHooks.get(event).([]any) {
			list := group.(*object).get("hooks").([]any)
			command := list[0].(*object).get("command").(string)
			args := strings.TrimSpace(command[busHook.FindStringIndex(command)[1]:])
			if args == "--rewake" && !withClaudeHooks {
				continue
			}
			if hasBusHookWithArgs(existing, args) {
				continue
			}
			existing = append(existing, group)
			hooks.set(event, existing)
			changed = true
		}
	}
	if !changed {
		return
	}
	settings.set("hooks", hooks)
	data, _ := json.MarshalIndent(settings, "", "  ")
	if err := writeAtomic(path, append(data, '\n')); err != nil {
		die(1, "%v", err)
	}
}

func hasBusHook(groups []any) bool {
	return hasBusHookWithArgs(groups, "")
}

func hasBusHookWithArgs(groups []any, args string) bool {
	for _, g := range groups {
		obj, _ := g.(*object)
		if obj == nil {
			continue
		}
		list, _ := obj.get("hooks").([]any)
		for _, h := range list {
			if ho, _ := h.(*object); ho != nil {
				if cmd, _ := ho.get("command").(string); cmd != "" {
					if match := busHook.FindStringIndex(cmd); match != nil && strings.TrimSpace(cmd[match[1]:]) == args {
						return true
					}
				}
			}
		}
	}
	return false
}

// unmergeHooks drops the agent-bus hooks from a harness settings file and
// reports whether it changed anything. The other hooks and the key order stay.
func unmergeHooks(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return false
	}
	cur, err := parseOrdered(data)
	if err != nil {
		die(1, "%s: %v", path, err)
	}
	settings, ok := cur.(*object)
	if !ok {
		die(1, "%s: not a JSON object", path)
	}
	hooks, _ := settings.get("hooks").(*object)
	if hooks == nil {
		return false
	}
	changed := false
	for _, event := range slices.Clone(hooks.keys) {
		groups, _ := hooks.get(event).([]any)
		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			group, ok := g.(*object)
			if !ok {
				kept = append(kept, g)
				continue
			}
			list, _ := group.get("hooks").([]any)
			keptHooks := make([]any, 0, len(list))
			for _, h := range list {
				if isBusHook(h) {
					changed = true
					continue
				}
				keptHooks = append(keptHooks, h)
			}
			if len(keptHooks) == 0 && len(list) > 0 {
				changed = true
				continue // the group held nothing but agent-bus
			}
			if len(keptHooks) != len(list) {
				group.set("hooks", keptHooks)
			}
			kept = append(kept, group)
		}
		if len(kept) == 0 {
			if len(groups) > 0 {
				hooks.del(event)
			}
			continue
		}
		hooks.set(event, kept)
	}
	if !changed {
		return false
	}
	if len(hooks.keys) == 0 {
		settings.del("hooks")
	}
	if len(settings.keys) == 0 {
		// The file held nothing but our hooks.
		if err := os.Remove(path); err != nil {
			die(1, "remove %s: %v", path, err)
		}
		return true
	}
	data, _ = json.MarshalIndent(settings, "", "  ")
	if err := writeAtomic(path, append(data, '\n')); err != nil {
		die(1, "%v", err)
	}
	return true
}

func isBusHook(h any) bool {
	ho, _ := h.(*object)
	if ho == nil {
		return false
	}
	cmd, _ := ho.get("command").(string)
	return busHook.MatchString(cmd)
}

// object is a JSON object that remembers key order, so rewriting a user's
// settings file only adds what we add.
type object struct {
	keys []string
	vals map[string]any
}

func (o *object) get(k string) any { return o.vals[k] }

func (o *object) set(k string, v any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	o.keys = slices.DeleteFunc(o.keys, func(key string) bool { return key == k })
}

func (o *object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		val, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func parseOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bufio.NewReader(bytes.NewReader(data)))
	dec.UseNumber()
	return parseValue(dec)
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		obj := &object{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			obj.set(k.(string), v)
		}
		_, err := dec.Token()
		return obj, err
	case json.Delim('['):
		list := []any{}
		for dec.More() {
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err := dec.Token()
		return list, err
	}
	return tok, nil
}
