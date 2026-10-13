// Drives the shipped opencode plugin modules (workspace/opencode-plugin) with fake v1 and v2
// hosts. Run by plugin_modules_test.go; any failed assertion exits non-zero.
//
//   node --experimental-strip-types plugin_modules.mjs <plugin dir> <fake rtk dir> <fake agent log>
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const [dir, , agentLog] = process.argv.slice(2);
const load = async (f) => (await import(pathToFileURL(join(dir, f)).href)).default;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const until = async (cond, what) => {
  for (let i = 0; i < 100; i++) {
    if (cond()) return;
    await sleep(50);
  }
  assert.fail("timed out waiting for " + what);
};

// A fake opencode 2 context: records the tool hook and exposes an event stream the test feeds.
const v2Host = (sessions = {}) => {
  const hooks = {};
  const queue = [];
  let wake = () => {};
  return {
    hooks,
    push: (...events) => {
      queue.push(...events);
      wake();
    },
    ctx: {
      tool: { hook: async (name, cb) => void (hooks[name] = cb) },
      // Like ctx.session.get: the session's info, a rejection for one that does not exist.
      session: {
        get: async ({ sessionID }) => {
          if (!(sessionID in sessions)) throw new Error("Session.NotFoundError");
          return { id: sessionID, ...sessions[sessionID] };
        },
      },
      event: {
        subscribe: ({ signal }) => (async function* () {
          while (!signal.aborted) {
            if (queue.length) yield queue.shift();
            else await new Promise((r) => ((wake = r), setTimeout(r, 20)));
          }
        })(),
      },
    },
  };
};

const mods = { caller: await load("agent-fleet-caller.js"), status: await load("agent-fleet-status.js"), rtk: await load("rtk.ts") };

// Shape: one module that both majors accept. v2 reads id + setup, v1 calls server().
const ids = new Set();
for (const [name, m] of Object.entries(mods)) {
  assert.equal(typeof m.id, "string", name + ": v2 needs a stable id");
  assert.equal(typeof m.setup, "function", name + ": v2 needs setup()");
  assert.equal(typeof m.server, "function", name + ": v1 (1.18.29+) needs server()");
  ids.add(m.id);
}
assert.equal(ids.size, 3, "plugin ids must be distinct");

// caller: stamps af's tools only, overwrites a forged value, on both majors.
{
  const afTool = "af_0123abcd_request_browser_action";
  const run = async (stampOn) => {
    const af = { x: "1", _af_caller_sid: "ses_FORGED" };
    await stampOn(afTool, "ses_real", af);
    assert.equal(af._af_caller_sid, "ses_real", "forged value must be overwritten");
    assert.equal(af.x, "1");
    for (const tool of ["other_probe_tool", "af_probe", "af_0123abcd", "shell", ""]) {
      const args = { x: "1" };
      await stampOn(tool, "ses_real", args);
      assert.deepEqual(args, { x: "1" }, tool + " must not be stamped");
    }
    const noSession = { x: "1" };
    await stampOn(afTool, undefined, noSession);
    assert.deepEqual(noSession, { x: "1" });
  };
  const h = v2Host();
  await mods.caller.setup(h.ctx);
  await run(async (tool, sessionID, input) => h.hooks["execute.before"]({ tool, sessionID, input }));
  const v1 = await mods.caller.server();
  await run(async (tool, sessionID, args) => v1["tool.execute.before"]({ tool, sessionID }, { args }));
}

// rtk: `rtk rewrite` exits non-zero when it rewrites (measured), and the plugin must still use it.
{
  const rewritten = async (tool, command, hook) => {
    const args = { command };
    await hook(tool, args);
    return args.command;
  };
  const h = v2Host();
  await mods.rtk.setup(h.ctx);
  assert.ok(h.hooks["execute.before"], "rtk on PATH: the v2 hook must be registered");
  const v2 = (tool, args) => h.hooks["execute.before"]({ tool, input: args });
  assert.equal(await rewritten("shell", "git status", v2), "rtk git status");
  assert.equal(await rewritten("bash", "git status", v2), "rtk git status");
  assert.equal(await rewritten("read", "git status", v2), "git status");
  assert.equal(await rewritten("shell", "unchanged", v2), "unchanged");
  const v1h = await mods.rtk.server();
  const v1 = (tool, args) => v1h["tool.execute.before"]({ tool }, { args });
  assert.equal(await rewritten("bash", "git status", v1), "rtk git status");
  assert.equal(await rewritten("read", "git status", v1), "git status");

  const saved = process.env.PATH;
  process.env.PATH = mkdtempSync(join(tmpdir(), "no-rtk-"));
  try {
    const off = v2Host();
    await mods.rtk.setup(off.ctx);
    assert.equal(off.hooks["execute.before"], undefined, "no rtk: the v2 hook must not be registered");
    assert.deepEqual(await mods.rtk.server(), {}, "no rtk: v1 hooks must be empty");
  } finally {
    process.env.PATH = saved;
  }
}

