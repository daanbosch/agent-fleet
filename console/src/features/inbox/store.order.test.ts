// The inbox store across tenants and out-of-order answers (ADR 0111, docs/log/130 A-3 in the
// Console, R-2/R-3 convergence): an answer for another tenant, a scope already left, or older
// than the one applied never reaches the screen.
import { it, expect, vi, beforeEach, describe } from "vitest";

const x = vi.hoisted(() => ({ tenant: "t1", api: vi.fn(), apiJSON: vi.fn() }));
vi.mock("../../core/api/client.ts", () => ({ api: x.api, apiJSON: x.apiJSON, getTenant: () => x.tenant, rel: (p: string) => p }));

import { inboxBadge, mailboxNewer, setInboxScope, useInboxStore } from "./store.ts";

const mb = (seq: number, read = 0) => ({ lastSeq: seq, readSeq: read, unseen: seq - read, dnd: false, acceptFrom: "tenant" as const });
const reply = (state: string, seq: number) => ({ items: [{ id: "t1-item", state, body: "t1 body" }], mailbox: mb(seq) });
const deferred = () => {
  let resolve: (v: unknown) => void = () => {};
  const p = new Promise((r) => (resolve = r));
  return { p, resolve };
};

beforeEach(() => {
  useInboxStore.getState().reset();
  x.tenant = "t1";
  x.api.mockReset();
  x.apiJSON.mockReset();
});

describe("inbox store ordering", () => {
  it("drops an older list that answers after a newer one", async () => {
    const old = deferred();
    x.api.mockImplementationOnce(() => old.p).mockResolvedValueOnce(reply("withdrawn", 2));
    const first = useInboxStore.getState().refresh();
    await useInboxStore.getState().refresh();
    expect(useInboxStore.getState().items[0].state).toBe("withdrawn");
    old.resolve(reply("claimed", 1));
    await first;
    expect(useInboxStore.getState().items[0].state).toBe("withdrawn");
    expect(useInboxStore.getState().mailbox?.lastSeq).toBe(2);
  });

  it("clears the previous tenant's bodies and badge when the new tenant's load fails", async () => {
    x.api.mockResolvedValueOnce(reply("claimed", 1));
    await useInboxStore.getState().refresh();
    x.tenant = "t2";
    x.api.mockResolvedValueOnce({ error: { code: "http_404" } });
    await useInboxStore.getState().refresh();
    expect(useInboxStore.getState().items).toEqual([]);
    expect(inboxBadge(useInboxStore.getState().mailbox)).toBe(0);
    expect(useInboxStore.getState().loadError).toBe(true);
  });

  it("ignores a cursor reply from the previous tenant", async () => {
    x.api.mockResolvedValueOnce(reply("claimed", 4));
    await useInboxStore.getState().refresh();
    const cursor = deferred();
    x.apiJSON.mockImplementationOnce(() => cursor.p);
    const first = useInboxStore.getState().markSeen();
    x.tenant = "t2";
    x.api.mockResolvedValueOnce({ items: [], mailbox: mb(0) });
    await useInboxStore.getState().refresh();
    cursor.resolve({ mailbox: mb(4) });
    await first;
    expect(inboxBadge(useInboxStore.getState().mailbox)).toBe(0);
  });

  it("drops a list asked under a scope that has since been left", async () => {
    setInboxScope("u|t1");
    const old = deferred();
    x.api.mockImplementationOnce(() => old.p);
    const first = useInboxStore.getState().refresh();
    x.tenant = "t2";
    x.api.mockResolvedValue({ items: [], mailbox: mb(0) });
    setInboxScope("u|t2");
    old.resolve(reply("claimed", 7));
    await first;
    expect(useInboxStore.getState().items).toEqual([]);
    expect(inboxBadge(useInboxStore.getState().mailbox)).toBe(0);
  });

  it("keeps the newer of two mailboxes", () => {
    expect(mailboxNewer(mb(3, 1), mb(2, 2))).toBe(false);
    expect(mailboxNewer(mb(3, 1), mb(3, 2))).toBe(true);
    expect(mailboxNewer(null, mb(0))).toBe(true);
  });
});
