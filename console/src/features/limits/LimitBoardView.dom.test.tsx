// The board as a viewer sees it (ADR 0111 decision 6). Pinned: a member with nothing received
// reads "unknown" (never available), members stay in roster order whatever their headroom, and
// the member's own setting saves for this tenant only.
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

const api = vi.fn();
const apiJSON = vi.fn();
const toast = vi.fn();
vi.mock("../../core/api/client.ts", async (orig) => ({
  ...(await orig<typeof import("../../core/api/client.ts")>()),
  api: (...args: unknown[]) => api(...args),
  apiJSON: (...args: unknown[]) => apiJSON(...args),
  errText: (e: { message?: string }) => e?.message || "",
}));
vi.mock("../../ui/ToastProvider.tsx", () => ({ useToast: () => toast }));

import { LimitBoardView } from "./LimitBoardView.tsx";
import { useTenantStore } from "../../core/store/tenant.ts";

const me = {
  enabled: true,
  audience: "none",
  level: "state",
  members: [],
  own: { kinds: [] },
  publishes: [],
};
const recent = new Date(Date.now() - 5 * 60000).toISOString();
const board = {
  enabled: true,
  next: "",
  members: [
    { userKey: "amy", email: "amy@example.com", kinds: [{ kind: "claude", units: [{ unit: "5h", status: "current", usedPct: 99, observedAt: recent }] }] },
    { userKey: "ben", email: "ben@example.com", kinds: [] },
    { userKey: "cat", email: "cat@example.com", kinds: [{ kind: "codex", units: [{ unit: "5h", status: "current", usedPct: 3, observedAt: recent }] }] },
  ],
};

let root: Root | null = null;
let host: HTMLDivElement | null = null;

async function mount() {
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () => {
    root!.render(<LimitBoardView />);
  });
  await act(async () => {
    await Promise.resolve();
  });
}

beforeEach(() => {
  (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  api.mockReset();
  apiJSON.mockReset();
  api.mockImplementation((path: string) => Promise.resolve(path.startsWith("api/limit-board/me") ? me : board));
});
afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = null;
});