// status: states reported in order, ids recorded, subagent sessions left out.
{
  const states = () => (existsSync(agentLog) ? readFileSync(agentLog, "utf8").trim().split("\n").filter(Boolean) : []);
  const ocid = () => readFileSync(join(process.env.HOME, ".config/agent-fleet/opencode-sid", process.env.AF_SESSION_SID), "utf8");
  const ev = (type, data) => ({ type, data });
  const h = v2Host();
  const cleanup = mods.status.setup(h.ctx);
  assert.equal(typeof cleanup, "function", "v2 setup must return its cleanup");
  const settle = async (n, what) => {
    await until(() => states().length >= n, what);
    await sleep(200);
  };
  const line = (s) => `session-status ${s} sid-test`;

  h.push(
    ev("session.created", { sessionID: "ses_parent" }),
    ev("session.created", { sessionID: "ses_child", parentID: "ses_parent" }),
    ev("session.execution.started", { sessionID: "ses_parent" }),
    ev("session.execution.started", { sessionID: "ses_child" }),
    ev("session.execution.succeeded", { sessionID: "ses_child" }),
  );
  await settle(1, "working");
  assert.deepEqual(states(), [line("working")], "a subagent finishing must not flip the badge to idle");
  assert.equal(ocid(), "ses_parent", "the slot must stay pinned to the parent, never a subagent");

  h.push(
    ev("session.execution.succeeded", { sessionID: "ses_parent" }),
    ev("session.execution.started", { sessionID: "ses_parent" }),
    ev("session.execution.interrupted", { sessionID: "ses_parent", reason: "user" }),
    ev("session.execution.started", { sessionID: "ses_parent" }),
    ev("session.execution.failed", { sessionID: "ses_parent" }),
  );
  await settle(6, "five more transitions");
  assert.deepEqual(states(), "working idle working idle working idle".split(" ").map(line));
  cleanup();

  // The stream is live-only: a subagent resumed after a restart never shows its session.created.
  // It must still be recognised (ctx.session.get), and a lookup that fails must change nothing.
  {
    const before = states().length;
    const h2 = v2Host({ ses_root: {}, ses_old_child: { parentID: "ses_root" } });
    const stop = mods.status.setup(h2.ctx);
    h2.push(
      ev("session.execution.started", { sessionID: "ses_root" }),
      ev("session.execution.started", { sessionID: "ses_old_child" }),
      ev("session.execution.succeeded", { sessionID: "ses_old_child" }),
      ev("session.execution.started", { sessionID: "ses_unknown" }),
      ev("session.execution.succeeded", { sessionID: "ses_unknown" }),
    );
    await settle(before + 1, "working");
    assert.deepEqual(states().slice(before), [line("working")], "an unseen subagent must not flip the badge");
    assert.equal(ocid(), "ses_root", "an unseen subagent must not take over the slot");
    stop();
  }

  // v1: idle on session.idle, working on message.*, a trailing message.* right after idle ignored.
  const before = states().length;
  const v1 = await mods.status.server();
  await v1.event({ event: { type: "message.updated", properties: { info: { sessionID: "ses_v1" } } } });
  await v1.event({ event: { type: "session.idle", properties: { sessionID: "ses_v1" } } });
  await v1.event({ event: { type: "message.updated", properties: { info: { sessionID: "ses_v1" } } } });
  await until(() => states().length >= before + 2, "two v1 transitions");
  await sleep(200);
  assert.deepEqual(states().slice(before), ["session-status working sid-test", "session-status idle sid-test"]);
  assert.equal(ocid(), "ses_v1");
}
console.log("ok");
