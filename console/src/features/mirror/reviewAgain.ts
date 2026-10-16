// "Review again in <reviewer>": remember which session reviewed a plan, and send the revised
// plan back to it as a peer message instead of launching a cold reviewer.
//
// Where the memory lives: this Console's localStorage, keyed by planner session + plan FILE
// path. The launch dialog is a Console flow and the reviewer's name is only known to it
// (LaunchResult.name), so no server record exists to read it back from. The cost is that
// another browser does not see the link and falls back to the launch dialog — a missed
// shortcut, never a wrong send. The path is half the key because one planner session can hold
// two plans (claude names the file per plan), and a revision rewrites the SAME file, which is
// what makes the second round find the first round's reviewer.
//
// Only the NAME is stored. The label shown on the button is read from the live session list
// each render, so a renamed reviewer shows its new title and a deleted one is not offered
// (names are immutable slugs, never reused).
import { useSyncExternalStore } from "react";
import { sessionPeerSend, sessionReady } from "../../core/api/client.ts";
import { useSessionsStore } from "../sessions/store.ts";
import type { Session } from "../../types/session.ts";
import { rereviewPrompt } from "./planReview.ts";

const LS_KEY = "af.plan-reviewers";
/** Oldest entry evicted first, so localStorage cannot grow without bound. */
const MAX_ENTRIES = 50;

type Store = Record<string, string>;

export const reviewerKey = (planner: string, planPath: string) => `${planner}\u0000${planPath}`;

function load(): Store {
  try {
    const parsed = JSON.parse(localStorage.getItem(LS_KEY) || "null") as unknown;
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    const out: Store = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) if (typeof v === "string" && v) out[k] = v;
    return out;
  } catch {
    return {}; // private mode / corrupt JSON: the shortcut is auxiliary, start empty
  }
}

let store: Store = load();
const listeners = new Set<() => void>();

function commit(next: Store) {
  store = next;
  try {
    localStorage.setItem(LS_KEY, JSON.stringify(next));
  } catch {
    /* quota / private mode: keep it in memory for this tab */
  }
  listeners.forEach((l) => l());
}

/** Record that `reviewer` reviewed the plan at `planPath` of `planner`. A later launch for the
 *  same plan replaces it: the newest reviewer is the one holding the latest round's context. */
export function rememberReviewer(planner: string, planPath: string, reviewer: string) {
  if (!planner || !planPath || !reviewer) return;
  const k = reviewerKey(planner, planPath);
  const next = { ...store };
  delete next[k]; // re-insert last so insertion order is recency
  next[k] = reviewer;
  const keys = Object.keys(next);
  for (const old of keys.slice(0, Math.max(0, keys.length - MAX_ENTRIES))) delete next[old];
  commit(next);
}

export function forgetReviewer(planner: string, planPath: string) {
  const k = reviewerKey(planner, planPath);
  if (!(k in store)) return;
  const next = { ...store };
  delete next[k];
  commit(next);
}

/** Does `planner` have any remembered reviewer? Gates the plan-file lookup behind the card. */
export const hasRememberedReviewer = (planner: string): boolean =>
  Object.keys(store).some((k) => k.startsWith(planner + "\u0000"));

export const rememberedReviewer = (planner: string, planPath: string): string => store[reviewerKey(planner, planPath)] || "";

/** Test seam: re-read storage (the module reads it once at import). */
export function reloadReviewersForTest() {
  store = load();
  listeners.forEach((l) => l());
}

const subscribe = (l: () => void) => {
  listeners.add(l);
  return () => void listeners.delete(l);
};

/** The session to offer "Review again" for: remembered, still in the loaded list, not the
 *  planner itself. null = launch dialog instead. An unloaded list is "unknown", not "gone",
 *  but offering nothing until it loads is the safe side. */
