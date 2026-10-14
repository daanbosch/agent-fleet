// The AF memory of an imported bundle (#1983). Pinned here: nothing is shown for a bundle without
// AF memory, an unmatched project is listed and never sent, only new memories of matched scopes go
// out (in batches, with the preview's hash and target), and the import id rides the query for the
// audit ledger.
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

import { BundleAfPanel, BUNDLE_BATCH } from "./memoryBundleImport.tsx";
import { ToastProvider } from "../../../ui/ToastProvider.tsx";
import { t } from "../../../lib/i18n/index.ts";

let root: Root | null = null;
let host: HTMLDivElement | null = null;
const flush = () =>
  act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
async function mount(onChanged = () => {}) {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => {
    root!.render(
      <ToastProvider>
        <BundleAfPanel importId="20261014T093000Z" onChanged={onChanged} />
      </ToastProvider>,
    );
  });
  await flush();
}
const item = (name: string, status: string, extra = {}) => ({ name, status, sourceHash: "h-" + name, ...extra });
const target = { id: "demo-1", display: "demo", root: "/r/demo", vcs: "git" };

beforeEach(() => {
  apiJSON.mockResolvedValue({ results: [] });
});
afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  api.mockReset();
  apiJSON.mockReset();
  document.body.innerHTML = "";
});

describe("BundleAfPanel", () => {
  it("shows nothing for a bundle without AF memory", async () => {
    api.mockResolvedValue({ importId: "x", hasAF: false, scopes: [] });
    await mount();
    expect(host!.textContent).toBe("");
  });

  it("lists matched and unmatched projects and sends only the new memories of matched scopes", async () => {
    api.mockResolvedValue({
      importId: "20261014T093000Z",
      hasAF: true,
      scopes: [
        {
          source: "src-1",
          display: "demo",
          target,
          match: "remote",
          counts: { new: 2, exists: 1 },
          items: [item("a", "new"), item("b", "new"), item("c", "exists", { reason: "local_differs" })],
        },
        { source: "lost-1", display: "lost", reason: "no_local_project", counts: { skipped: 3 }, items: [] },
        { source: "user", match: "user", counts: { new: 1, secret: 1 }, items: [item("u", "new"), item("s", "secret", { findings: [{ path: "file", line: 1, rule: "github-token", hint: "ghp_…" }] })] },
      ],
    });
    const changed = vi.fn();
    await mount(changed);
    const text = host!.textContent ?? "";
    expect(text).toContain(t("mem.bi_no_local", { n: 3 }));
    expect(text).toContain(t("mem.bi_by_remote"));
    expect(text).toContain(t("mem.bi_user_scope"));
    expect(text).toContain(t("mem.bi_status_exists"));
    expect(text).toContain("github-token");
    const btn = Array.from(host!.querySelectorAll("button")).find((b) => b.textContent === t("mem.bi_apply", { n: 3 }));
    expect(btn).toBeTruthy();
    apiJSON.mockResolvedValue({ results: [{ scope: "src-1", name: "a", result: "imported" }] });
    await act(async () => btn!.click());
    await flush();
    expect(apiJSON).toHaveBeenCalledTimes(1);
    const [url, method, body] = apiJSON.mock.calls[0];
    expect(url).toBe("api/agents/memory/af-import?importId=20261014T093000Z");
    expect(method).toBe("POST");
    expect(body.items).toEqual([
      { scope: "src-1", target: "demo-1", name: "a", sourceHash: "h-a" },
      { scope: "src-1", target: "demo-1", name: "b", sourceHash: "h-b" },
      { scope: "user", target: "", name: "u", sourceHash: "h-u" },
    ]);
    expect(JSON.stringify(body)).not.toContain("lost-1");
    expect(changed).toHaveBeenCalled();
  });

  it("sends a large import in batches of the Agent's cap", async () => {
    const items = Array.from({ length: BUNDLE_BATCH + 5 }, (_, i) => item("m" + i, "new"));
    api.mockResolvedValue({
      importId: "20261014T093000Z",
      hasAF: true,
      scopes: [{ source: "src-1", display: "demo", target, match: "id", counts: { new: items.length }, items }],
    });
    await mount();
    const btn = Array.from(host!.querySelectorAll("button")).find((b) => b.textContent === t("mem.bi_apply", { n: items.length }));
    await act(async () => btn!.click());
    await flush();
    expect(apiJSON).toHaveBeenCalledTimes(2);
    expect(apiJSON.mock.calls[0][2].items).toHaveLength(BUNDLE_BATCH);
    expect(apiJSON.mock.calls[1][2].items).toHaveLength(5);
  });
});
