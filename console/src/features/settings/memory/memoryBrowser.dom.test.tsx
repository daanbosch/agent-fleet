// The memory browser (#1985): rows with author / uses / pin, search, body view, forget and
// restore through the existing revert route (commit in the query for the audit). Withheld memories are a count, never a name.
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

import { MemoryBrowserPanel } from "./memoryBrowser.tsx";
import { ConfirmProvider } from "../../../ui/ConfirmProvider.tsx";
import { ToastProvider } from "../../../ui/ToastProvider.tsx";

let root: Root | null = null;
let host: HTMLDivElement | null = null;

const project = { id: "app-0123456789ab", root: "/r/app", vcs: "git", display: "app" };
const entries = [
  {
    name: "rules",
    scope: "user",
    description: "house rules",
    type: "feedback",
    updated: "2026-10-01T00:00:00Z",
    pinned: true,
    uses: 7,
    revision: 3,
    authorKind: "claude",
    authorSession: "claude-main",
  },
  {
    name: "build-cmd",
    scope: "project",
    project,
    description: "how to build",
    type: "project",
    updated: "2026-10-02T00:00:00Z",
    pinned: false,
    uses: 2,
    revision: 1,
    authorKind: "member",
    authorSession: "console",
  },
];
const changes = [
  {
    commit: "livecommit1",
    at: "2026-10-02T00:00:00Z",
    op: "create",
    scope: "project",
    project,
    name: "build-cmd",
    authorKind: "member",
    authorSession: "console",
    latest: true,
    live: true,
    revertible: true,
  },
  {
    commit: "livecommit0",
    at: "2026-10-01T00:00:00Z",
    op: "create",
    scope: "user",
    name: "rules",
    authorKind: "claude",
    authorSession: "claude-main",
    latest: true,
    live: true,
    revertible: true,
  },
  {
    commit: "abcdef1234",
    at: "2026-10-03T00:00:00Z",
    op: "forget",
    scope: "project",
    project,
    name: "old-note",
    authorKind: "member",
    authorSession: "console",
    latest: true,
    live: false,
    revertible: true,
  },
];

const flush = () =>
  act(async () => {
    for (let i = 0; i < 4; i++) await Promise.resolve();
  });
const confirmButton = () => document.body.querySelector<HTMLButtonElement>(".ui-confirm-actions button:last-child");
const click = async (b: Element | null | undefined) => {
  expect(b).toBeTruthy();
  await act(async () => (b as HTMLButtonElement).click());
  await flush();
};
const text = (label: string) =>
  Array.from(document.body.querySelectorAll<HTMLButtonElement>("button")).find((b) => b.textContent === label);
const select = () => document.body.querySelector<HTMLSelectElement>(".mem-br-bar select")!;
async function setValue(el: HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement, v: string) {
  const proto = Object.getPrototypeOf(el);
  await act(async () => {
    Object.getOwnPropertyDescriptor(proto, "value")!.set!.call(el, v);
    el.dispatchEvent(new Event(el instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
  });
}

beforeEach(async () => {
  api.mockImplementation(async (url: string) => {
    if (url.startsWith("api/agents/memory/entries/list")) return { entries, withheld: 2 };
    if (url.startsWith("api/agents/memory/entries/changes")) return { changes };
    if (url.startsWith("api/agents/memory/entries/member-read"))
      return { ...entries[0], body: "always run the tests" };
    return {};
  });
  apiJSON.mockResolvedValue({ name: "x" });
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => {
    root!.render(
      <ToastProvider>
        <ConfirmProvider>
          <MemoryBrowserPanel reload={0} onChanged={() => {}} />
        </ConfirmProvider>
      </ToastProvider>,
    );
  });
  await flush();
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

describe("MemoryBrowserPanel", () => {
  it("lists the selected scope with author, uses and pin, and only a count for withheld ones", () => {
    const rows = Array.from(document.body.querySelectorAll(".mem-br-row"));
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain("rules");
    expect(rows[0].textContent).toContain("claude · claude-main");
    expect(rows[0].textContent).toContain("7");
    expect(rows[0].textContent).toMatch(/pinned|ピン留め/);
    expect(document.body.textContent).toMatch(/2 /);
  });

  it("searches name, description, type and author", async () => {
    const input = document.body.querySelector<HTMLInputElement>('input[type="search"]')!;
    await setValue(input, "no-such");
    expect(document.body.querySelectorAll(".mem-br-row")).toHaveLength(0);
    await setValue(input, "HOUSE");
    expect(document.body.querySelectorAll(".mem-br-row")).toHaveLength(1);
    await setValue(input, "claude-main");
    expect(document.body.querySelectorAll(".mem-br-row")).toHaveLength(1);
  });

  it("shows a body from the member-read route", async () => {
    await click(text("View") ?? text("表示"));
    expect(api).toHaveBeenCalledWith(
      "api/agents/memory/entries/member-read?scope=user&project=&name=rules",
    );
    expect(document.body.querySelector(".mem-br-body")!.textContent).toBe("always run the tests");
  });

  it("forgets from the list through the revert route on the memory's newest change", async () => {
    await setValue(select(), "project:" + project.id);
    expect(document.body.querySelectorAll(".mem-br-row")[0].textContent).toContain("build-cmd");
    await click(document.body.querySelectorAll(".mem-br-row")[0].querySelectorAll("button")[1]);
    await click(confirmButton());
    expect(apiJSON).toHaveBeenCalledWith("api/agents/memory/entries/revert?commit=livecommit1", "POST", {
      commit: "livecommit1",
      forget: true,
      ack: false,
    });
  });

  it("offers no forget when the memory's newest change is outside the loaded window", async () => {
    expect(document.body.querySelectorAll(".mem-br-row")[0].querySelectorAll("button")[1].disabled).toBe(false);
    api.mockImplementation(async (url: string) =>
      url.startsWith("api/agents/memory/entries/list") ? { entries } : { changes: [] },
    );
    await act(async () => root!.unmount());
    root = createRoot(host!);
    await act(async () => {
      root!.render(
        <ToastProvider>
          <ConfirmProvider>
            <MemoryBrowserPanel reload={0} onChanged={() => {}} />
          </ConfirmProvider>
        </ToastProvider>,
      );
    });
    await flush();
    expect(document.body.querySelectorAll(".mem-br-row")[0].querySelectorAll("button")[1].disabled).toBe(true);
  });

  it("restores a forgotten memory through the revert route", async () => {
    await setValue(select(), "project:" + project.id);
    expect(document.body.textContent).toContain("old-note");
    const restore = Array.from(document.body.querySelectorAll<HTMLButtonElement>(".mem-br-row button")).at(-1);
    await click(restore);
    expect(apiJSON).toHaveBeenCalledWith("api/agents/memory/entries/revert?commit=abcdef1234", "POST", {
      commit: "abcdef1234",
      forget: false,
      ack: false,
    });
  });
});
