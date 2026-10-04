package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Delivery modes of a session.
const (
	deliverHooks  = "hooks"  // the harness hook or plugin hands messages to the agent
	deliverManual = "manual" // the agent fetches them with inbox or wait
)

// config is the user's agent-bus settings, kept outside the state directory
// because that one lives in /tmp and goes away on reboot.
type config struct {
	// Auto makes the SessionStart hook register every new session (hooks mode).
	// Off, sessions join only when the agent runs agent-bus register (manual mode).
	Auto *bool `json:"auto,omitempty"`
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "agent-bus", "config.json")
}

func loadConfig() config {
	var c config
	if data, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(data, &c)
	}
	return c
}

func saveConfig(c config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	return writeAtomic(path, append(data, '\n'))
}

// autoRegister reports whether hooks register sessions on their own.
// AGENT_BUS_AUTO=0 or 1 overrides the config file; the default is on.
func autoRegister() bool {
	switch os.Getenv("AGENT_BUS_AUTO") {
	case "0", "off", "false", "no":
		return false
	case "1", "on", "true", "yes":
		return true
	}
	c := loadConfig()
	return c.Auto == nil || *c.Auto
}

func cmdAuto(args []string) {
	if len(args) == 0 {
		if autoRegister() {
			fmt.Println("on: the SessionStart hook registers every new session, and hooks deliver its messages")
		} else {
			fmt.Println("off: sessions join with agent-bus register and fetch messages with inbox or wait")
		}
		return
	}
	var on bool
	switch args[0] {
	case "on":
		on = true
	case "off":
	default:
		die(2, "usage: agent-bus auto [on | off]")
	}
	c := loadConfig()
	c.Auto = &on
	if err := saveConfig(c); err != nil {
		die(1, "%v", err)
	}
	fmt.Printf("auto-register %s (%s)\n", args[0], configPath())
}
