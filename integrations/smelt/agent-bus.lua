-- agent-bus for Smelt. Install this file in ~/.config/smelt/plugins/.
-- The hook owns registration and respects each session's delivery setting.

smelt.plugin("agent-bus")

local bus = smelt.os.getenv("AGENT_BUS")
  or (smelt.os.home() .. "/.local/bin/agent-bus"
    .. (smelt.os.platform() == "windows" and ".exe" or ""))
local session_id
local session_cwd
local pending = false
local closing = false
local late_messages = {}

local function deliver_late()
  if not session_id or closing then return end
  for _, item in ipairs(late_messages) do
    smelt.engine.submit_command("agent-bus",
      "Message received for previous Smelt session " .. item.id .. ":\n" .. item.text,
      nil, "agent-bus message")
  end
  late_messages = {}
end

local function hook(event, id, cwd)
  local input = smelt.json.encode({
    hook_event_name = event,
    session_id = id,
    cwd = cwd,
  })
  local result = smelt.process.run(bus, { "hook" }, {
    stdin = input,
    env = { SMELT_SESSION_ID = id },
    cwd = cwd,
    timeout_secs = 5,
    max_output_bytes = 1024 * 1024,
  })
  if not result or result.exit_code ~= 0 or result.timed_out then return nil end
  if not result.stdout or result.stdout == "" then return nil end
  local decoded, parsed = pcall(smelt.json.decode, result.stdout)
  if not decoded or type(parsed) ~= "table" then return nil end
  local output = parsed.hookSpecificOutput
  if type(output) ~= "table" then return nil end
  local message = output.additionalContext
  if type(message) == "string" and message ~= "" then return message end
end

local function current_id()
  return smelt.session.id()
end

local function start_session()
  local id = current_id()
  if not id or id == "" then return end
  session_id = id
  smelt.os.setenv("SMELT_SESSION_ID", id)
  local cwd = smelt.session.cwd()
  session_cwd = cwd
  smelt.spawn(function()
    local intro = hook("SessionStart", id, cwd)
    if intro and session_id == id and current_id() == id then
      -- Keep registration guidance for the next user turn without starting
      -- an unsolicited model request at launch.
      smelt.session.context_note("agent-bus", intro)
    end
  end)
  deliver_late()
end

local function end_session(id)
  if not id or id == "" then return end
  local cwd = session_cwd
  smelt.spawn(function() hook("SessionEnd", id, cwd) end)
end

smelt.events.on("session_started", function(id)
  if id ~= session_id then start_session() end
end)

smelt.events.on("session_ended", function(id)
  if id == session_id then
    smelt.session.context_note("agent-bus", nil)
    session_id = nil
  end
  end_session(id)
end)

smelt.events.on("turn_complete", function()
  smelt.session.context_note("agent-bus", nil)
end)

smelt.tick.every(3, function()
  if pending or closing or not session_id then return end
  pending = true
  local id = session_id
  local cwd = smelt.session.cwd()
  session_cwd = cwd
  local ok, message = pcall(hook, "PostToolUse", id, cwd)
  pending = false
  if not ok or not message then return end
  if session_id ~= id or current_id() ~= id then
    table.insert(late_messages, { id = id, text = message })
    deliver_late()
    return
  end
  -- Smelt queues this command behind an active turn and starts it immediately
  -- while idle. The hook has already removed the delivered inbox messages.
  smelt.engine.submit_command("agent-bus", message, nil, "agent-bus message")
end)

smelt.lifecycle.on_ready(function()
  if not session_id then start_session() end
end)

smelt.lifecycle.on_shutdown(function(ctx)
  closing = true
  end_session(ctx.session_id or session_id)
end)
