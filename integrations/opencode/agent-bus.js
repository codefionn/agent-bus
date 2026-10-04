// agent-bus for opencode 2.x: appends messages from other agent sessions to
// tool results, the same way the Claude Code and Codex hooks do. The first
// tool call of a session counts as its start, which registers it when
// agent-bus auto-register is on.

import { execFileSync } from "node:child_process"
import { homedir } from "node:os"
import { join } from "node:path"

const AGENT_BUS = process.env.AGENT_BUS ?? join(homedir(), ".local", "bin", process.platform === "win32" ? "agent-bus.exe" : "agent-bus")

const started = new Set()

function poll(sessionID) {
  const event = started.has(sessionID) ? "PostToolUse" : "SessionStart"
  started.add(sessionID)
  try {
    const out = execFileSync(AGENT_BUS, ["hook"], {
      input: JSON.stringify({ hook_event_name: event, cwd: process.cwd() }),
      // The bash tool exports OPENCODE_SESSION_ID; match it so both sides name the same session.
      env: { ...process.env, OPENCODE_SESSION_ID: sessionID },
      timeout: 5000,
      encoding: "utf8",
    })
    return out.trim() ? JSON.parse(out).hookSpecificOutput?.additionalContext : undefined
  } catch {
    return undefined
  }
}

export default {
  id: "agent-bus",
  setup: async (api) => {
    await api.tool.hook("execute.after", (call) => {
      if (call.status !== "completed" || !call.sessionID) return
      const text = poll(call.sessionID)
      if (!text) return
      const result = call.result
      if (typeof result.output === "string") result.output = result.output ? `${result.output}\n\n${text}` : text
      else if (Array.isArray(result.content)) result.content.push({ type: "text", text })
    })
  },
}
