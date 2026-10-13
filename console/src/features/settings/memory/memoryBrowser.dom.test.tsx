// The memory browser (#1985): rows with author / uses / pin, search, body view, forget and
// restore through the existing revert route (commit in the query for the audit), and the save form
// (#1997) with its secret refusal. Withheld memories are a count, never a name.
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
let readKinds: string[] = [];

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
// The memory list proper; the forgotten list below it reuses the row class.
const listRows = () => Array.from(document.body.querySelectorAll(".mem-br-list")[0].querySelectorAll(".mem-br-row"));
const select = () => document.body.querySelector<HTMLSelectElement>(".mem-br-bar select")!;
async function setValue(el: HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement, v: string) {
  const proto = Object.getPrototypeOf(el);
  await act(async () => {
    Object.getOwnPropertyDescriptor(proto, "value")!.set!.call(el, v);
    el.dispatchEvent(new Event(el instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
  });
}

beforeEach(async () => {
  readKinds = [];
  api.mockImplementation(async (url: string) => {
    if (url.startsWith("api/agents/memory/entries/list")) return { entries, withheld: 2 };
    if (url.startsWith("api/agents/memory/entries/member-forgotten")) {
      const u = new URL("http://x/" + url);
      const sc = u.searchParams.get("scope");
      const pid = u.searchParams.get("project");
      return {
        forgotten: changes.filter(
          (c) => c.latest && !c.live && c.revertible && (!sc || (c.scope === sc && (sc === "user" || c.project?.id === pid))),
        ),
      };
    }
    if (url.startsWith("api/agents/memory/entries/member-latest")) {
      const u = new URL("http://x/" + url);
      const c = changes.find((ch) => ch.name === u.searchParams.get("name") && ch.latest);
      return c ?? { error: { code: "memory_not_found", message: "none" } };
    }
    if (url.startsWith("api/agents/memory/entries/member-search"))
      return { hits: [{ ...entries[0], snippets: ["the zebra rule"] }] };
    if (url.startsWith("api/agents/memory/entries/member-read"))
      return { ...entries[0], kinds: readKinds, body: "always run the tests" };
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
  it("opens on all scopes, tagging each row with its scope or project", () => {
    expect(select().value).toBe("all");
    const rows = listRows();
    expect(rows.map((r) => r.querySelector(".mem-af-name")!.textContent)).toEqual(["rules", "build-cmd"]);
    expect(rows[0].querySelector(".mem-br-where")!.textContent).toMatch(/User-wide|ユーザー共通/);
    expect(rows[1].querySelector(".mem-br-where")!.textContent).toBe("app");
  });

  it("lists the selected scope with author, uses and pin, and only a count for withheld ones", async () => {
    await setValue(select(), "user");
    const rows = listRows();
    expect(rows).toHaveLength(1);
    expect(rows[0].querySelector(".mem-br-where")).toBeNull();
    expect(rows[0].textContent).toContain("rules");
    expect(rows[0].textContent).toContain("claude · claude-main");
    expect(rows[0].textContent).toContain("7");
    expect(rows[0].textContent).toMatch(/pinned|ピン留め/);
    expect(document.body.textContent).toMatch(/2 /);
  });

  it("searches name, description, type and author", async () => {
    const input = document.body.querySelector<HTMLInputElement>('input[type="search"]')!;
    await setValue(input, "no-such");
    expect(listRows()).toHaveLength(0);
    await setValue(input, "HOUSE");
    expect(listRows()).toHaveLength(1);
    // The filter runs over every scope: a project-scoped memory matches too.
    await setValue(input, "how to build");
    expect(listRows()[0].querySelector(".mem-af-name")!.textContent).toBe("build-cmd");
    await setValue(input, "claude-main");
    expect(listRows()).toHaveLength(1);
  });

  it("asks the body search after a pause and shows the matching lines of a memory the list fields miss", async () => {
    const input = document.body.querySelector<HTMLInputElement>('input[type="search"]')!;
    const searchCalls = () => api.mock.calls.filter((c) => String(c[0]).startsWith("api/agents/memory/entries/member-search"));
    await setValue(input, "zeb");
    await setValue(input, "zebra");
    // Nothing is asked while the user is still typing, and the list fields match nothing yet.
    expect(searchCalls()).toHaveLength(0);
    expect(listRows()).toHaveLength(0);
    await act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });
    await flush();
    expect(searchCalls()).toHaveLength(1);
    expect(searchCalls()[0][0]).toBe("api/agents/memory/entries/member-search?limit=50&q=zebra");
    const rows = listRows();
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain("rules");
    expect(rows[0].textContent).toContain("the zebra rule");
    // Clearing the box brings the plain list back and drops the snippets.
    await setValue(input, "");
    expect(listRows()[0].textContent).not.toContain("the zebra rule");
  });

  it("orders body-only hits by the server's rank, after the instant field matches", async () => {
    const more = [
      ...entries,
      { ...entries[0], name: "second", description: "other", authorKind: "codex", authorSession: "c1" },
      { ...entries[0], name: "third", description: "other", authorKind: "codex", authorSession: "c2" },
    ];
    const base = api.getMockImplementation()!;
    api.mockImplementation(async (url: string) => {
      if (url.startsWith("api/agents/memory/entries/list")) return { entries: more };
      if (url.startsWith("api/agents/memory/entries/member-search"))
        return { hits: [{ ...more[3], snippets: ["zebra 3"] }, { ...more[2], snippets: ["zebra 2"] }, { ...more[0], snippets: ["zebra 1"] }] };
      return base(url);
    });
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
    await setValue(document.body.querySelector<HTMLInputElement>('input[type="search"]')!, "zebra");
    await act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });
    await flush();
    const names = listRows().map((r) => r.querySelector(".mem-af-name")!.textContent);
    // listed order is rules, second, third; the server ranked third, second, rules
    expect(names).toEqual(["third", "second", "rules"]);
  });

  it("returns project-scoped body hits for a query under the default selection, without a scope param", async () => {
    const base = api.getMockImplementation()!;
    api.mockImplementation(async (url: string) => {
      if (url.startsWith("api/agents/memory/entries/member-search"))
        return { hits: [{ ...entries[1], snippets: ["compile with zebra"] }] };
      return base(url);
    });
    await setValue(document.body.querySelector<HTMLInputElement>('input[type="search"]')!, "zebra");
    await act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });
    await flush();
    const calls = api.mock.calls.filter((c) => String(c[0]).startsWith("api/agents/memory/entries/member-search"));
    expect(calls).toHaveLength(1);
    expect(calls[0][0]).not.toContain("scope=");
    const rows = listRows();
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain("build-cmd");
    expect(rows[0].textContent).toContain("compile with zebra");
    expect(rows[0].querySelector(".mem-br-where")!.textContent).toBe("app");
  });

  it("scopes the body search to the user scope when it is chosen", async () => {
    await setValue(select(), "user");
    await setValue(document.body.querySelector<HTMLInputElement>('input[type="search"]')!, "zebra");
    await act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });
    expect(api).toHaveBeenCalledWith("api/agents/memory/entries/member-search?scope=user&limit=50&q=zebra");
  });

  it("scopes the body search to the selected project", async () => {
    await setValue(select(), "project:" + project.id);
    await setValue(document.body.querySelector<HTMLInputElement>('input[type="search"]')!, "zebra");
    await act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });
    expect(api).toHaveBeenCalledWith(
      `api/agents/memory/entries/member-search?scope=project&project=${project.id}&limit=50&q=zebra`,
    );
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
    expect(listRows()[0].textContent).toContain("build-cmd");
    await click(listRows()[0].querySelectorAll("button")[1]);
    await click(confirmButton());
    expect(apiJSON).toHaveBeenCalledWith("api/agents/memory/entries/revert?commit=livecommit1", "POST", {
      commit: "livecommit1",
      forget: true,
      ack: false,
      expectRevision: 1,
    });
  });

  it("forgets a memory whose newest change is outside the loaded changes, resolving it on demand", async () => {
    api.mockImplementation(async (url: string) => {
      if (url.startsWith("api/agents/memory/entries/list")) return { entries };
      if (url.startsWith("api/agents/memory/entries/member-latest"))
        return { ...changes[1], commit: "oldcommit9" };
      return {};
    });
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
    const forgetBtn = listRows()[0].querySelectorAll("button")[1];
    expect(forgetBtn.disabled).toBe(false);
    await click(forgetBtn);
    expect(api).toHaveBeenCalledWith("api/agents/memory/entries/member-latest?scope=user&project=&name=rules");
    await click(confirmButton());
    expect(apiJSON).toHaveBeenCalledWith("api/agents/memory/entries/revert?commit=oldcommit9", "POST", {
      commit: "oldcommit9",
      forget: true,
      ack: false,
      expectRevision: 3,
    });
  });

  it("does not forget when no change can be resolved", async () => {
    api.mockImplementation(async (url: string) => {
      if (url.startsWith("api/agents/memory/entries/list")) return { entries };
      if (url.startsWith("api/agents/memory/entries/member-latest"))
        return { error: { code: "memory_not_found", message: "no published change" } };
      return {};
    });
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
    await click(listRows()[0].querySelectorAll("button")[1]);
    expect(confirmButton()).toBeNull();
    expect(apiJSON).not.toHaveBeenCalled();
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

  it("opens the purge dialog for a forgotten memory and asks for its preview, calling no write", async () => {
    await setValue(select(), "project:" + project.id);
    const purge = Array.from(document.body.querySelectorAll<HTMLButtonElement>(".mem-br-row button")).at(-2);
    expect(purge?.textContent).toMatch(/Purge history|履歴から消去/);
    await click(purge);
    expect(api).toHaveBeenCalledWith(expect.stringContaining("api/agents/memory/entries/purge-preview?scope=project"));
    expect(apiJSON).not.toHaveBeenCalled();
  });

  it("saves an edit at the revision it read, and shows secret findings without saving", async () => {
    await click(text("View") ?? text("表示"));
    await click(text("Edit") ?? text("編集"));
    const body = document.body.querySelector<HTMLTextAreaElement>("textarea")!;
    await setValue(body, "new text");
    apiJSON.mockResolvedValueOnce({
      error: { code: "memory_secret_detected", message: "x" },
      findings: [{ path: "body", line: 1, rule: "aws-key", hint: "AKIA…" }],
    });
    await click(document.body.querySelector<HTMLButtonElement>('button[type="submit"]'));
    expect(apiJSON).toHaveBeenCalledWith("api/agents/memory/entries/member-save", "POST", {
      scope: "user",
      project: "",
      name: "rules",
      description: "house rules",
      type: "feedback",
      kinds: [],
      body: "new text",
      revision: 3,
    });
    expect(document.body.querySelector(".mem-findings")!.textContent).toContain("aws-key");
    expect(document.body.querySelector("form")).toBeTruthy();
  });

  it("creates a memory in the selected project at revision 0, with the project id in the query", async () => {
    await setValue(select(), "project:" + project.id);
    await click(text("New memory") ?? text("新しいメモリ"));
    const inputs = document.body.querySelectorAll<HTMLInputElement>("form input");
    await setValue(inputs[0], "fresh");
    await setValue(inputs[1], "a fact");
    await setValue(document.body.querySelector<HTMLTextAreaElement>("textarea")!, "body");
    await click(document.body.querySelector<HTMLButtonElement>('button[type="submit"]'));
    expect(apiJSON).toHaveBeenCalledWith(
      "api/agents/memory/entries/member-save?project=app-0123456789ab",
      "POST",
      expect.objectContaining({ scope: "project", project: project.id, name: "fresh", revision: 0 }),
    );
    expect(document.body.querySelector("form")).toBeNull();
  });

  it("creates a new memory in the user scope while every scope is shown", async () => {
    await click(text("New memory") ?? text("新しいメモリ"));
    await click(document.body.querySelector<HTMLButtonElement>('button[type="submit"]'));
    expect(apiJSON).toHaveBeenCalledWith(
      "api/agents/memory/entries/member-save",
      "POST",
      expect.objectContaining({ scope: "user", project: "", revision: 0 }),
    );
  });

  it("saves an edit to the scope it was opened in, even if the selector moved", async () => {
    await click(text("View") ?? text("表示"));
    await click(text("Edit") ?? text("編集"));
    await setValue(select(), "project:" + project.id);
    await click(document.body.querySelector<HTMLButtonElement>('button[type="submit"]'));
    expect(apiJSON).toHaveBeenCalledWith(
      "api/agents/memory/entries/member-save",
      "POST",
      expect.objectContaining({ scope: "user", project: "", name: "rules", revision: 3 }),
    );
  });

  it("keeps the kinds of the memory when its body is edited", async () => {
    readKinds = ["claude"];
    await click(text("View") ?? text("表示"));
    await click(text("Edit") ?? text("編集"));
    await click(document.body.querySelector<HTMLButtonElement>('button[type="submit"]'));
    expect(apiJSON).toHaveBeenCalledWith(
      "api/agents/memory/entries/member-save",
      "POST",
      expect.objectContaining({ kinds: ["claude"] }),
    );
  });

  it("asks for the forgotten memories of the scope on screen, not the recent changes", async () => {
    expect(api).toHaveBeenCalledWith("api/agents/memory/entries/member-forgotten");
    expect(api).not.toHaveBeenCalledWith(expect.stringContaining("entries/changes"));
    await setValue(select(), "user");
    expect(api).toHaveBeenCalledWith("api/agents/memory/entries/member-forgotten?scope=user");
    expect(document.body.textContent).not.toContain("old-note");
    await setValue(select(), "project:" + project.id);
    expect(api).toHaveBeenCalledWith(
      `api/agents/memory/entries/member-forgotten?scope=project&project=${encodeURIComponent(project.id)}`,
    );
    expect(document.body.textContent).toContain("old-note");
  });

  it("counts withheld forgotten memories and says when the list is cut", async () => {
    api.mockImplementation(async (url: string) => {
      if (url.startsWith("api/agents/memory/entries/list")) return { entries };
      if (url.startsWith("api/agents/memory/entries/member-forgotten"))
        return { forgotten: [changes[2]], withheld: 3, truncated: true };
      return {};
    });
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
    const warns = Array.from(document.body.querySelectorAll(".mem-warn")).map((e) => e.textContent ?? "");
    expect(warns.some((w) => w.includes("3"))).toBe(true);
    expect(warns.some((w) => w.includes("1"))).toBe(true);
    expect(document.body.textContent).toContain("old-note");
  });

  it("keeps offering a project that holds only forgotten memories, even when its rows were cut or another scope is selected", async () => {
    const userRow = { ...changes[2], scope: "user", project: undefined, name: "u-note", commit: "cu" };
    api.mockImplementation(async (url: string) => {
      if (url.startsWith("api/agents/memory/entries/list")) return { entries: [] };
      if (url.startsWith("api/agents/memory/entries/member-forgotten")) {
        // All scopes: the user row fills the answer, the project's row did not fit but is named.
        if (!url.includes("?")) return { forgotten: [userRow], projects: [project], truncated: true };
        return { forgotten: [userRow], projects: [] };
      }
      return {};
    });
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
    const options = () => Array.from(select().options).map((o) => o.value);
    expect(options()).toEqual(["all", "user", "project:" + project.id]);
    await setValue(select(), "user");
    expect(options()).toEqual(["all", "user", "project:" + project.id]);
    await setValue(select(), "project:" + project.id);
    expect(select().value).toBe("project:" + project.id);
  });

  it("says the forgotten list is incomplete when the Agent ran out of checking budget", async () => {
    api.mockImplementation(async (url: string) => {
      if (url.startsWith("api/agents/memory/entries/list")) return { entries };
      if (url.startsWith("api/agents/memory/entries/member-forgotten")) return { forgotten: [], projects: [], withheld: 1, incomplete: true };
      return {};
    });
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
    const warns = Array.from(document.body.querySelectorAll(".mem-warn")).map((e) => e.textContent ?? "");
    expect(warns.some((w) => w.includes("minimum") || w.includes("最小値"))).toBe(true);
    expect(warns.some((w) => w.includes("Only the newest") || w.includes("新しい順"))).toBe(false);
  });

  it("drops another scope's forgotten rows at once when the scope changes, before the new list arrives", async () => {
    let release: (v: unknown) => void = () => {};
    api.mockImplementation(async (url: string) => {
      if (url.startsWith("api/agents/memory/entries/list")) return { entries };
      if (url === "api/agents/memory/entries/member-forgotten") return { forgotten: [changes[2]], projects: [project] };
      if (url.startsWith("api/agents/memory/entries/member-forgotten?scope=user"))
        return new Promise((resolve) => {
          release = resolve;
        });
      return {};
    });
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
    expect(document.body.textContent).toContain("old-note");
    await setValue(select(), "user");
    expect(api).toHaveBeenCalledWith("api/agents/memory/entries/member-forgotten?scope=user");
    // The user scope's answer is still pending: the project's row and its Restore are gone already.
    expect(document.body.textContent).not.toContain("old-note");
    expect(text("Restore") ?? text("復元")).toBeUndefined();
    await act(async () => release({ forgotten: [], projects: [project] }));
    await flush();
    expect(document.body.textContent).not.toContain("old-note");
  });
});
