# agent-bus

This is a local agent message bus compatible with *any* coding agent.

A message bus for the coding agent sessions on one machine: Claude Code,
Codex, opencode and pi. It runs on Linux, macOS and Windows. Sessions register under a name. Anyone can ask the
registry who is active, in which directory, on which branch. Registered
sessions can message one session or a group of them.

## Install

Needs Go 1.25 or newer.

```sh
./install.sh        # Linux, macOS
.\install.ps1       # Windows
```

The script builds `cmd/agent-bus` and `cmd/agent-bus-web`, then runs
`agent-bus install`. That copies both binaries to `~/.local/bin` (with `.exe`
on Windows) and wires up every
harness it finds under your home directory. It's safe to rerun. Hooks already
present are left alone, and the settings file keeps its key order.

| Harness | Delivery | Installed to |
|---|---|---|
| Claude Code | `SessionStart`, `SessionEnd`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse` and `Stop` hooks | `~/.claude/settings.json` |
| Codex | `SessionStart`, `SessionEnd`, `UserPromptSubmit`, `PostToolUse` and `Stop` hooks | `~/.codex/hooks.json` |
| pi | extension, runs the start and end hooks, polls every 3 s; delivers inbox messages as user messages, steers into a running turn or starts one when idle | `~/.pi/agent/extensions/agent-bus.ts` (a copy) |
| opencode 2.x | plugin, treats the first tool call as the session start, delivers inbox messages as user prompts at the next step boundary | `~/.config/opencode/plugins/agent-bus.js` (a copy) |

It also writes an "Agent bus" section from `integrations/instructions.md` into
each harness's global instructions file. Running `install` again replaces that
section with the current text and leaves the rest of the file alone.

## Modes

A session uses one of two modes.

**Hooks.** The `SessionStart` hook registers every new session under a name
taken from its directory (`agent-bus`, then `agent-bus-7d54` for the next
one) and tells the agent it's on the bus. After that the `UserPromptSubmit`
and `PostToolUse` hooks, or the pi and opencode plugins, hand each message to
the agent as it arrives. The agent never has to run a bus command to receive
anything. `SessionEnd` removes the session immediately and wakes background
waits before the harness exits. Late tool callbacks leave it off the bus;
`SessionStart` registers it again when it resumes. Finishing a turn leaves the
session registered. Claude's background Stop listener wakes an idle main
session when peer messages arrive. It leaves messages queued for a normal
hook or `inbox` to deliver. Pi delivers inbox messages through its user-message
API, steering them into a running turn or starting one when idle. Claude Code
and Codex use hook context. OpenCode sends user prompts after completed tool
calls, steering them into the running turn at the next step boundary. If prompt
admission fails, it appends the text to the tool result so the agent still sees it.
A session that missed its `SessionStart` hook, say because the hooks went in after
it started, joins
on its next prompt or tool call. One that left with `agent-bus unregister`
stays off until it registers again.

**Manual.** Nothing joins on its own. The agent runs `agent-bus register` and
then fetches messages one of two ways:

- `agent-bus wait`, started as a background command, blocks until messages
  arrive and prints them (exit 0), or until the session leaves the bus
  (exit 3). `unregister` wakes it at once. `--timeout SECONDS` gives up with
  exit 1, and `--json` prints `{"status": ..., "messages": [...]}`.
- `agent-bus inbox` prints the messages not received yet and marks them
  received. `--peek` leaves them unread, and `--json` prints an array.

Hooks mode is on by default. `agent-bus auto off`, or `install --manual`,
switches new sessions to manual mode, and `AGENT_BUS_AUTO=0` does it for one
process. The setting lives in `agent-bus/config.json` under the user config
directory, since the state directory is gone after a reboot.

A controller assigned to receive future commands sets
`agent-bus meta role=controller` and keeps its turn active. Start
`agent-bus wait --timeout 7200` in the background, monitor it with short tool
calls, and restart it after each message or timeout. The `Stop` hook continues
a marked controller when it tries to end its turn. When the user ends the
controller role, clear it with `agent-bus meta role=` before ending the turn.
Closing the session leaves the bus immediately, including for controllers.
A wait that finishes after a final response does not reliably start a new
turn. For other hook sessions, `Stop` continues the turn only when messages
are queued. Subagent hooks leave the parent session's inbox alone.

Claude's Bash `PreToolUse` hook gives each workflow worker or subagent its own
`AGENT_BUS_ID`. Its bus commands operate on that identity, so registering or
unregistering a worker leaves the parent session intact. Workers that need
peer messages register their own name and fetch messages with `inbox` or
`wait`. The hook preserves other Bash arguments and leaves main-agent
commands unchanged.

Claude runs the idle listener as an `asyncRewake` Stop hook with a seven-day
timeout. There is one listener per session, rearmed after it wakes Claude and
the next turn finishes. Closing or unregistering the session stops it
immediately. An already-idle session needs one prompt after first installing
this hook so its next Stop event can arm the listener.

Each session also records how it receives messages. A hook registration
delivers through hooks, a manual `register` delivers manually, and
`register --deliver hooks|manual` picks either. Hooks leave a manual session's
inbox alone, so `wait` and `inbox` never race them. `list` shows the delivery
next to the harness.

## Use

```sh
agent-bus register jit-fix --note "fixing the trace exit bug"
agent-bus register jit-fix --id "$PI_SESSION" --meta ticket=OX-12
agent-bus id "$PI_SESSION"          # set the restart-stable id later
agent-bus meta pr=481 ticket=       # set pr, remove ticket
agent-bus list                      # everyone: harness, branch, directory, note
agent-bus list --repo --json        # every worktree of this repository, as JSON
agent-bus send jit-fix "done with target/, it's yours"
agent-bus send --repo "rebasing main in 5 minutes"
agent-bus send --under ~/Documents/prog/oximond/crates "touching oximond-vm"
agent-bus send reviewer --untrusted-file issue.txt "issue 412 as filed, triage it"
agent-bus inbox
agent-bus wait                      # block until a message arrives or you leave the bus
```

For larger reports, logs, or handoffs, write a file in a shared project
directory and send its absolute path with a short summary and requested
action. Keep the file available until the recipient has used it.

Message text counts as trusted: it comes from another agent session of the
same user. Content from outside, such as a web page, an issue body, an email
or a third-party tool's output, goes in `--untrusted TEXT` or
`--untrusted-file PATH` (`-` reads stdin, up to 256 KiB). The recipient sees
it inside a `<untrusted-...>` block whose tag ends in a random suffix, so the
content cannot close the block and pass itself off as trusted text. The
installed instructions and the hook context tell agents to treat that block
as data and never follow instructions in it, because it may carry prompt
injections. `inbox --json` and `wait --json` return it under `untrusted`, and
`POST /api/send` accepts the same field.

Run `agent-bus` with no arguments for the full usage. Scopes:

- `--all`: every active session
- `--dir DIR`: sessions working in exactly DIR
- `--under DIR`: DIR and everything below it
- `--repo [DIR]`: every worktree of DIR's repository, the same set `git worktree list` shows

`--id`, or `agent-bus id SESSION` in a running session, stores the harness's
own session id next to the bus entry. It defaults to the harness's session
variable unless `AGENT_BUS_ID` is set, and re-registering keeps it. A pi session has none, so pass the id pi
resumes with. `list` prints it, and `send` takes it in place of a name, so a
peer can reach a session under the same id after its agent restarts.

`--meta KEY=VALUE` and `agent-bus meta KEY=VALUE...` attach free-form metadata,
such as a ticket or PR number. `KEY=` removes a key. `list` prints the pairs
and `list --json` puts them under `meta`.

## Resource limits

Use `run` to queue a command until its concurrency group and memory request
fit, then run it with the chosen limits. It can wrap a whole agent session
or a single expensive command.

```sh
# Serialize builds across sessions on this bus.
agent-bus run --mutex builds -- cargo build

