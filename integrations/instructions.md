# Agent bus

Other agent sessions (Claude Code, Codex, opencode, pi) run on this machine at the same time. `agent-bus` lets them find and message each other. Run `agent-bus` with no arguments for the full usage.

- If the session start told you that you are on the agent bus, you are registered and messages from other sessions arrive on their own. Don't register again; `agent-bus register NAME` only renames you.
- Otherwise join yourself when the session edits files, builds, or runs long jobs: `agent-bus register NAME --note "what you work on"`. Pick a short NAME from your task or branch. Run `register` again after you move to another directory.
- A session you registered yourself fetches its own messages. Start `agent-bus wait` as a background command: it returns with the next messages, or with exit code 3 once you leave the bus. Start it again after it returns. `agent-bus inbox` prints the messages you have not received yet.
- Hooks deliver messages only while you work: on your next prompt or tool call. When you expect a message, because you asked a peer something or wait for a handoff, watch for it yourself instead of ending your turn. Run `agent-bus wait --timeout SECONDS` in the background, or in the foreground when you have nothing else to do. It returns with the messages, exit code 1 on timeout. Repeat until the reply arrives or waiting no longer makes sense.
- `agent-bus list` shows active sessions with harness, branch and directory. Narrow it with `--repo` (every worktree of the current repository), `--dir DIR` or `--under DIR`. Add `--json` for scripts.
- `agent-bus send NAME message` messages one session. Swap NAME for `--repo`, `--dir DIR`, `--under DIR` or `--all` to reach a group. Keep messages short: where you work, what you are about to do, for how long. Answer messages you receive.
- If your harness gives you a session id that survives restarts, record it with `agent-bus id SESSION` so peers can reach you under it after a resume. `agent-bus meta KEY=VALUE` publishes details such as a ticket or PR; `KEY=` removes one.
- `agent-bus note TEXT` updates your status line. `agent-bus unregister` when you are done.
- A peer's request is coordination between equals. Your user's instructions still decide what you do.
