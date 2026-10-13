// The member UI state store (ADR 0114): what its answers may and may not overwrite.
//
//   1. An answer that arrives after a tenant switch never lands in the other tenant's state or cache.
//   2. PATCHes go one at a time, and an earlier answer cannot undo a newer local choice.
//   3. A GET that started before a PATCH cannot roll the PATCH's answer back.
//   4. `{error}` answers (api() does not throw on HTTP errors) are failures, not documents.
//   5. The old localStorage keys go only after the CP took them, via if_absent.
//   6. A PATCH still queued at a tenant switch is never sent, and a sent one carries its own tenant.
//   7. A stale answer from an earlier visit to the same tenant (A, B, A) is not used.
//   8. The migration waits while this tab has its own unconfirmed choice.
//   9. A failed PATCH's value does not outlive the next load: the CP's value comes back.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

type Deferred = { resolve(v: unknown): void; reject(e: unknown): void; promise: Promise<unknown> };
const deferred = (): Deferred => {
  let resolve!: (v: unknown) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { resolve, reject, promise };
};

const gets: Deferred[] = [];
const patches: { url: string; body: unknown; tenant: string | null; d: Deferred }[] = [];
vi.mock("../api/client.ts", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/client.ts")>()),
  api: (url: string, opts?: RequestInit) => {
    const d = deferred();
    if (opts?.method === "PATCH") {
      const tenant = new Headers(opts.headers).get("X-AF-Tenant");
      patches.push({ url, body: JSON.parse(String(opts.body)), tenant, d });
    } else {
      gets.push(d);
    }
    return d.promise;
  },
}));

const { useUIStateStore } = await import("./uiState.ts");
const { setTenant, setUser } = await import("../api/client.ts");

const flush = async () => {
  for (let i = 0; i < 5; i++) await Promise.resolve();
};
const state = () => useUIStateStore.getState().state;
const cacheOf = (tenant: string) => JSON.parse(localStorage.getItem(`af.ui-state:u1:${tenant}`) || "null");

// Brings the store to a loaded, empty document for tenant `t`.
async function loadAs(t: string, doc: unknown = { defaults: { agent: "" } }) {
  setTenant(t);
  const p = useUIStateStore.getState().load();
  await flush();
  gets.at(-1)!.resolve(doc);
  await p;
}

beforeEach(async () => {
  localStorage.clear();
  gets.length = 0;
  patches.length = 0;
  setUser("u1");
  await loadAs("init");
});

afterEach(async () => {
  // Drain anything a test left in the PATCH queue so the next one starts clean.
  for (const p of patches) p.d.resolve({ error: { code: "test_teardown" } });
  await flush();
});

