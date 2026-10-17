// The purge dialog (#1982): it shows what would be rewritten from the preview, says the exported
// bundles cannot be recalled, and calls the purge route only once the memory's name is typed —
// with the preview's plan, and the project id also in the query for the audit.
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

import { PurgeHistoryDialog } from "./memoryPurge.tsx";
import { ToastProvider } from "../../../ui/ToastProvider.tsx";
import type { PurgePreview } from "./memoryTypes.ts";

let root: Root | null = null;
let host: HTMLDivElement | null = null;
const onClose = vi.fn();
const onDone = vi.fn();

const preview: PurgePreview = {
  scope: "project",
  name: "build-cmd",
  live: true,
  keepsCurrent: true,
  nothing: false,
  changes: [{ commit: "0123456789abcdef", at: "2026-10-01T00:00:00Z", op: "update", authorKind: "claude" }],
  changesTotal: 2,
  snapshots: 1,
  refs: [
    { name: "refs/heads/main", kind: "main", affected: 3 },
    { name: "refs/imports/20261001T000000Z/main", kind: "import", affected: 2 },
    { name: "refs/premigrate/20261001T000000Z", kind: "rescue", affected: 0 },
  ],
  findings: [{ path: "af/projects/app/build-cmd.md", line: 4, rule: "github-token", hint: "ghp_****" }],
  plan: "abc123",
};

const flush = () =>
  act(async () => {
    for (let i = 0; i < 4; i++) await Promise.resolve();
  });

const mount = async () => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => {
    root!.render(
      <ToastProvider>
        <PurgeHistoryDialog
          target={{ scope: "project", project: { id: "app-0123456789ab" }, name: "build-cmd", where: "app" }}
          onClose={onClose}
          onDone={onDone}
        />
      </ToastProvider>,
    );
  });
  await flush();
};

const buttons = () => Array.from(document.body.querySelectorAll<HTMLButtonElement>("button"));
const runBtn = () => buttons().find((b) => /Purge history|履歴から消去する/.test(b.textContent ?? ""))!;

const type = async (value: string) => {
  const input = document.body.querySelector<HTMLInputElement>(".mem-purge input")!;
  const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  await act(async () => {
    set.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
};

beforeEach(() => {
  api.mockResolvedValue(preview);
  apiJSON.mockResolvedValue({ name: "build-cmd" });
});

afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  api.mockReset();
  apiJSON.mockReset();
  onClose.mockReset();
  onDone.mockReset();
  document.body.innerHTML = "";
});

describe("PurgeHistoryDialog", () => {
  it("asks for the preview of the right memory and shows what would be rewritten", async () => {
    await mount();
    expect(api).toHaveBeenCalledWith(
      "api/agents/memory/entries/purge-preview?scope=project&project=app-0123456789ab&name=build-cmd",
    );
    const text = document.body.textContent ?? "";
    expect(text).toContain("refs/heads/main");
    expect(text).toContain("refs/imports/20261001T000000Z/main");
    // A ref with nothing to rewrite is not listed.
    expect(text).not.toContain("refs/premigrate");
    expect(text).toContain("github-token");
    expect(text).toContain("ghp_****");
    // The bundles warning is on screen before the button can be pressed.
    expect(text).toMatch(/cannot be recalled|取り消せません/);
  });

  it("does not purge until the memory's name is typed", async () => {
    await mount();
    expect(runBtn().disabled).toBe(true);
    await type("build");
    expect(runBtn().disabled).toBe(true);
    await act(async () => runBtn().click());
    expect(apiJSON).not.toHaveBeenCalled();
    await type("build-cmd");
    expect(runBtn().disabled).toBe(false);
  });

  it("sends the plan, and the project in the query, then reports done", async () => {
    await mount();
    await type("build-cmd");
    await act(async () => runBtn().click());
    await flush();
    expect(apiJSON).toHaveBeenCalledWith("api/agents/memory/entries/purge?project=app-0123456789ab", "POST", {
      scope: "project",
      project: "app-0123456789ab",
      name: "build-cmd",
      plan: "abc123",
      confirm: "build-cmd",
    });
    expect(onDone).toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
  });

  it("keeps the dialog open and says why when the Agent refuses", async () => {
    apiJSON.mockResolvedValue({ error: { code: "memory_conflict", message: "the history changed since the preview; review it again" } });
    await mount();
    await type("build-cmd");
    await act(async () => runBtn().click());
    await flush();
    expect(document.body.textContent).toContain("the history changed since the preview");
    expect(onClose).not.toHaveBeenCalled();
    expect(onDone).not.toHaveBeenCalled();
  });

  it("offers nothing to press when there is no past revision", async () => {
    api.mockResolvedValue({ ...preview, nothing: true });
    await mount();
    // No confirmation field is even offered.
    expect(document.body.querySelector(".mem-purge input")).toBeNull();
    expect(runBtn().disabled).toBe(true);
  });
});
