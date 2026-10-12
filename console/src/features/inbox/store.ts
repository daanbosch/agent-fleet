// Client state for the member inbox (ADR 0111 decisions 2, 8 and 10).
//
// The CP's database is the truth. GET /api/inbox/stream is a nudge that carries only the
// mailbox counters, and a change there re-reads GET /api/inbox. When the stream is not there
// (an older CP answers 404, a proxy drops it) the same route is polled instead, so the inbox
// keeps working with nothing but REST.
//
// This is not the notification center: that is a seven-day event history, and an open
// request must neither expire there nor sit among answer-ready events (decision 8). The
// badge is the inbox's own.
import { create } from "zustand";
import { api, apiJSON, getTenant, rel, type ApiError } from "../../core/api/client.ts";

export type InboxKind = "dm" | "request";
export type InboxState = "" | "open" | "claimed" | "done" | "declined" | "withdrawn";
export type InboxOp = "claim" | "decline" | "release" | "done" | "withdraw";

export interface InboxPerson {
  userKey: string;
  email: string;
  removed: boolean;
}

export interface InboxRecipient extends InboxPerson {
  /** The sender alone sees each recipient's delivery ("" for everyone else). */
  delivery: "" | "unread" | "read" | "declined";
  eligible: boolean;
}

export interface InboxItem {
  id: string;
  kind: InboxKind;
  state: InboxState;
  role: "sender" | "recipient";
  delivery: "unread" | "read" | "declined";
  sender: InboxPerson;
  recipients: InboxRecipient[];
  assignee: InboxPerson | null;
  assignedToMe: boolean;
  body: string;
  canClaim: boolean;
  longOpen: boolean;
  unseen: boolean;
  seq: number;
  createdAt: string;
  updatedAt: string;
  finalAt: string;
  session: { name: string; dir: string; branch: string; launchedAt: string; by: InboxPerson } | null;
  pr: { url: string; branch: string } | null;
}

export interface InboxMailbox {
  lastSeq: number;
  readSeq: number;
  unseen: number;
  dnd: boolean;
  acceptFrom: "tenant" | "chosen";
}

export interface InboxSettings {
  acceptFrom: "tenant" | "chosen";
  dnd: boolean;
  allowed: InboxPerson[];
  blocked: InboxPerson[];
  /** Bodies are encrypted at rest with the tenant key. */
  sealed: boolean;
}

export type InboxResult = { ok: true; item?: InboxItem } | { ok: false; error: ApiError };

interface InboxStore {
  items: InboxItem[];
  mailbox: InboxMailbox | null;
  /** The last read failed (an older CP without the inbox, a network drop). */
  loadError: boolean;
  loaded: boolean;
  /** The tenant the items belong to; a reply for another tenant is dropped. */
  tenant: string;
  refresh(): Promise<void>;
  send(kind: InboxKind, recipients: string[], body: string): Promise<InboxResult>;
  act(id: string, op: InboxOp): Promise<InboxResult>;
  markRead(id: string): Promise<void>;
  markSeen(): Promise<void>;
  setPR(id: string, url: string, branch: string): Promise<InboxResult>;
  reset(): void;
}

function itemResult(d: { item?: InboxItem; error?: ApiError } | null | undefined): InboxResult {
  if (!d || d.error) return { ok: false, error: d?.error ?? { code: "unknown", message: "" } };
  return { ok: true, item: d.item };
}

export const useInboxStore = create<InboxStore>((set, get) => ({
  items: [],
  mailbox: null,
  loadError: false,
  loaded: false,
  tenant: "",
  async refresh() {
    const tenant = getTenant();
    const d = await api("api/inbox").catch(() => null);
    if (tenant !== getTenant()) return;
    if (!d || d.error || !Array.isArray(d.items)) {
      set({ loadError: true, tenant });
      return;
    }
    set({ items: d.items, mailbox: d.mailbox ?? null, loadError: false, loaded: true, tenant });
  },
  async send(kind, recipients, body) {
    const d = await apiJSON("api/inbox/items", "POST", { kind, recipients, body }).catch(() => null);
    void get().refresh();
    return itemResult(d);
  },
  async act(id, op) {
    const d = await apiJSON(`api/inbox/items/${encodeURIComponent(id)}/${op}`, "POST", {}).catch(() => null);
    void get().refresh();
    return itemResult(d);
  },
  async markRead(id) {
    await apiJSON(`api/inbox/items/${encodeURIComponent(id)}/read`, "POST", {}).catch(() => null);
    void get().refresh();
  },
  async markSeen() {
    const mb = get().mailbox;
    if (!mb || mb.readSeq >= mb.lastSeq) return;
    const d = await apiJSON("api/inbox/cursor", "POST", { seq: mb.lastSeq }).catch(() => null);
    if (d && !d.error && d.mailbox) set({ mailbox: d.mailbox });
  },
  async setPR(id, url, branch) {
    const d = await apiJSON(`api/inbox/items/${encodeURIComponent(id)}/pr`, "PUT", { url, branch }).catch(() => null);
    void get().refresh();
    return itemResult(d);
  },
  reset() {
    set({ items: [], mailbox: null, loadError: false, loaded: false, tenant: "" });
  },
}));