export function liveReviewer(
  planner: string,
  planPath: string,
  sessions: Session[],
  remembered = rememberedReviewer(planner, planPath),
): Session | null {
  if (!planPath || !remembered || remembered === planner) return null;
  const s = sessions.find((x) => x.name === remembered);
  // resumable:false = archive only, nothing a message could wake.
  return s && (s.alive || s.resumable !== false) ? s : null;
}

export const reviewerLabel = (s: Session) => s.title || s.label || s.name;

export const useHasRememberedReviewer = (planner: string): boolean =>
  useSyncExternalStore(subscribe, () => hasRememberedReviewer(planner));

/** The reviewer for the plan at `planPath` of `planner`, reactive to both the memory and the
 *  session list. */
export function useReviewer(planner: string, planPath: string): Session | null {
  const remembered = useSyncExternalStore(subscribe, () => rememberedReviewer(planner, planPath));
  const sessions = useSessionsStore((s) => s.sessions);
  return liveReviewer(planner, planPath, sessions, remembered);
}

export type RereviewResult = { ok: true } | { ok: false; message: string; silent?: boolean };

const READY_TIMEOUT_MS = 30_000;
const READY_POLL_MS = 500;

/** Make the reviewer able to take a message: resume it when stopped and wait for its composer.
 *  Runs BEFORE the planner is rejected on the live card, so a reviewer that will not come up
 *  leaves the plan exactly as it was. `silent` = start() already toasted the reason. */
export async function prepareReviewer(
  reviewer: Session,
  deps = { start: (n: string) => useSessionsStore.getState().start(n), ready: sessionReady, sleep: (ms: number) => new Promise<void>((r) => setTimeout(r, ms)) },
): Promise<RereviewResult> {
  if (reviewer.alive) return { ok: true };
  if (!(await deps.start(reviewer.name))) return { ok: false, message: "", silent: true };
  for (let waited = 0; waited < READY_TIMEOUT_MS; waited += READY_POLL_MS) {
    if (await deps.ready(reviewer.name)) return { ok: true };
    await deps.sleep(READY_POLL_MS);
  }
  return { ok: false, message: "timeout" };
}

/** Send the revised plan to the reviewer. `question` (not `request`) because the findings are
 *  the answer: a request only owes a reply when blocked, and the planner is waiting. */
export async function sendRereview(
  planner: string,
  planPath: string,
  reviewer: Session,
  send: typeof sessionPeerSend = sessionPeerSend,
): Promise<RereviewResult> {
  const r = await send(reviewer.name, planner, "question", rereviewPrompt({ parent: planner, path: planPath }));
  if (r.ok) return { ok: true };
  // A reviewer deleted since the list was read is gone for good: stop offering it.
  if (r.code === "peer_target_unknown") forgetReviewer(planner, planPath);
  return { ok: false, message: r.message || "" };
}


/** The two cards' flows share one order: make the reviewer ready, THEN do the card's own
 *  precondition (`beforeSend`: the live card rejects the planner so it is idle rather than
 *  behind the approval modal), THEN send, and only a successful send runs `onSent` (the carried
 *  card discards itself there). A failure before the send leaves the plan card untouched; one
 *  after it leaves it as usable as a cancelled launch dialog does. */
export async function runRereview(o: {
  planner: string;
  path: string;
  reviewer: Session;
  beforeSend?: () => Promise<void>;
  onSent?: () => void;
  onFail: (message: string) => void;
  prepare?: typeof prepareReviewer;
  send?: typeof sessionPeerSend;
}): Promise<boolean> {
  const ready = await (o.prepare ?? prepareReviewer)(o.reviewer);
  if (!ready.ok) {
    if (!ready.silent) o.onFail(ready.message);
    return false;
  }
  await o.beforeSend?.();
  const sent = await sendRereview(o.planner, o.path, o.reviewer, o.send);
  if (!sent.ok) {
    o.onFail(sent.message);
    return false;
  }
  o.onSent?.();
  return true;
}
