// The Memory snapshots entry (ADR 0022): a status header over History / Targets / Export-import.
// Pinned here: where each control lives after the split (the codex memories toggle and Automatic
// snapshots in Targets, Restore this point in History, the bundle export in Export / import),
// view switching, the remembered view, deep links, and that none of Agent Fleet's own memory
// controls came along.
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

const api = vi.fn();
const apiJSON = vi.fn();
vi.mock("../../../core/api/client.ts", async (importActual) => ({
  ...(await importActual<typeof import("../../../core/api/client.ts")>()),
  api: (...args: unknown[]) => api(...args),
  apiJSON: (...args: unknown[]) => apiJSON(...args),
}));
vi.mock("../../../ui/ToastProvider.tsx", () => ({ useToast: () => () => {} }));
vi.mock("../../../ui/ConfirmProvider.tsx", () => ({ useConfirm: () => async () => true }));
const ws = { state: "running" };
const startWs = vi.fn();
vi.mock("../../../core/store/workspace.ts", () => ({
  useWorkspaceStore: (sel: (s: { state: string; start: () => void }) => unknown) =>
    sel({ state: ws.state, start: startWs }),
  wsStartBusy: () => false,
}));

const { MemorySnapshotsTab } = await import("./MemorySnapshotsTab.tsx");
const { useSettingsUI } = await import("../store.ts");
const { t } = await import("../../../lib/i18n/index.ts");

let root: Root | null = null;
let host: HTMLDivElement | null = null;

const flush = () =>
  act(async () => {
    for (let i = 0; i < 4; i++) await Promise.resolve();
  });
async function mount() {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => root!.render(<MemorySnapshotsTab />));
  await flush();
}
function unmount() {
  act(() => root?.unmount());
  host?.remove();
  root = host = null;
}
const tab = (key: string) => host!.querySelector<HTMLButtonElement>(`[role=tab][id$="-tab-${key}"]`)!;
const goto = async (key: string) => {
  await act(async () => tab(key).click());
  await flush();
};
const text = () => host!.querySelector("[role=tabpanel]")?.textContent ?? "";
const selectedTab = () => host!.querySelector("[role=tab][aria-selected=true]")?.id.replace(/^.*-tab-/, "");

beforeEach(() => {
  ws.state = "running";
  localStorage.clear();
  api.mockImplementation((path: string) => {
    if (path.startsWith("api/agents/memory/roots"))
      return Promise.resolve({
        roots: [{ kind: "claude", label: "Claude", scopes: true, files: 3, bytes: 100, projects: [] }],
        inactive: [{ kind: "codex", label: "Codex", reason: "codex_memories_disabled", toggleable: true, enabled: false }],
        auto: true,
        autoLocked: false,
        lastSnapshot: "2026-10-04T10:00:00Z",
      });
    if (path.startsWith("api/agents/memory/snapshots"))
      return Promise.resolve({
        snapshots: [
          { rev: "r1", short: "r1", at: "2026-10-04T10:00:00Z", subject: "s", trigger: "auto", kinds: ["claude"], projects: [], files: 3 },
        ],
      });
    return Promise.resolve({ diff: "" });
  });
});

afterEach(() => {
  unmount();
  act(() => useSettingsUI.getState().closeSettings());
  api.mockReset();
  apiJSON.mockReset();
  startWs.mockClear();
});

