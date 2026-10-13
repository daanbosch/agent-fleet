// The right-click menu on file / folder links in the Markdown view: `[text](rel/path)` links
// (wireLinks) and inline-code paths (linkifyPathRefs). Both are DOM-built, so the menu is
// reached through PathLinkMenuHost; a surface without a host or without onOpenFile keeps the
// browser's own menu.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

const toasts: unknown[] = [];
vi.mock("../../ui/ToastProvider.tsx", () => ({
  useToast: () => (m: unknown) => toasts.push(m),
}));

// fs listing for `[text](path)` links and the resolver for inline-code paths.
let listing: { name: string; type: string }[] = [];
let resolved: Record<string, { path: string; type: string }> = {};
vi.mock("../../core/api/client.ts", () => ({
  api: vi.fn(async () => ({ entries: listing })),
  apiJSON: vi.fn(async (_path: string, _method: string, body: { refs: string[] }) => {
    const out: Record<string, { path: string; type: string }> = {};
    for (const ref of body.refs) if (resolved[ref]) out[ref] = resolved[ref];
    return { resolved: out };
  }),
  downloadURL: (p: string) => "/dl/" + p,
}));

const revealed: string[] = [];
vi.mock("../files/store.ts", () => ({
  useFilesStore: {
    getState: () => ({ revealInFiles: (path: string) => revealed.push(path) }),
  },
}));

const { MarkdownView } = await import("./MarkdownView.tsx");
const { PathLinkMenuHost } = await import("./PathLinkMenu.tsx");
const { clearPathRefCache } = await import("./pathResolve.ts");

let host: HTMLDivElement;
let root: Root;
const opened: { path: string; openInNew?: boolean }[] = [];

const render = async (source: string, opts: { menu?: boolean; open?: boolean } = {}) => {
  const view = (
    <MarkdownView
      source={source}
      basePath="repos/x/README.md"
      baseDir="repos/x"
      onOpenFile={
        opts.open === false
          ? undefined
          : (path: string, _l?: number, _c?: number, openInNew?: boolean) => opened.push({ path, openInNew })
      }
    />
  );
  await act(async () => {
    root.render(opts.menu === false ? view : <PathLinkMenuHost>{view}</PathLinkMenuHost>);
  });
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
};
const settle = () =>
  act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
const contextMenu = async (a: Element) => {
  const e = new MouseEvent("contextmenu", { bubbles: true, cancelable: true, clientX: 10, clientY: 10 });
  await act(async () => {
    a.dispatchEvent(e);
  });
  await settle();
  return e;
};
const menuEl = () => document.body.querySelector<HTMLElement>("ul.files-ctxmenu");
const labels = () => [...(menuEl()?.querySelectorAll(".ui-menu-item") ?? [])].map((b) => b.textContent?.trim());
const repoLink = () => host.querySelector<HTMLAnchorElement>("a.repo-link")!;
const pathLink = () => host.querySelector<HTMLAnchorElement>("a.md-path-link")!;

beforeEach(() => {
  toasts.length = 0;
  opened.length = 0;
  revealed.length = 0;
  listing = [];
  resolved = {};
  clearPathRefCache();
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});

afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
});

