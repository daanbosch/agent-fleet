// The Agent memory page while the workspace is not running (#1735). The history and the import
// need the Agent, so the page shows the start prompt instead; the Agent Fleet memory switch is a
// ui-pref the member sets for the next start, and this page is its only home, so it stays.
import { describe, it, expect, afterEach, vi } from "vitest";
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
// The flip asks first (#1972); here the member says yes. What the dialog says is pinned in
// memoryAutoSync.dom.test.tsx.
const askConfirm = vi.fn(async (_opts: unknown) => true);
vi.mock("../../../ui/ConfirmProvider.tsx", () => ({ useConfirm: () => askConfirm }));
vi.mock("../../../core/store/workspace.ts", () => ({
  useWorkspaceStore: (sel: (s: { state: string; start: () => void }) => unknown) => sel({ state: "stopped", start: () => {} }),
  wsStartBusy: () => false,
}));

const { MemoryTab } = await import("./MemoryTab.tsx");
const { setSettings, settingsDefaults, getSettings, normalizeExternalMemory } = await import("../../../lib/settings.ts");
const { t } = await import("../../../lib/i18n/index.ts");

let root: Root | null = null;
let host: HTMLDivElement | null = null;

afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  api.mockReset();
  apiJSON.mockReset();
  askConfirm.mockClear();
  setSettings(settingsDefaults());
});

describe("MemoryTab while the workspace is stopped", () => {
  it("still offers the Agent Fleet memory switch, both ways, without calling the Agent", async () => {
    setSettings(settingsDefaults());
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
    await act(async () => root!.render(<MemoryTab />));

    expect(host.textContent).toContain(t("mem.ws_required_title"));
    const row = Array.from(host.querySelectorAll(".ds-row")).find(
      (r) => r.querySelector(".ds-label")?.textContent === t("mem.af_switch"),
    );
    expect(row).toBeTruthy();
    expect(getSettings().agentMemory).toBe(false);
    const [on, off] = Array.from(row!.querySelectorAll<HTMLButtonElement>(".seg-btn"));
    await act(async () => on.click());
    expect(getSettings().agentMemory).toBe(true);
    await act(async () => off.click());
    expect(getSettings().agentMemory).toBe(false);
    expect(askConfirm).toHaveBeenCalledTimes(2);
    // No Agent to ask: no preview, no wake. It reconciles when the workspace starts.
    expect(api).not.toHaveBeenCalled();
    expect(apiJSON).not.toHaveBeenCalled();
  });

  // #1988: the outside-client setting is off by default, has three states, and needs no Agent and
  // no confirmation.
  it("offers the outside-client setting, off by default, without calling the Agent", async () => {
    setSettings(settingsDefaults());
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
    await act(async () => root!.render(<MemoryTab />));
    const row = Array.from(host.querySelectorAll(".ds-row")).find(
      (r) => r.querySelector(".ds-label")?.textContent === t("mem.ext_switch"),
    );
    expect(row).toBeTruthy();
    expect(getSettings().externalMemory).toBe("off");
    const [, read, write] = Array.from(row!.querySelectorAll<HTMLButtonElement>(".seg-btn"));
    await act(async () => read.click());
    expect(getSettings().externalMemory).toBe("read");
    await act(async () => write.click());
    expect(getSettings().externalMemory).toBe("write");
    expect(getSettings().agentMemory).toBe(false);
    expect(askConfirm).not.toHaveBeenCalled();
    expect(api).not.toHaveBeenCalled();
  });
});

// The Agent reads only "read" and "write"; anything else shown as on would show one thing and
// enforce another.
describe("normalizeExternalMemory", () => {
  it("keeps the two on-states and turns everything else off", () => {
    expect(normalizeExternalMemory("read")).toBe("read");
    expect(normalizeExternalMemory("write")).toBe("write");
    for (const v of [undefined, null, "", "on", "WRITE", true, 1, {}]) expect(normalizeExternalMemory(v)).toBe("off");
  });
});