describe("LimitBoardView", () => {
  it("lists members in roster order and words an empty entry as unknown", async () => {
    await mount();
    const rows = Array.from(document.querySelectorAll(".lb-roster > li"));
    expect(rows.map((r) => r.querySelector(".lb-who")?.textContent)).toEqual([
      "amy@example.com",
      "ben@example.com",
      "cat@example.com",
    ]);
    expect(rows[1].textContent).toContain("不明");
    expect(document.body.textContent).not.toMatch(/利用可能|available/i);
    expect(rows[0].textContent).toContain("99% 以上");
  });

  it("saves the member's own setting", async () => {
    apiJSON.mockResolvedValue({ ...me, audience: "tenant", level: "band" });
    await mount();
    const selects = document.querySelectorAll<HTMLSelectElement>(".lb-form select");
    await act(async () => {
      selects[0].value = "tenant";
      selects[0].dispatchEvent(new Event("change", { bubbles: true }));
    });
    await act(async () => {
      selects[1].value = "band";
      selects[1].dispatchEvent(new Event("change", { bubbles: true }));
    });
    const save = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find((b) => b.textContent?.includes("保存"))!;
    await act(async () => {
      save.click();
    });
    expect(apiJSON).toHaveBeenCalledWith("api/limit-board/me", "PUT", { audience: "tenant", level: "band", members: [] });
  });

  // Review finding (#2017): a tenant switch while the board is open must not leave tenant A's
  // roster on screen (or A's draft saveable under B), and a late answer for A is dropped.
  it("clears and re-reads on a tenant switch", async () => {
    useTenantStore.setState({ tenant: "a" });
    await mount();
    expect(document.body.textContent).toContain("amy@example.com");
    api.mockImplementation((path: string) =>
      Promise.resolve(
        path.startsWith("api/limit-board/me")
          ? me
          : { enabled: true, next: "", members: [{ userKey: "zed", email: "zed@b.example", kinds: [] }] },
      ),
    );
    await act(async () => {
      useTenantStore.setState({ tenant: "b" });
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(document.body.textContent).not.toContain("amy@example.com");
    expect(document.body.textContent).toContain("zed@b.example");
  });

  it("drops an answer that arrives after the tenant changed", async () => {
    useTenantStore.setState({ tenant: "a" });
    let releaseA: (v: unknown) => void = () => {};
    api.mockImplementation((path: string) =>
      path.startsWith("api/limit-board/me") ? Promise.resolve(me) : new Promise((r) => (releaseA = r)),
    );
    await mount();
    const pendingA = releaseA;
    api.mockImplementation((path: string) =>
      Promise.resolve(
        path.startsWith("api/limit-board/me")
          ? me
          : { enabled: true, next: "", members: [{ userKey: "zed", email: "zed@b.example", kinds: [] }] },
      ),
    );
    await act(async () => {
      useTenantStore.setState({ tenant: "b" });
    });
    await act(async () => {
      pendingA(board);
      await Promise.resolve();
    });
    expect(document.body.textContent).toContain("zed@b.example");
    expect(document.body.textContent).not.toContain("amy@example.com");
  });

  // Review finding (#2017): an open board re-reads the CP (here on becoming visible), so a
  // revoked publication disappears, while the member's unsaved draft is left alone.
  it("re-reads on visibility without overwriting the draft", async () => {
    await mount();
    const sel = document.querySelector<HTMLSelectElement>(".lb-form select")!;
    await act(async () => {
      sel.value = "tenant";
      sel.dispatchEvent(new Event("change", { bubbles: true }));
    });
    api.mockImplementation((path: string) =>
      Promise.resolve(path.startsWith("api/limit-board/me") ? me : { ...board, members: board.members.map((m) => ({ ...m, kinds: [] })) }),
    );
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(document.body.textContent).not.toContain("99% 以上");
    expect(document.querySelector<HTMLSelectElement>(".lb-form select")!.value).toBe("tenant");
  });

  const flush = async () => {
    await act(async () => {
      await Promise.resolve();
    });
  };
  const visible = async () => {
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await flush();
  };
  const revoked = { ...board, members: board.members.map((m) => ({ ...m, kinds: [] })) };

  // Review finding (#2017, round 2): a "show more" answer held across a newer re-read (here one
  // that found the publications revoked) must not draw the old values again.
  it("drops a held page that answers after a newer read", async () => {
    api.mockImplementation((path: string) => Promise.resolve(path.startsWith("api/limit-board/me") ? me : { ...board, next: "cat" }));
    await mount();
    let releaseMore: (v: unknown) => void = () => {};
    api.mockImplementation((path: string) =>
      path.includes("after=") ? new Promise((r) => (releaseMore = r)) : Promise.resolve(path.startsWith("api/limit-board/me") ? me : revoked),
    );
    const more = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find((b) => b.textContent?.includes("さらに表示"))!;
    await act(async () => {
      more.click();
    });
    await visible();
    await act(async () => {
      releaseMore({ enabled: true, next: "", members: [{ userKey: "dan", email: "dan@example.com", kinds: board.members[0].kinds }] });
      await Promise.resolve();
    });
    expect(document.body.textContent).not.toContain("99% 以上");
    expect(document.body.textContent).not.toContain("dan@example.com");
  });

  it("keeps the newer of two reads that finish out of order", async () => {
    await mount();
    let releaseOld: (v: unknown) => void = () => {};
    api.mockImplementationOnce(() => Promise.resolve(me)).mockImplementationOnce(() => new Promise((r) => (releaseOld = r)));
    await visible(); // read 1: its board answer is held
    api.mockImplementation((path: string) => Promise.resolve(path.startsWith("api/limit-board/me") ? me : revoked));
    await visible(); // read 2: the publication was revoked
    await act(async () => {
      releaseOld(board); // read 1 answers last, with the pre-revocation values
      await Promise.resolve();
    });
    expect(document.body.textContent).not.toContain("99% 以上");
  });

  it("hides values when a re-read fails", async () => {
    await mount();
    expect(document.body.textContent).toContain("99% 以上");
    api.mockImplementation(() => Promise.resolve({ error: { code: "http_500", message: "x" } }));
    await visible();
    expect(document.body.textContent).not.toContain("99% 以上");
    expect(document.body.textContent).toContain("再確認できなかった");
  });

  it("draws nobody's values once a re-read finds the board switched off", async () => {
    await mount();
    api.mockImplementation((path: string) =>
      Promise.resolve(path.startsWith("api/limit-board/me") ? { ...me, enabled: false } : { ...board, enabled: false }),
    );
    await visible();
    expect(document.body.textContent).not.toContain("99% 以上");
    expect(document.body.textContent).not.toContain("amy@example.com");
  });
});