describe("[text](path) link menu", () => {
  it("opens the menu on right click and opens the file in a new pane from it", async () => {
    listing = [{ name: "a.md", type: "file" }];
    await render("[doc](docs/a.md)");
    const e = await contextMenu(repoLink());
    expect(e.defaultPrevented).toBe(true);
    expect(menuEl()).not.toBeNull();
    expect(labels()).toEqual(expect.arrayContaining(["Open in pane", "Open in new pane", "Show in Files"]));
    // Destructive actions stay out of a rendered document.
    expect(labels().join("|")).not.toMatch(/Rename|Delete|New file/i);
    const newPane = [...menuEl()!.querySelectorAll("button")].find((b) => b.textContent?.includes("Open in new pane"))!;
    await act(async () => newPane.click());
    expect(opened).toEqual([{ path: "repos/x/docs/a.md", openInNew: true }]);
    expect(menuEl()).toBeNull();
  });

  it("opens from the keyboard (Shift+F10)", async () => {
    listing = [{ name: "a.md", type: "file" }];
    await render("[doc](docs/a.md)");
    await act(async () => {
      repoLink().dispatchEvent(new KeyboardEvent("keydown", { key: "F10", shiftKey: true, bubbles: true, cancelable: true }));
    });
    await settle();
    expect(menuEl()).not.toBeNull();
  });

  it("offers reveal but no open item for a folder", async () => {
    listing = [{ name: "docs", type: "dir" }];
    await render("[docs](docs)");
    await contextMenu(repoLink());
    const l = labels();
    expect(l).toContain("Show in Files");
    expect(l).not.toContain("Open in pane");
    const reveal = [...menuEl()!.querySelectorAll("button")].find((b) => b.textContent?.includes("Show in Files"))!;
    await act(async () => reveal.click());
    expect(revealed).toEqual(["repos/x/docs"]);
  });

  it("toasts instead of opening a menu when the target is gone", async () => {
    listing = [];
    await render("[doc](docs/a.md)");
    await contextMenu(repoLink());
    expect(menuEl()).toBeNull();
    expect(String(toasts[0])).toContain("repos/x/docs/a.md");
  });

  it("leaves the native menu alone without a host", async () => {
    await render("[doc](docs/a.md)", { menu: false });
    expect((await contextMenu(repoLink())).defaultPrevented).toBe(false);
    expect(menuEl()).toBeNull();
  });

  it("leaves the native menu alone without onOpenFile (the shared view)", async () => {
    await render("[doc](docs/a.md)", { open: false });
    expect((await contextMenu(repoLink())).defaultPrevented).toBe(false);
    expect(menuEl()).toBeNull();
  });

  it("keeps left click opening the file", async () => {
    listing = [{ name: "a.md", type: "file" }];
    await render("[doc](docs/a.md)");
    await act(async () => {
      repoLink().dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
    });
    await settle();
    expect(opened).toEqual([{ path: "repos/x/docs/a.md", openInNew: false }]);
    expect(menuEl()).toBeNull();
  });
});

describe("inline-code path link menu", () => {
  it("opens the menu with file actions", async () => {
    resolved["docs/a.md"] = { path: "repos/x/docs/a.md", type: "file" };
    await render("see `docs/a.md`");
    const e = await contextMenu(pathLink());
    expect(e.defaultPrevented).toBe(true);
    expect(labels()).toEqual(
      expect.arrayContaining(["Open in pane", "Open in new pane", "Show in Files", "Copy the name", "Copy the relative path", "Download"]),
    );
  });

  it("limits a path outside home to open, copy path and download", async () => {
    resolved["/scratch/out.txt"] = { path: "/scratch/out.txt", type: "file" };
    await render("see `/scratch/out.txt`");
    await contextMenu(pathLink());
    const l = labels();
    expect(l).toEqual(expect.arrayContaining(["Open in pane", "Copy the path", "Download"]));
    expect(l).not.toContain("Show in Files");
    expect(l).not.toContain("Copy the name");
  });

  it("toasts when the file vanished after the link was rendered", async () => {
    resolved["docs/a.md"] = { path: "repos/x/docs/a.md", type: "file" };
    await render("see `docs/a.md`");
    clearPathRefCache();
    resolved = {};
    await contextMenu(pathLink());
    expect(menuEl()).toBeNull();
    expect(String(toasts[0])).toContain("repos/x/docs/a.md");
  });

  it("does not suppress the native menu without a host", async () => {
    resolved["docs/a.md"] = { path: "repos/x/docs/a.md", type: "file" };
    await render("see `docs/a.md`", { menu: false });
    expect((await contextMenu(pathLink())).defaultPrevented).toBe(false);
  });
});