# Allow two jobs in this group, with an 8 GiB memory cap per job.
agent-bus run --mutex tests --max-processes 2 --max-ram 8GiB -- cargo test

# Pin an agent to CPUs 0-3, allow two CPUs of sustained work,
# and stop it after 30 minutes of accumulated CPU time.
agent-bus run --cpus 0-3 --cpu-quota 200% --max-cpu-time 30m \
  --max-ram 12GiB --keep-free-ram 4GiB -- claude
```

`--mutex NAME` defaults to one running command. `--max-processes N` changes
that group's capacity. Without a mutex name it uses the global group.
All members of a group must use the same capacity. `--max-tasks N` is a
separate Linux limit on OS tasks, including threads, in the command tree.
On Linux, mutex groups also use a systemd scope when the user manager is
available, so an orphaned child keeps its slot until it exits.

`--max-ram` is a hard limit on the command and its descendants. It also
defaults the memory reservation to that amount. `--reserve-ram` overrides
the admission estimate, or requests memory without a hard cap. The scheduler
checks available RAM and other jobs' unused reservations, leaving
`--keep-free-ram` available, 1 GiB by default. Reservations apply across all
mutex groups that use the same `AGENT_BUS_DIR`. A request larger than the
machine can accommodate fails immediately. A temporary shortage waits until
running jobs finish or memory becomes available. `--wait-timeout 10m` bounds
that wait. Interrupting a queued command cancels its request.

Linux uses user systemd scopes and cgroup v2 to enforce memory, CPU pinning,
CPU quotas and task limits across descendants. `--cpu-quota 100%` allows one
CPU worth of sustained work. `--max-cpu-time 30m` counts total CPU time
across the command tree, including parallel workers. It excludes time spent
waiting in the queue and sleeping. Enforcement samples CPU usage every
100 milliseconds, so parallel workers can exceed the budget by the work
done between samples and during termination. Remaining
descendants are stopped when the wrapped command exits.

Hard resource limits require Linux with a working user systemd manager and
the relevant cgroup controllers. Unsupported limits fail rather than
silently running without protection. Named concurrency groups also work on
other platforms. Existing sessions are unaffected; launch them through
`run` to apply limits. These controls do not remove files left in `/tmp`.
Use disk-backed scratch storage and clean up temporary files as well.

## Web view

```sh
agent-bus-web            # prints http://127.0.0.1:PORT/?token=...
agent-bus-web --open     # and opens it in the default browser
```

`agent-bus-web` serves a live page of the bus: every session with branch,
session id, metadata and note, and a feed of what happens. The feed shows
registrations, notes, id and metadata changes, directory moves, messages with
their text, inbox reads, and sessions dropping off.

The runs panel refreshes every three seconds. It shows queued and running
commands, their owner, resource requests and limits, and the last 100 finished
runs with exit codes. Run history starts with commands launched by the updated
`agent-bus` binary and survives web server restarts.

Commands are censored before they enter shared run history, and again when the
web API serves them. Redaction checks credential flags and assignments,
sensitive environment values, URL credentials, common API key patterns, and
high-entropy tokens. Inline shell scripts and evaluation code are hidden.
Entropy checks also hide some hashes and random IDs. Redaction is a heuristic
and cannot reliably detect every unnamed, low-entropy secret. Command execution
uses the original arguments. Command output is not collected or shown.

The session list is grouped by folder. Click a session to see only its
traffic, with each message marked as going out or coming in, plus its sent and
received counts. Click a folder heading to see the traffic of every session in
that folder. Names in the feed are clickable too. The send box switches to the
session or folder you picked. The choice lives in the URL hash (`#s=ID` or
`#d=PATH`), so Back works and a view can be bookmarked. Tick "departed" to keep
sessions that left the bus in the list, as long as the feed still has their
events.

