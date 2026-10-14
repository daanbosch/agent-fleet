// agent-fleet's shared opencode 2 server (issue #2120): the two things the server's own
// process cannot do for the Terminal sessions that share it.
//
// It does nothing unless af started this server for shared Terminal sessions
// (AF_OPENCODE_SHARED_SERVE=1, no AF_SESSION_SID in its environment). A standalone TUI has a
// server of its own whose environment is already that session's.
//
// 1. Engine token per session. The server's environment carries the workspace-scoped engine token
//    (the provider block reads {env:AF_ENGINE_TOKEN}), so every session would spend as the
//    workspace. A request that carries exactly that token gets the Authorization of the af
//    session it belongs to, minted by the Agent (`workspace-agent opencode-engine-token <name>`).
//    The token is never stored on the session: opencode.db is plaintext and a fork copies
//    metadata. Anything else (another provider, a request without the token) is left alone.
//
// 2. A shell command waits for its environment. A session's shell environment is held in the
//    server's memory only, and until it is put there the shell sees the SERVER's environment
//    (provider keys, the server password). Two moments leave a session without one: a server
//    that has just restarted (the Agent puts every live Terminal session's environment back and
//    then writes its pid to a marker file), and a session created from inside a TUI with /new
//    (the TUI plugin puts the pane's environment and only then tags the session with af.sid).
//    The shell tool waits for both, up to a bound, and then runs regardless: a missing Agent
//    must not wedge every command. Sessions Managed by af (af.managed), subagent sessions and
//    sessions that are not new are never held for a tag.
//
// opencode 1 has no shared server; the module only keeps the file loadable there.
import { execFile } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const AGENT_BIN = process.env.AF_WORKSPACE_AGENT_BIN || "/usr/local/bin/workspace-agent";
const READY_FILE = process.env.AF_OPENCODE_READY_FILE || (process.env.HOME ? join(process.env.HOME, ".config/agent-fleet/opencode-env-ready") : "");
const READY_WAIT_MS = Number(process.env.AF_OPENCODE_READY_WAIT_MS) || 10000;
const TAG_WAIT_MS = Number(process.env.AF_OPENCODE_TAG_WAIT_MS) || 5000;
const NEW_SESSION_MS = 60000; // a session older than this is not waiting for a first tag
const TOKEN_TTL_MS = 10 * 60 * 1000;
const TOKEN_MISS_TTL_MS = 60 * 1000;
const SHELL_TOOLS = new Set(["shell", "bash"]);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const shared = () => process.env.AF_OPENCODE_SHARED_SERVE === "1" && !process.env.AF_SESSION_SID;

const readyPid = () => {
  try {
    return Number(readFileSync(READY_FILE, "utf8").trim());
  } catch {
    return 0;
  }
};

const mintToken = (name) =>
  new Promise((resolve) => {
    try {
      execFile(AGENT_BIN, ["opencode-engine-token", name], { timeout: 20000 }, (err, stdout) => resolve(err ? "" : String(stdout).trim()));
    } catch {
      resolve("");
    }
  });

export default {
  id: "agent-fleet.shared",
  async setup(ctx) {
    if (!shared()) return;
    const get = (sessionID) => ctx.session.get({ sessionID });

    // --- engine token ---
    const tokens = new Map(); // af session name -> { value, until }
    const tokenFor = async (name) => {
      const hit = tokens.get(name);
      if (hit && hit.until > Date.now()) return hit.value;
      const value = await mintToken(name);
      tokens.set(name, { value, until: Date.now() + (value ? TOKEN_TTL_MS : TOKEN_MISS_TTL_MS) });
      return value;
    };
    const names = new Map(); // session id -> af session name
    await ctx.session.hook("http.request", async (event) => {
      const workspaceToken = process.env.AF_ENGINE_TOKEN;
      const headers = event && event.request && event.request.headers;
      if (!workspaceToken || !headers || headers.get("authorization") !== `Bearer ${workspaceToken}`) return;
      let name = names.get(event.sessionID);
      if (!name) {
        try {
          const info = await get(event.sessionID);
          name = info && info.metadata && info.metadata["af.name"];
        } catch {
          return;
        }
        if (typeof name !== "string" || !name) return;
        names.set(event.sessionID, name);
      }
      const token = await tokenFor(name);
      if (token) headers.set("authorization", `Bearer ${token}`);
    });

    // --- shell waits for its environment ---
    let ready = false; // this server generation has its environments (or the wait ran out)
    const waitReady = async () => {
      const until = Date.now() + READY_WAIT_MS;
      while (!ready && Date.now() < until) {
        if (readyPid() === process.pid) break;
        await sleep(100);
      }
      ready = true;
    };
    const placed = new Set(); // sessions that need no more waiting
    const waitTagged = async (sessionID) => {
      if (placed.has(sessionID)) return;
      const until = Date.now() + TAG_WAIT_MS;
      while (Date.now() < until) {
        let info;
        try {
          info = await get(sessionID);
        } catch {
          break;
        }
        const md = (info && info.metadata) || {};
        const created = info && info.time && info.time.created;
        if (info && info.parentID) break;
        if (md["af.sid"] || md["af.managed"]) break;
        if (typeof created === "number" && Date.now() - created > NEW_SESSION_MS) break;
        await sleep(100);
      }
      placed.add(sessionID);
    };
    await ctx.tool.hook("execute.before", async (event) => {
      if (!SHELL_TOOLS.has(String(event && event.tool))) return;
      if (!ready) await waitReady();
      if (event.sessionID) await waitTagged(event.sessionID);
    });
  },
  // opencode 1 has no shared server.
  async server() {
    return {};
  },
};
