local events, lifecycle, calls, commands, notes, env = {}, {}, {}, {}, {}, {}
local current = "session-one"
local response
local run_result
local decode_raises = false
local on_run

smelt = {
  plugin = function(name) assert(name == "agent-bus") end,
  os = {
    getenv = function(name) return env[name] end,
    setenv = function(name, value) env[name] = value end,
    home = function() return "/home/test" end,
    platform = function() return "linux" end,
  },
  session = {
    id = function() return current end,
    cwd = function() return "/work" end,
    context_note = function(name, value) notes[name] = value end,
  },
  json = {
    encode = function(value) return value end,
    decode = function()
      if decode_raises then error("bad JSON") end
      return response
    end,
  },
  process = {
    run = function(path, args, opts)
      table.insert(calls, { path = path, args = args, opts = opts })
      if on_run then on_run(opts.stdin.hook_event_name) end
      return run_result or { stdout = "hook output", exit_code = 0, timed_out = false }
    end,
  },
  engine = {
    submit_command = function(name, body, _, display)
      table.insert(commands, { name = name, body = body, display = display })
    end,
  },
  events = { on = function(name, fn) events[name] = fn end },
  tick = { every = function(_, fn) events.tick = fn end },
  lifecycle = {
    on_ready = function(fn) lifecycle.ready = fn end,
    on_shutdown = function(fn) lifecycle.shutdown = fn end,
  },
  spawn = function(fn) fn() end,
}

dofile("integrations/smelt/agent-bus.lua")
response = { hookSpecificOutput = { additionalContext = "bus intro" } }
lifecycle.ready()
assert(calls[1].opts.stdin.hook_event_name == "SessionStart")
assert(calls[1].opts.stdin.session_id == current)
assert(calls[1].opts.env.SMELT_SESSION_ID == current)
assert(env.SMELT_SESSION_ID == current)
assert(notes["agent-bus"] == "bus intro")
assert(#commands == 0)

response = { hookSpecificOutput = { additionalContext = "peer message" } }
events.tick()
assert(calls[2].opts.stdin.hook_event_name == "PostToolUse")
assert(commands[1].body == "peer message")
assert(commands[1].name == "agent-bus")

response = { hookSpecificOutput = {} }
events.tick()
assert(#commands == 1)

response = { hookSpecificOutput = { additionalContext = "must not deliver" } }
run_result = { stdout = "", exit_code = 0, timed_out = false }
events.tick()
assert(#commands == 1)
run_result = { stdout = "hook output", exit_code = 1, timed_out = false }
events.tick()
assert(#commands == 1)
run_result = { stdout = "hook output", exit_code = 0, timed_out = true }
events.tick()
assert(#commands == 1)
run_result = nil
decode_raises = true
events.tick()
assert(#commands == 1)
decode_raises = false

events.turn_complete()
assert(notes["agent-bus"] == nil)
events.session_ended("session-one")
assert(calls[#calls].opts.stdin.hook_event_name == "SessionEnd")
current = "session-two"
response = { hookSpecificOutput = { additionalContext = "new intro" } }
events.session_started(current)
assert(env.SMELT_SESSION_ID == current)
assert(notes["agent-bus"] == "new intro")
assert(calls[#calls].opts.stdin.session_id == current)

response = { hookSpecificOutput = { additionalContext = "late peer message" } }
on_run = function(event)
  if event ~= "PostToolUse" then return end
  on_run = nil
  events.session_ended("session-two")
  current = "session-three"
  events.session_started(current)
end
events.tick()
assert(commands[2].body ==
  "Message received for previous Smelt session session-two:\nlate peer message")
assert(calls[#calls].opts.stdin.session_id == "session-three")

lifecycle.shutdown({ session_id = current })
assert(calls[#calls].opts.stdin.hook_event_name == "SessionEnd")
local after_shutdown = #calls
events.tick()
assert(#calls == after_shutdown)

smelt.os.platform = function() return "windows" end
response = { hookSpecificOutput = {} }
dofile("integrations/smelt/agent-bus.lua")
lifecycle.ready()
assert(calls[#calls].path == "/home/test/.local/bin/agent-bus.exe")

env.AGENT_BUS = "C:/custom/agent-bus.exe"
dofile("integrations/smelt/agent-bus.lua")
lifecycle.ready()
assert(calls[#calls].path == env.AGENT_BUS)

print("smelt plugin contracts passed")
