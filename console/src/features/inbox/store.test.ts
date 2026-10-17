// The inbox badge and the lists the cards are grouped into (ADR 0111 decisions 2 and 10).
import { describe, it, expect, vi } from "vitest";

// The store's API client reads localStorage at import, which the node project has not got.
vi.mock("../../core/api/client.ts", () => ({ api: vi.fn(), apiJSON: vi.fn(), getTenant: () => "", rel: (p: string) => p }));
import { assignedItems, inboxBadge, receivedItems, sentItems, waitingItems, type InboxItem, type InboxMailbox } from "./store.ts";

const mb = (o: Partial<InboxMailbox> = {}): InboxMailbox => ({ lastSeq: 5, readSeq: 2, unseen: 3, dnd: false, acceptFrom: "tenant", ...o });

const item = (o: Partial<InboxItem>): InboxItem => ({
  id: "i",
  kind: "request",
  state: "open",
  role: "recipient",
  delivery: "unread",
  sender: { userKey: "a", email: "a@example.com", removed: false },
  recipients: [],
  assignee: null,
  assignedToMe: false,
  body: "x",
  canClaim: false,
  longOpen: false,
  unseen: false,
  seq: 1,
  createdAt: "2026-10-12T00:00:00Z",
  updatedAt: "2026-10-12T00:00:00Z",
  finalAt: "",
  session: null,
  pr: null,
  ...o,
});

describe("inboxBadge", () => {
  it("is the server's unseen count", () => {
    expect(inboxBadge(mb())).toBe(3);
  });
  it("is muted by do-not-disturb, which never stops delivery (docs/log/130 A-7)", () => {
    expect(inboxBadge(mb({ dnd: true }))).toBe(0);
  });
  it("is 0 before the first load and for an older CP", () => {
    expect(inboxBadge(null)).toBe(0);
  });
});

describe("grouping", () => {
  const items = [
    item({ id: "w", canClaim: true }),
    item({ id: "taken", state: "claimed", assignee: { userKey: "c", email: "", removed: false } }),
    item({ id: "mine", state: "claimed", assignedToMe: true }),
    item({ id: "sent", role: "sender" }),
    item({ id: "dm", kind: "dm", state: "" }),
  ];
  it("waiting holds only what the caller can take now", () => {
    expect(waitingItems(items).map((i) => i.id)).toEqual(["w"]);
  });
  it("assigned holds the caller's own claims", () => {
    expect(assignedItems(items).map((i) => i.id)).toEqual(["mine"]);
  });
  it("received and sent split by role", () => {
    expect(receivedItems(items).map((i) => i.id)).toEqual(["w", "taken", "mine", "dm"]);
    expect(sentItems(items).map((i) => i.id)).toEqual(["sent"]);
  });
});
