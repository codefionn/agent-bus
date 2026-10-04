/**
 * agent-bus for pi: delivers messages from other agent sessions.
 *
 * On session start it runs the SessionStart hook, which registers the session
 * when agent-bus auto-register is on; shutdown takes it off the bus again.
 * Then it polls `agent-bus hook` every few seconds. A message that arrives
 * while the agent works is steered into the running turn; one that arrives
 * while pi is idle waits for the next prompt and shows a notification.
 */

import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { execFile } from "node:child_process";
import { homedir } from "node:os";
import { join } from "node:path";

const AGENT_BUS = process.env.AGENT_BUS ?? join(homedir(), ".local", "bin", process.platform === "win32" ? "agent-bus.exe" : "agent-bus");
const POLL_MS = 3000;

function poll(cwd: string, event = "PostToolUse"): Promise<string | undefined> {
  return new Promise((resolve) => {
    const child = execFile(AGENT_BUS, ["hook"], { timeout: 5000 }, (err, stdout) => {
      if (err || !stdout.trim()) return resolve(undefined);
      try {
        resolve(JSON.parse(stdout).hookSpecificOutput?.additionalContext);
      } catch {
        resolve(undefined);
      }
    });
    child.on("error", () => resolve(undefined));
    child.stdin?.end(JSON.stringify({ hook_event_name: event, cwd }));
  });
}

export default function (pi: ExtensionAPI) {
  let ctx: ExtensionContext | undefined;
  let timer: ReturnType<typeof setInterval> | undefined;
  let busy = false;

  pi.on("session_start", async (_event, c) => {
    ctx = c;
    const intro = await poll(c.cwd, "SessionStart");
    if (intro) pi.sendMessage({ customType: "agent-bus", content: intro, display: true }, { deliverAs: "nextTurn" });
    if (timer) return;
    timer = setInterval(async () => {
      if (busy || !ctx) return;
      busy = true;
      try {
        const text = await poll(ctx.cwd);
        if (!text) return;
        const idle = ctx.isIdle();
        pi.sendMessage(
          { customType: "agent-bus", content: text, display: true },
          { deliverAs: idle ? "nextTurn" : "steer" },
        );
        if (idle) ctx.ui.notify("agent-bus: new message from another session", "info");
      } finally {
        busy = false;
      }
    }, POLL_MS);
  });

  pi.on("session_shutdown", async () => {
    if (timer) clearInterval(timer);
    timer = undefined;
    if (ctx) await poll(ctx.cwd, "SessionEnd");
  });
}
