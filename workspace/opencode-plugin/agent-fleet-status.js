// agent-fleet status bridge for opencode.
//
// Mirrors what claude does via settings.json hooks: report the session's live
// working/idle state to the Workspace Agent so the Console can badge 進行中 / 入力待ち
// and notify on arrival. opencode has no settings.json hooks, but it does expose a
// plugin event stream — we listen to it and shell out to `workspace-agent
// session-status <state> <sid>`.
//
// The session id we report is OUR deterministic sid (uuidv5(dir|name)), injected as
// AF_SESSION_SID when the Agent launches `opencode --continue` in tmux. opencode's
// own internal session id is irrelevant — one tmux = one opencode = one AF session,
// so all events map to that single sid. The Agent records {state,ts} keyed by it and
// wireSession surfaces it (same store/path as claude). Absent AF_SESSION_SID (e.g. a
// shell-launched opencode) the plugin is a no-op.
//
// One module for both opencode majors, with no dependency on @opencode/plugin: opencode 2
// reads `id` and `setup` from a plain default-exported object and ignores `server`;
// opencode 1 (object entrypoints, 1.18.29+) calls `server` and ignores the rest. The two
// event vocabularies differ, the debounced state machine behind them does not.
//
// State mapping (debounced — only fires on transitions, so ~2 calls per turn):
//   1: message.* (assistant producing output / user just submitted) -> working
//      session.idle (response finished, awaiting input)             -> idle
//   2: session.execution.started                                    -> working
//      session.execution.succeeded | failed | interrupted | session.idle -> idle
//      (measured on 2.0.28: started/succeeded bracket a turn; the others by their names)
//
// opencode 1 can emit a finalization message.* shortly AFTER session.idle (the
// assistant message settling, usage, etc.). Taken literally that flips the badge
// back to working with no following idle => stuck 進行中. So message.* within a
// short grace window after session.idle is treated as trailing and ignored; a
// genuine new turn always arrives well after that window. The 2 events are explicit
// lifecycle edges and need no such window.
//
// It also records the opencode session id (ses_…) of the live session, keyed by
// AF_SESSION_SID, into ~/.config/agent-fleet/opencode-sid/<AF_SESSION_SID>. The Agent
// reads it to resume THIS slot's own session (opencode --session <id>) on relaunch,
// so each slot keeps a distinct conversation instead of sharing via --continue.
import { execFile } from "node:child_process";
import { writeFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";

const GRACE_MS = 1500; // ignore message.* this long after session.idle (trailing finalize)
// The override exists for tests (plugin_modules_test.go): they must observe the reported states
// without touching a real Agent.
const AGENT_BIN = process.env.AF_WORKSPACE_AGENT_BIN || "/usr/local/bin/workspace-agent";

const bridge = () => {
  const sid = process.env.AF_SESSION_SID;
  const sidDir = process.env.HOME ? join(process.env.HOME, ".config/agent-fleet/opencode-sid") : null;
  let cur = "";
  let idleAt = 0;
  let queue = Promise.resolve();
  // Set when the plugin unloads. Whatever is still in flight (a session lookup, a queued report)
  // must not write the slot or report a state after a reload's new bridge took over.
  let stopped = false;
  const set = (state) => {
    if (!sid || state === cur || stopped) return;
    cur = state;
    // Fire-and-forget, but one at a time: separate processes finish in any order, and a
    // working that lands after its idle leaves the badge stuck. Absolute path so it resolves
    // regardless of the plugin's PATH.
    queue = queue.then(
      () =>
        new Promise((resolve) => {
          if (stopped) return resolve();
          try {
            execFile(AGENT_BIN, ["session-status", state, sid], () => resolve());
          } catch {
            resolve();
          }
        }),
    );
  };
  const recordOcid = (ocid) => {
    if (!sid || !ocid || !sidDir || stopped) return;
    try {
      mkdirSync(sidDir, { recursive: true });
      writeFileSync(join(sidDir, sid), ocid);
    } catch {
      /* ignore */
    }
  };
  const idle = () => {
    idleAt = Date.now();
    set("idle");
  };

  // opencode 1. Extract the opencode session id from whichever event carries one, so the
  // recorded slot→session mapping always tracks the CURRENTLY-ACTIVE session — not just the
  // first one created. opencode can start a new session mid-run (e.g. after an interrupt); if
  // we only captured session.created we'd keep resuming/reading the stale session and the
  // chat would show nothing new. session.idle / message.* both carry the live session id.
  const sidOf = (props) => {
    if (!props) return null;
    if (props.sessionID) return props.sessionID; // session.*, message.removed, part.*
    if (props.info && props.info.sessionID) return props.info.sessionID; // message.updated
    if (props.part && props.part.sessionID) return props.part.sessionID; // message.part.updated
    return null;
  };
  const onV1 = (event) => {
    const t = event && event.type;
    if (!t) return;
    const ocid = sidOf(event.properties);
    if (ocid) recordOcid(ocid); // keep the slot pinned to the live session
    if (t === "session.idle") {
      idle();
    } else if (t.startsWith("message.")) {
      if (Date.now() - idleAt < GRACE_MS) return; // trailing finalize after idle
      set("working");
    }
  };

  // opencode 2: the payload is under `data`. A subagent's own execution would otherwise flip
  // the badge to idle while the parent still works, and pin the slot to the subagent's id, so
  // child sessions are left out of both. The event stream is live-only: a subagent resumed after
  // a restart or reload never shows its session.created here, so an unseen id is looked up
  // (`lookup` = ctx.session.get). A failed lookup leaves the state and the id alone.
  const childOf = new Map(); // session id -> is a subagent session
  const isChild = async (id, lookup) => {
    if (childOf.has(id)) return childOf.get(id);
    try {
      const info = await lookup({ sessionID: id });
      childOf.set(id, Boolean(info && info.parentID));
      return childOf.get(id);
    } catch {
      return null;
    }
  };
  const onV2 = async (event, lookup) => {
    const t = event && event.type;
    const data = event && event.data;
    if (!t || !data || typeof data.sessionID !== "string") return;
    if (t === "session.created") {
      childOf.set(data.sessionID, Boolean(data.parentID));
      if (!data.parentID) recordOcid(data.sessionID);
      return;
    }
    const working = t === "session.execution.started";
    const done =
      t === "session.execution.succeeded" ||
      t === "session.execution.failed" ||
      t === "session.execution.interrupted" ||
      t === "session.idle";
    if (!working && !done) return;
    const child = await isChild(data.sessionID, lookup);
    if (stopped || child !== false) return;
    if (working) {
      recordOcid(data.sessionID);
      set("working");
    } else {
      idle();
    }
  };
  return { onV1, onV2, stop: () => void (stopped = true) };
};

export default {
  id: "agent-fleet.status",
  // opencode 2: the subscription is an async iterator; the returned cleanup aborts it.
  setup(ctx) {
    const { onV2, stop } = bridge();
    const ac = new AbortController();
    void (async () => {
      try {
        for await (const event of ctx.event.subscribe({ signal: ac.signal })) await onV2(event, (input) => ctx.session.get(input));
      } catch {
        /* aborted on unload, or the stream ended */
      }
    })();
    return () => {
      stop();
      ac.abort();
    };
  },
  // opencode 1.
  async server() {
    const { onV1 } = bridge();
    return { event: async ({ event }) => onV1(event) };
  },
};