/** The badge number. Do-not-disturb mutes the badge, never delivery (decision 10). */
export function inboxBadge(mb: InboxMailbox | null): number {
  if (!mb || mb.dnd) return 0;
  return mb.unseen;
}

/** Requests the caller can take now: the "waiting for someone" list. */
export function waitingItems(items: InboxItem[]): InboxItem[] {
  return items.filter((i) => i.role === "recipient" && i.canClaim);
}

/** Requests the caller holds. */
export function assignedItems(items: InboxItem[]): InboxItem[] {
  return items.filter((i) => i.assignedToMe && i.state === "claimed");
}

export function receivedItems(items: InboxItem[]): InboxItem[] {
  return items.filter((i) => i.role === "recipient");
}

export function sentItems(items: InboxItem[]): InboxItem[] {
  return items.filter((i) => i.role === "sender");
}

// --- The channel: one stream plus a polling fallback, refcounted to one per tab ----------

const POLL_FALLBACK_MS = 20000; // no stream: poll at this pace
const POLL_SAFETY_MS = 120000; // stream up: an occasional re-read in case a frame was lost
const RETRY_MAX_MS = 60000;
const WATCHDOG_MS = 45000; // the server pings every 20 seconds

let users = 0;
let ctrl: AbortController | null = null;
let streaming = false;
let pollTimer = 0;
let retryTimer = 0;
let backoff = 2000;
let streamTenant = "";
let lastSeq = -1;

/** True while the inbox stream is connected. */
export const inboxStreaming = (): boolean => streaming;

function applyFrame(line: string): void {
  let obj: { stream?: string; data?: InboxMailbox };
  try {
    obj = JSON.parse(line);
  } catch {
    return;
  }
  if (obj.stream !== "inbox" || !obj.data) return;
  const mb = obj.data;
  const st = useInboxStore.getState();
  // The counters alone are enough for the badge. Anything else re-reads the list.
  useInboxStore.setState({ mailbox: { ...(st.mailbox ?? mb), ...mb } });
  if (mb.lastSeq !== lastSeq) {
    lastSeq = mb.lastSeq;
    void st.refresh();
  }
}

async function connect(): Promise<void> {
  if (users === 0 || ctrl || document.hidden) return;
  const my = new AbortController();
  ctrl = my;
  streamTenant = getTenant();
  let watchdog = 0;
  const arm = () => {
    window.clearTimeout(watchdog);
    watchdog = window.setTimeout(() => my.abort(), WATCHDOG_MS);
  };
  let unsupported = false;
  try {
    arm();
    const res = await fetch(rel("api/inbox/stream"), { signal: my.signal });
    const ct = res.headers.get("Content-Type") || "";
    if (!res.ok || !ct.startsWith("text/event-stream") || !res.body) {
      unsupported = res.status === 404 || res.status === 405;
      void res.body?.cancel().catch(() => {});
      return;
    }
    streaming = true;
    backoff = 2000;
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      arm();
      buf += dec.decode(value, { stream: true });
      let idx: number;
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const frame = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        if (my.signal.aborted) return;
        if (frame.startsWith("data:")) applyFrame(frame.slice(5).trim());
      }
    }
  } catch {
    /* aborted (hidden tab, tenant switch, watchdog) or dropped: retry below */
  } finally {
    window.clearTimeout(watchdog);
    if (ctrl === my) ctrl = null;
    streaming = false;
    if (users > 0) {
      window.clearTimeout(retryTimer);
      // An older CP has no stream: polling carries the inbox, and the stream is looked
      // for again only rarely.
      retryTimer = window.setTimeout(() => void connect(), unsupported ? 300000 : backoff);
      backoff = Math.min(backoff * 2, RETRY_MAX_MS);
    }
  }
}

function pollTick(): void {
  if (document.hidden) return;
  // A tenant switch: drop what belongs to the old tenant and start over.
  if (streamTenant && streamTenant !== getTenant()) {
    useInboxStore.getState().reset();
    lastSeq = -1;
    ctrl?.abort();
    void useInboxStore.getState().refresh();
    return;
  }
  void useInboxStore.getState().refresh();
}

let lastPoll = 0;
function schedulePoll(): void {
  window.clearInterval(pollTimer);
  pollTimer = window.setInterval(() => {
    const every = streaming ? POLL_SAFETY_MS : POLL_FALLBACK_MS;
    if (Date.now() - lastPoll < every && !(streamTenant && streamTenant !== getTenant())) return;
    lastPoll = Date.now();
    pollTick();
  }, 5000);
}

function onVisibility(): void {
  if (document.hidden) ctrl?.abort();
  else {
    void useInboxStore.getState().refresh();
    void connect();
  }
}

/** Start the inbox channel for this tab. Returns the stopper; refcounted. */
export function startInboxChannel(): () => void {
  if (++users === 1) {
    lastPoll = Date.now();
    void useInboxStore.getState().refresh();
    void connect();
    schedulePoll();
    document.addEventListener("visibilitychange", onVisibility);
  }
  let stopped = false;
  return () => {
    if (stopped) return;
    stopped = true;
    if (--users === 0) {
      window.clearInterval(pollTimer);
      window.clearTimeout(retryTimer);
      document.removeEventListener("visibilitychange", onVisibility);
      ctrl?.abort();
      ctrl = null;
      lastSeq = -1;
    }
  };
}
