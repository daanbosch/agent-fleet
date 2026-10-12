// The muse import panel (#1991). Pinned here: the shared-through-git warning shows before the
// confirm, an item AF has its own version of is listed and never sent, the confirm needs the
// switch on, and the request carries the working copy name and the project id in the query.
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

import { MuseImportPanel } from "./memoryMuseImport.tsx";
import { ToastProvider } from "../../../ui/ToastProvider.tsx";
import { setSetting } from "../../../lib/settings.ts";

let root: Root | null = null;
let host: HTMLDivElement | null = null;

const project = { id: "app-0123456789ab", root: "/r/app", vcs: "git", display: "app" };
const sources = [
  { dir: "app", count: 4, shared: 1, project },
  { dir: "orphan", count: 1, shared: 0, reason: "no_project" },
];
const previewBody = {
  dir: "app",
  project,
  counts: { new: 2, diverged: 1, secret: 1 },
  items: [
    { name: "team-note", status: "new", sourceHash: "h1", shared: true },
    { name: "my-note", status: "new", sourceHash: "h2" },
    { name: "edited-in-af", status: "diverged", reason: "af_changed", sourceHash: "h3" },
    { name: "has-key", status: "secret", findings: [{ path: "file", line: 4, rule: "github-token", hint: "ghp_…(40)" }] },
  ],
};

const flush = () =>
  act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });

async function mount() {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => {
    root!.render(
      <ToastProvider>
        <MuseImportPanel reload={0} onChanged={() => {}} />
      </ToastProvider>,
    );
  });
  await flush();
}

async function pickSource() {
  const sel = host!.querySelector<HTMLSelectElement>("select")!;
  await act(async () => {
    sel.value = "app";
    sel.dispatchEvent(new Event("change", { bubbles: true }));
  });
  await flush();
}

const applyButton = () => host!.querySelector<HTMLButtonElement>(".mem-ci-actions button")!;

beforeEach(() => {
  setSetting("locale", "en");
  setSetting("agentMemory", true);
  api.mockImplementation((path: string) =>
    Promise.resolve(path.startsWith("api/agents/memory/muse-import/preview") ? previewBody : { sources }),
  );
  apiJSON.mockImplementation((_p: string, _m: string, body: { items: { name: string }[] }) =>
    Promise.resolve({ results: body.items.map((i) => ({ name: i.name, result: "imported" })) }),
  );
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

describe("MuseImportPanel", () => {
  it("leaves a working copy with no project unselectable", async () => {
    await mount();
    const opts = Array.from(host!.querySelectorAll<HTMLOptionElement>("option"));
    expect(opts.find((o) => o.value === "app")?.disabled).toBe(false);
    expect(opts.find((o) => o.value === "orphan")?.disabled).toBe(true);
  });

  it("warns about files shared through git and marks them", async () => {
    await mount();
    await pickSource();
    expect(api.mock.calls.some(([p]) => p === "api/agents/memory/muse-import/preview?dir=app")).toBe(true);
    expect(host!.textContent).toContain("tracked by git");
    expect(host!.querySelectorAll(".mem-badge")).not.toHaveLength(0);
    expect(host!.textContent).toContain("1 of these are shared through git");
    expect(host!.textContent).toContain("the Agent Fleet copy was saved or changed here after the import");
    expect(host!.querySelector(".mem-findings")?.textContent).toContain("ghp_…(40)");
  });

  it("sends only new and update items, with the dir and the project id in the query", async () => {
    await mount();
    await pickSource();
    expect(applyButton().textContent).toContain("2");
    await act(async () => applyButton().click());
    await flush();
    expect(apiJSON).toHaveBeenCalledTimes(1);
    const [path, method, body] = apiJSON.mock.calls[0];
    expect(path).toBe("api/agents/memory/muse-import?project=app-0123456789ab");
    expect(method).toBe("POST");
    expect(body).toMatchObject({ project: "app-0123456789ab", dir: "app" });
    expect(body.items.map((i: { name: string }) => i.name)).toEqual(["team-note", "my-note"]);
  });

  it("needs the switch on to import", async () => {
    setSetting("agentMemory", false);
    await mount();
    await pickSource();
    expect(applyButton().disabled).toBe(true);
    expect(host!.textContent).toContain("Turn on the Agent Fleet memory switch");
  });
});
