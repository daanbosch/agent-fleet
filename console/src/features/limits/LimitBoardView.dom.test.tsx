// The board as a viewer sees it (ADR 0111 decision 6). Pinned: a member with nothing received
// reads "unknown" (never available), members stay in roster order whatever their headroom, and
// the member's own setting saves for this tenant only.
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

const api = vi.fn();
const apiJSON = vi.fn();
const toast = vi.fn();
vi.mock("../../core/api/client.ts", () => ({
  api: (...args: unknown[]) => api(...args),
  apiJSON: (...args: unknown[]) => apiJSON(...args),
  errText: (e: { message?: string }) => e?.message || "",
}));
vi.mock("../../ui/ToastProvider.tsx", () => ({ useToast: () => toast }));

import { LimitBoardView } from "./LimitBoardView.tsx";

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
});
