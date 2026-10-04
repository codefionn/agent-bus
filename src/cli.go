// Package bus is a message bus between the agent sessions on one machine.
package bus

import (
	"fmt"
	"os"
	"strings"
)

const usage = `agent-bus register [NAME] [--deliver hooks|manual] [--id SESSION] [--meta KEY=VALUE]... [--note TEXT]
                                          join the bus, or move to the current directory
agent-bus note TEXT                       update your status line
agent-bus id [SESSION]                    print or set the harness's session id for this session
agent-bus meta [KEY=VALUE]...             print or set metadata; KEY= removes KEY
agent-bus list [SCOPE] [--json]           active sessions with directory and branch
agent-bus send (NAME | SCOPE) MESSAGE...  message one session or every session in SCOPE
agent-bus inbox [--peek] [--json]         print the messages not received yet
agent-bus wait [--timeout SECONDS] [--json]
                                          block until messages arrive (exit 0), the session
                                          leaves the bus (exit 3) or the timeout passes (exit 1)
agent-bus auto [on | off]                 show or set whether hooks register new sessions
agent-bus whoami
agent-bus unregister
agent-bus hook                            hook and plugin entry (reads a Claude Code hook payload)
agent-bus install [--auto | --manual]     copy agent-bus to ~/.local/bin and wire up every harness found

Two ways to use the bus:

    hooks    The SessionStart hook registers every new session and the
             harness hook or plugin hands messages to the agent as they come.
             SessionEnd takes the session off again. On while auto is on.
    manual   The agent runs agent-bus register itself. It receives messages
             by running agent-bus wait in the background, which returns with
             the next messages or when the session leaves, or by asking
             agent-bus inbox for the ones it has not received yet.

A session registered by its hook delivers through hooks; one registered with
agent-bus register delivers manually unless it passes --deliver hooks.

SCOPE selects sessions by their working directory:

    --all          every active session
    --dir DIR      sessions working in exactly DIR
    --under DIR    sessions working in DIR or below it
    --repo [DIR]   sessions in any worktree of DIR's repository (one ` + "`git worktree list`" + `);
                   DIR defaults to the current directory

` + "`list`" + ` works from any process, registered or not, and reads only the registry.
It never asks the sessions themselves. ` + "`list --json`" + ` is the stable form for
scripts. Branches come from git at list time, so they stay current.

Works from Claude Code, Codex, opencode and pi, or any shell that runs inside
one of them, on Linux, macOS and Windows. A session is identified by the
harness's own session id when it exports one (CLAUDE_CODE_SESSION_ID,
CODEX_THREAD_ID, OPENCODE_SESSION_ID), otherwise by the harness process.
AGENT_BUS_ID overrides both, but the session still needs a harness ancestor to
tie its lifetime to.

--id (or ` + "`agent-bus id SESSION`" + ` later) records the harness's own session id,
the one it keeps when an agent is resumed after a restart. It defaults to the
session variable above. ` + "`list`" + `
shows it and ` + "`send`" + ` accepts it in place of NAME, so peers can address a
session the same way before and after a restart. Metadata is free-form
KEY=VALUE pairs a session publishes about itself, such as a task or ticket;
` + "`list`" + ` shows them and ` + "`list --json`" + ` has them under "meta".

Only registered sessions whose harness process is still alive can send or
receive. Sessions whose harness exited, or that have not used the bus for
AGENT_BUS_TTL seconds (default 12h), drop off the bus with their unread
messages. State lives in AGENT_BUS_DIR (default /tmp/agent-bus-$UID, or
%LOCALAPPDATA%\agent-bus on Windows).`

var commands = map[string]func([]string){
	"register":   cmdRegister,
	"unregister": cmdUnregister,
	"note":       cmdNote,
	"auto":       cmdAuto,
	"id":         cmdID,
	"meta":       cmdMeta,
	"whoami":     cmdWhoami,
	"list":       cmdList,
	"send":       cmdSend,
	"inbox":      cmdInbox,
	"wait":       cmdWait,
	"hook":       cmdHook,
	"install":    cmdInstall,
}

func die(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agent-bus: "+format+"\n", args...)
	os.Exit(code)
}

// Main runs the command line in args, without the program name.
func Main(args []string) {
	if len(args) < 1 || commands[args[0]] == nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(usage))
		os.Exit(2)
	}
	if args[0] != "install" {
		setup()
	}
	commands[args[0]](args[1:])
}
