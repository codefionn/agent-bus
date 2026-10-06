import assert from "node:assert/strict"
import childProcess from "node:child_process"
import { syncBuiltinESMExports } from "node:module"
import { mock, test } from "node:test"

let output = ""
const polls = []
mock.method(childProcess, "execFileSync", (_file, _args, options) => {
  polls.push(options)
  return output
})
syncBuiltinESMExports()
const { default: plugin } = await import("./agent-bus.js")

async function setup(prompt) {
  let callback
  await plugin.setup({
    tool: { hook: async (event, handler) => {
      assert.equal(event, "execute.after")
      callback = handler
    } },
    session: { prompt },
  })
  return callback
}

function hookOutput(text) {
  return JSON.stringify({ hookSpecificOutput: { additionalContext: text } })
}

test("delivers peer text as a user prompt without changing the tool result", async () => {
  const prompts = []
  const receive = await setup(async (input) => { prompts.push(input) })
  const text = '[12:00:00] peer (pi, /work): review this\n<untrusted-123456abcdef>outside content</untrusted-123456abcdef>'
  output = hookOutput(text)
  const result = { output: "original tool output" }
  await receive({ status: "completed", sessionID: "user-prompt", result })
  assert.deepEqual(prompts, [{ sessionID: "user-prompt", text, delivery: "steer" }])
  assert.deepEqual(result, { output: "original tool output" })
  assert.equal(JSON.parse(polls.at(-1).input).hook_event_name, "SessionStart")
  assert.equal(polls.at(-1).env.OPENCODE_SESSION_ID, "user-prompt")

  output = ""
  await receive({ status: "completed", sessionID: "user-prompt", result })
  assert.equal(prompts.length, 1)
  assert.equal(JSON.parse(polls.at(-1).input).hook_event_name, "PostToolUse")
  const count = polls.length
  await receive({ status: "failed", sessionID: "user-prompt", result })
  await receive({ status: "completed", result })
  assert.equal(polls.length, count)
})

test("keeps consumed inbox text visible when prompt admission fails", async () => {
  const receive = await setup(async () => { throw new Error("session unavailable") })
  output = hookOutput("peer message")
  for (const original of ["original tool output", ""]) {
    const result = { output: original }
    await receive({ status: "completed", sessionID: "fallback", result })
    assert.equal(result.output, original ? `${original}\n\npeer message` : "peer message")
  }
  const result = { content: [{ type: "text", text: "original tool output" }] }
  await receive({ status: "completed", sessionID: "fallback", result })
  assert.deepEqual(result.content, [
    { type: "text", text: "original tool output" },
    { type: "text", text: "peer message" },
  ])
})