describe("MemorySnapshotsTab", () => {
  it("opens on History with Restore this point, and no other view's controls", async () => {
    await mount();
    expect(Array.from(host!.querySelectorAll("[role=tab]")).map((b) => b.textContent)).toEqual([
      t("mem.view_history"),
      t("mem.view_targets"),
      t("mem.view_transfer"),
    ]);
    expect(selectedTab()).toBe("history");
    expect(text()).toContain(t("mem.restore"));
    expect(text()).not.toContain(t("mem.auto_label"));
    expect(text()).not.toContain(t("mem.export_do"));
  });

  it("keeps the status header on every view: last snapshot, automatic on, Snapshot now", async () => {
    await mount();
    for (const v of ["history", "targets", "transfer"]) {
      await goto(v);
      const head = host!.querySelector(".mem-status")!.textContent!;
      expect(head, v).toContain(t("mem.status_auto_on"));
      expect(head, v).toContain(t("mem.snapshot_now"));
      expect(head, v).not.toContain(t("mem.never"));
    }
  });

  // An unread or failed roots load is not "automatic off" and not "no snapshots yet".
  it("asserts nothing in the header until the roots are read", async () => {
    let release: (v: unknown) => void = () => {};
    const rootsReply = new Promise((r) => (release = r));
    const base = api.getMockImplementation()!;
    api.mockImplementation((path: string) => (path.startsWith("api/agents/memory/roots") ? rootsReply : base(path)));
    await mount();
    const head = () => host!.querySelector(".mem-status")!.textContent!;
    expect(head()).not.toContain(t("mem.status_auto_off"));
    expect(head()).not.toContain(t("mem.status_auto_on"));
    expect(head()).not.toContain(t("mem.never"));
    await act(async () => release({ roots: [], auto: true, autoLocked: false, lastSnapshot: "2026-10-04T10:00:00Z" }));
    await flush();
    expect(head()).toContain(t("mem.status_auto_on"));
  });

  it("asserts nothing when the roots fail to load", async () => {
    const base = api.getMockImplementation()!;
    api.mockImplementation((path: string) =>
      path.startsWith("api/agents/memory/roots") ? Promise.resolve({ error: { code: "boom", message: "boom" } }) : base(path),
    );
    await mount();
    const head = host!.querySelector(".mem-status")!.textContent!;
    expect(head).not.toContain(t("mem.status_auto_off"));
    expect(head).not.toContain(t("mem.never"));
    expect(host!.querySelector(".mem-warn")).toBeTruthy();
  });

  it("puts the roots, the codex memories toggle and Automatic snapshots under Targets", async () => {
    await mount();
    await goto("targets");
    expect(text()).toContain("Claude");
    expect(text()).toContain("Codex");
    // The codex toggle is the on/off control on its root's row.
    expect(host!.querySelector(".mem-root-off .seg-btn")).toBeTruthy();
    expect(text()).toContain(t("mem.codex_cost_hint"));
    expect(text()).toContain(t("mem.auto_label"));
    expect(text()).not.toContain(t("mem.restore"));
  });

  it("puts the bundle export under Export / import", async () => {
    await mount();
    await goto("transfer");
    expect(text()).toContain(t("mem.export_do"));
    expect(text()).not.toContain(t("mem.auto_label"));
  });

  it("carries none of Agent Fleet's own memory controls", async () => {
    await mount();
    for (const v of ["history", "targets", "transfer"]) {
      await goto(v);
      expect(host!.textContent, v).not.toContain(t("mem.af_switch"));
      expect(host!.textContent, v).not.toContain(t("mem.br_title"));
      expect(host!.textContent, v).not.toContain(t("mem.pin_title"));
    }
  });

  it("remembers its view apart from the Agent memory entry's", async () => {
    localStorage.setItem("af-settings-views", JSON.stringify({ memory: "changes" }));
    await mount();
    expect(selectedTab()).toBe("history");
    await goto("targets");
    unmount();
    await mount();
    expect(selectedTab()).toBe("targets");
    expect(JSON.parse(localStorage.getItem("af-settings-views")!)).toEqual({
      memory: "changes",
      memorysnapshots: "targets",
    });
  });

  it("opens the view a deep link names", async () => {
    act(() => useSettingsUI.getState().openSettings("memorysnapshots:transfer"));
    await mount();
    expect(selectedTab()).toBe("transfer");
  });

  it("offers Start and calls nothing while the workspace is stopped", async () => {
    ws.state = "stopped";
    await mount();
    expect(host!.textContent).toContain(t("mem.ws_required_title"));
    expect(host!.querySelector("[role=tab]")).toBeNull();
    expect(host!.textContent).not.toContain(t("mem.af_switch"));
    const start = Array.from(host!.querySelectorAll<HTMLButtonElement>("button")).find(
      (b) => b.textContent === t("mem.start_ws"),
    )!;
    await act(async () => start.click());
    expect(startWs).toHaveBeenCalledTimes(1);
    expect(api).not.toHaveBeenCalled();
  });
});
