// agent-bus for opencode 2.x: delivers messages from other agent sessions as
// user prompts, steering them into the running turn at the next step boundary.
// The first tool call registers the session when agent-bus auto-register is on.

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
    await api.tool.hook("execute.after", async (call) => {
      if (call.status !== "completed" || !call.sessionID) return
      const text = poll(call.sessionID)
      if (!text) return
      try {
        // prompt returns the admitted inbox item without waiting for generation.
        await api.session.prompt({ sessionID: call.sessionID, text, delivery: "steer" })
        return
      } catch {
        // The hook consumed these messages. Keep them visible if prompt admission fails.
      }
      const result = call.result
      if (typeof result.output === "string") result.output = result.output ? `${result.output}\n\n${text}` : text
      else if (Array.isArray(result.content)) result.content.push({ type: "text", text })
    })
  },
}