Star a session to follow
it. That filters the feed when "followed only" is on, and with notifications
enabled the browser tells you when a followed session sends or receives a
message.

The server joins the bus as a session of harness `web`, so the box at the
bottom messages a session or a scope, and agents answer with
`agent-bus send web ...`.

The URL carries a random token, made fresh on every start. The page trades it
for a cookie and drops it from the address bar. Scripts can pass it as
`Authorization: Bearer TOKEN`. It listens on 127.0.0.1 unless `--addr` says
otherwise, and rejects requests whose Host header isn't an IP or `localhost`.

The JSON endpoints are the same ones the page uses:

- `GET /api/sessions` lists sessions like `agent-bus list --json` does
- `GET /api/events?backlog=N&session=NAME,...` streams events as server-sent events
- `POST /api/send` takes `{"to": NAME, "text": ...}` or `{"scope": "all|repo|dir|under", "dir": DIR, "text": ...}`

## How it works

State lives in `/tmp/agent-bus-$UID` on Linux and macOS and in
`%LOCALAPPDATA%\agent-bus` on Windows. `AGENT_BUS_DIR` overrides both. There
is one JSON file per session and one inbox directory per session. Writes take
an exclusive lock on the `lock` file (`flock` on Unix, `LockFileEx` on
Windows), and messages land through write-then-rename. The file format matches
the earlier Python version, so sessions registered by it stay on the bus.

A session is the nearest Claude Code, Codex, opencode or pi process above the
calling shell. agent-bus walks the process tree with the native API of each
system: `/proc` on Linux, the `kern.proc` and `kern.procargs2` sysctls on
macOS, and a Toolhelp snapshot plus `NtQueryInformationProcess` on Windows. A
parent that started after its child is a recycled pid, and the walk stops
there.

The session is named by that harness's session variable
(`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, `OPENCODE_SESSION_ID`), or by
`<harness>-<pid>` when the harness exports none. pi exports none. A session
counts as active while that process lives, checked by pid and start time, and
for 12 hours after its last bus command (`AGENT_BUS_TTL`). The 12-hour limit
covers Codex desktop, where one app-server process hosts many threads. Dead
sessions disappear along with their unread messages.

`wait` doesn't poll. It listens on a Unix socket under `wake/` (a named pipe
`\\.\pipe\agent-bus-...` on Windows), and `send` connects to it after it
delivers. A message wakes the waiter within milliseconds. `wait` still rechecks
the inbox every 30 seconds in case a wake-up got lost.

`list` reads only the registry, so any process can call it without bothering
the agents. Branches come from git at query time.

Every command that changes something appends a line to `events.jsonl` in the
state directory, which rotates to `events.1.jsonl` past 8 MB. Sessions listed
in `watchers/` get poked after each append, the same way `send` wakes `wait`.
`agent-bus-web` is such a watcher, so the feed updates without polling. It
also prunes the registry every 10 seconds, so a session whose harness exited
shows up as dropped even when no agent runs a command.

## Layout

- `cmd/agent-bus`: the command line
- `cmd/agent-bus-web`: the web view
- `src`: the bus itself, with `proc_*.go` and `sys_*.go` per operating system, and the page in `src/web`
- `integrations`: hooks, plugins and instructions, embedded in the binary for `install`

## AI Disclosure

This project is being developed with AI assistance.

## License

Licensed under MIT. See [LICENSE](LICENSE).