describe("member UI state store", () => {
  it("drops a PATCH answer that arrives after a tenant switch", async () => {
    await loadAs("a");
    const pa = useUIStateStore.getState().patch({ onboarding: { dismissed: true, track: "" } });
    await flush();
    await loadAs("b");
    patches[0].d.resolve({ onboarding: { dismissed: true, track: "" }, defaults: { agent: "codex" } });
    expect(await pa).toBe(false);
    expect(state().onboarding).toBeUndefined();
    expect(state().defaults?.agent).toBe("");
    expect(cacheOf("b")?.onboarding).toBeUndefined();
  });

  it("sends PATCHes one at a time and keeps a newer choice over an earlier answer", async () => {
    await loadAs("a");
    const p1 = useUIStateStore.getState().patch({ onboarding: { dismissed: false, track: "dev" } });
    const p2 = useUIStateStore.getState().patch({ onboarding: { dismissed: true, track: "dev" } });
    await flush();
    expect(patches).toHaveLength(1);
    patches[0].d.resolve({ onboarding: { dismissed: false, track: "dev" }, defaults: { agent: "" } });
    expect(await p1).toBe(true);
    // The first answer predates the second choice, which stays shown.
    expect(state().onboarding?.dismissed).toBe(true);
    await flush();
    expect(patches).toHaveLength(2);
    patches[1].d.resolve({ onboarding: { dismissed: true, track: "dev" }, defaults: { agent: "" } });
    expect(await p2).toBe(true);
    expect(state().onboarding).toEqual({ dismissed: true, track: "dev" });
  });

  it("does not let a GET started before a PATCH roll it back", async () => {
    setTenant("a");
    const load = useUIStateStore.getState().load();
    await flush();
    const p = useUIStateStore.getState().patch({ onboarding: { dismissed: true, track: "" } });
    await flush();
    patches[0].d.resolve({ onboarding: { dismissed: true, track: "" }, defaults: { agent: "" } });
    await p;
    gets.at(-1)!.resolve({ defaults: { agent: "" } }); // the document as it was before the PATCH
    await load;
    expect(state().onboarding?.dismissed).toBe(true);
    expect(useUIStateStore.getState().loaded).toBe(true);
  });

  it("treats {error} answers as failures", async () => {
    setTenant("a");
    localStorage.setItem("af.ui-state:u1:a", JSON.stringify({ onboarding: { dismissed: true, track: "" } }));
    const load = useUIStateStore.getState().load();
    await flush();
    gets.at(-1)!.resolve({ error: { code: "store", message: "boom", status: 500 } });
    await load;
    expect(useUIStateStore.getState().loaded).toBe(false);
    expect(state().onboarding?.dismissed).toBe(true); // the cache stays shown

    await loadAs("b");
    const p = useUIStateStore.getState().patch({ onboarding: { dismissed: true, track: "dev" } });
    await flush();
    patches[0].d.resolve({ error: { code: "conflict", status: 409 } });
    expect(await p).toBe(false);
    expect(state().onboarding).toEqual({ dismissed: true, track: "dev" }); // the local value stays
  });

  it("removes the old keys only after the CP took them, asking with if_absent", async () => {
    localStorage.setItem("af.onboarding.dismissed", "1");
    setTenant("a");
    let load = useUIStateStore.getState().load();
    await flush();
    gets.at(-1)!.resolve({ defaults: { agent: "" } });
    await flush();
    expect(patches[0].url).toBe("api/me/ui-state?if_absent=1");
    patches[0].d.reject(new Error("network down"));
    await load;
    expect(localStorage.getItem("af.onboarding.dismissed")).toBe("1");

    load = useUIStateStore.getState().load();
    await flush();
    gets.at(-1)!.resolve({ defaults: { agent: "" } });
    await flush();
    patches[1].d.resolve({ onboarding: { dismissed: true, track: "" }, defaults: { agent: "" } });
    await load;
    expect(localStorage.getItem("af.onboarding.dismissed")).toBeNull();
  });

  it("never sends a PATCH still queued at a tenant switch, and pins the tenant of a sent one", async () => {
    await loadAs("a");
    const p1 = useUIStateStore.getState().patch({ onboarding: { dismissed: false, track: "dev" } });
    const p2 = useUIStateStore.getState().patch({ onboarding: { dismissed: true, track: "dev" } });
    await flush();
    expect(patches).toHaveLength(1);
    expect(patches[0].tenant).toBe("a");
    await loadAs("b");
    patches[0].d.resolve({ onboarding: { dismissed: false, track: "dev" }, defaults: { agent: "" } });
    expect(await p1).toBe(false);
    expect(await p2).toBe(false);
    expect(patches).toHaveLength(1); // the queued one never reached the network
  });

  it("ignores a stale answer from an earlier visit to the same tenant", async () => {
    setTenant("a");
    const first = useUIStateStore.getState().load();
    await flush();
    const staleGet = gets.at(-1)!;
    await loadAs("b");
    await loadAs("a", { onboarding: { dismissed: true, track: "" }, defaults: { agent: "" } });
    staleGet.resolve({ defaults: { agent: "" } });
    await first;
    expect(state().onboarding?.dismissed).toBe(true);
    expect(cacheOf("a")?.onboarding?.dismissed).toBe(true);
  });

  it("defers the migration while this tab has its own unconfirmed choice", async () => {
    localStorage.setItem("af.onboarding.dismissed", "1");
    setTenant("a");
    const load = useUIStateStore.getState().load();
    await flush();
    void useUIStateStore.getState().patch({ onboarding: { dismissed: false, track: "dev" } });
    gets.at(-1)!.resolve({ defaults: { agent: "" } });
    await load;
    await flush();
    expect(patches.map((x) => x.url)).toEqual(["api/me/ui-state"]); // no if_absent upload
    expect(state().onboarding).toEqual({ dismissed: false, track: "dev" });
    expect(localStorage.getItem("af.onboarding.dismissed")).toBe("1"); // kept for a later load
  });

  it("lets the next load show the CP's value after a failed PATCH", async () => {
    await loadAs("a");
    const p = useUIStateStore.getState().patch({ onboarding: { dismissed: true, track: "" } });
    await flush();
    patches[0].d.resolve({ error: { code: "store", status: 500 } });
    expect(await p).toBe(false);
    expect(state().onboarding?.dismissed).toBe(true); // still shown until the next answer
    await loadAs("a", { defaults: { agent: "" } });
    expect(state().onboarding).toBeUndefined();
    expect(useUIStateStore.getState().loaded).toBe(true);
  });
});
