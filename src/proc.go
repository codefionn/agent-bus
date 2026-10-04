package bus

import (
	"os"
	"strings"
)

// proc is what agent-bus needs to know about one process.
type proc struct {
	pid, ppid int
	start     uint64 // OS-specific start time; only compared with itself, so a recycled pid is not mistaken for the owner
	name      string
	args      []string
}

var harnessNames = map[string]string{"claude": "claude", "codex": "codex", "opencode": "opencode", ".opencode": "opencode", "pi": "pi"}

func baseName(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		path = path[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(path), ".exe")
}

// harnessOf names the agent harness p runs, or returns "".
func harnessOf(p *proc) string {
	names := []string{baseName(p.name)}
	if len(p.args) > 0 {
		// macOS reports the name of the file a symlink resolves to, such as a version number.
		names = append(names, baseName(p.args[0]))
	}
	for _, n := range names {
		if h := harnessNames[n]; h != "" {
			return h
		}
	}
	if names[0] != "node" && names[0] != "bun" {
		return ""
	}
	for _, a := range p.args[1:min(3, len(p.args))] {
		a = strings.ReplaceAll(a, `\`, "/")
		switch {
		case strings.Contains(a, "pi-coding-agent") || baseName(a) == "pi":
			return "pi"
		case strings.Contains(a, "@anthropic-ai/claude-code"):
			return "claude"
		}
	}
	return ""
}

// findHarness returns the nearest ancestor that is an agent harness.
func findHarness() (string, *proc) {
	child, _ := procInfo(os.Getpid())
	pid := os.Getppid()
	for pid > 1 {
		p, ok := procInfo(pid)
		if !ok {
			break
		}
		// A parent younger than its child means the real parent exited and its pid was reused.
		if child != nil && child.start != 0 && p.start > child.start {
			break
		}
		if h := harnessOf(p); h != "" {
			return h, p
		}
		if p.ppid == pid {
			break
		}
		child, pid = p, p.ppid
	}
	return "", nil
}
