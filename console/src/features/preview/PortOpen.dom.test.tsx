// PortOpen: what a click on a port does across the availability matrix, that Ctrl/⌘ and
// middle-click keep their meaning, that the menu lists all three targets with reasons where one
// is unavailable, and that "Expose this port…" only exists with a preview domain.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

const { PortOpen } = await import("./PortOpen.tsx");
const { usePreviewStore } = await import("./previewState.ts");
const { useLayoutStore } = await import("../../layout/store.ts");
const { useWorkspaceStore } = await import("../../core/store/workspace.ts");
const { useSettingsUI } = await import("../settings/store.ts");
const { t } = await import("../../lib/i18n/index.ts");
type Info = import("./previewState.ts").PreviewInfo;

let root: Root | null = null;
let host: HTMLDivElement;
const openTarget = vi.fn();
const openTargetInNew = vi.fn();
const original = { openTarget: useLayoutStore.getState().openTarget, openTargetInNew: useLayoutStore.getState().openTargetInNew };

const info = (over: Partial<Info> = {}): Info => ({
  domain: "pv.example.com",
  urls: { "3000": "https://abc-3000.pv.example.com" },
  ports: [3000],
  tempPorts: [],
  public: false,
  tenantShare: false,
  maxPorts: 8,
  ...over,
});

const render = async (port: number, running = true) => {
  await act(async () => {
    root!.render(<PortOpen port={port} running={running} variant="chip" />);
  });
};
const chip = () => host.querySelector<HTMLButtonElement>(".sess-port")!;
const menuBtn = () => host.querySelector<HTMLButtonElement>(".sess-port-menu")!;
const menu = () => document.body.querySelector<HTMLElement>(".port-menu");
const items = () => [...document.body.querySelectorAll<HTMLButtonElement>(".port-menu-item")];

let open: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  localStorage.clear();
  openTarget.mockReset();
  openTargetInNew.mockReset();
  useLayoutStore.setState({ openTarget, openTargetInNew });
  // The state is injected; a refresh must not overwrite it from the network.
  usePreviewStore.setState({ info: info(), refresh: async () => {} });
  open = vi.spyOn(window, "open").mockReturnValue(null);
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root?.unmount());
  host.remove();
  root = null;
  open.mockRestore();
  useLayoutStore.setState(original);
  useWorkspaceStore.setState({ browserUnavailable: "" });
  useSettingsUI.setState({ settingsOpen: false, previewAddPort: null });
});

describe("PortOpen chip", () => {
  it("opens a listed port at its subdomain URL, noopener, and names it in the tooltip", async () => {
    await render(3000);
    expect(chip().title).toBe(t("portopen.title_subdomain", { port: 3000, url: "abc-3000.pv.example.com/" }));
    await act(async () => chip().click());
    expect(open).toHaveBeenCalledWith("https://abc-3000.pv.example.com/", "_blank", "noopener");
    expect(openTarget).not.toHaveBeenCalled();
  });

  it("opens an unlisted port through the owner-only entry, never a hand-built subdomain URL", async () => {
    await render(5173);
    await act(async () => chip().click());
    const url = String(open.mock.calls[0][0]);
    expect(new URL(url).pathname).toMatch(/preview-open-own$/);
    expect(new URL(url).searchParams.get("port")).toBe("5173");
    expect(open.mock.calls[0][2]).toBe("noopener");
  });

  it("Ctrl-click and middle-click keep opening the browser pane in a new layout tab", async () => {
    await render(5173);
    await act(async () => {
      chip().dispatchEvent(new MouseEvent("click", { bubbles: true, ctrlKey: true }));
    });
    expect(openTargetInNew).toHaveBeenCalledWith({ content: { kind: "browser", port: 5173, path: "/" } }, true);
    await act(async () => {
      chip().dispatchEvent(new MouseEvent("auxclick", { bubbles: true, button: 1 }));
    });
    expect(openTargetInNew).toHaveBeenCalledTimes(2);
    expect(open).not.toHaveBeenCalled();
  });

  it("without a preview domain a click is the browser pane, as before", async () => {
    usePreviewStore.setState({ info: info({ domain: "", urls: {} }) });
    await render(5173);
    await act(async () => chip().click());
    expect(openTarget).toHaveBeenCalledWith({ content: { kind: "browser", port: 5173, path: "/" } });
    expect(open).not.toHaveBeenCalled();
  });

  it("without a domain or a browser it is the lightweight preview", async () => {
    usePreviewStore.setState({ info: info({ domain: "", urls: {} }) });
    useWorkspaceStore.setState({ browserUnavailable: "kubernetes" });
    await render(5173);
    await act(async () => chip().click());
    expect(open).toHaveBeenCalledWith(expect.stringMatching(/preview\/5173\/$/), "_blank", "noopener");
  });

  it("a start with a domain but no issued URL does not default to the subdomain", async () => {
    usePreviewStore.setState({ info: info({ urls: {} }) });
    await render(3000);
    await act(async () => chip().click());
    expect(openTarget).toHaveBeenCalled();
    expect(open).not.toHaveBeenCalled();
  });

  it("badges: unlisted is only-you, a listed shared port is shared, a public one is public", async () => {
    await render(5173);
    expect(chip().className).toContain("port-only_you");
    usePreviewStore.setState({ info: info({ tenantShare: true }) });
    await render(3000);
    expect(chip().className).toContain("port-shared");
    usePreviewStore.setState({ info: info({ tenantShare: true, public: true }) });
    await render(3000);
    expect(chip().className).toContain("port-public");
  });
});

describe("PortOpen menu", () => {
  it("lists all three targets with the default marked, and Copy URL gives the lightweight URL for an unlisted port", async () => {
    const write = vi.fn();
    Object.assign(navigator, { clipboard: { writeText: write } });
    await render(5173);
    await act(async () => menuBtn().click());
    expect(menu()).not.toBeNull();
    const names = items().map((i) => i.textContent ?? "");
    expect(names[0]).toContain(t("portopen.target.subdomain"));
    expect(names[0]).toContain(t("portopen.default"));
    expect(names[1]).toContain(t("portopen.target.pane"));
    expect(names[2]).toContain(t("portopen.target.light"));
    await act(async () => items()[3].click()); // Copy URL
    expect(write).toHaveBeenCalledWith(expect.stringMatching(/preview\/5173\/$/));
    expect(write.mock.calls[0][0]).not.toContain("preview-open-own");
  });

  it("disables unavailable targets and says why", async () => {
    usePreviewStore.setState({ info: info({ domain: "", urls: {} }) });
    useWorkspaceStore.setState({ browserUnavailable: "kubernetes" });
    await render(5173);
    await act(async () => menuBtn().click());
    const [sub, pane, light] = items();
    expect(sub.disabled).toBe(true);
    expect(sub.title).toBe(t("portopen.unavailable.no_domain"));
    expect(pane.disabled).toBe(true);
    expect(pane.title).toBe(t("portopen.unavailable.no_browser"));
    expect(light.disabled).toBe(false);
  });

  it("offers Expose this port… only with a preview domain, and opens settings with the port", async () => {
    await render(5173);
    await act(async () => menuBtn().click());
    const expose = items().find((i) => i.textContent?.includes(t("portopen.expose")))!;
    expect(expose).toBeTruthy();
    await act(async () => expose.click());
    expect(useSettingsUI.getState().settingsSection).toBe("preview");
    expect(useSettingsUI.getState().previewAddPort).toBe(5173);

    usePreviewStore.setState({ info: info({ domain: "", urls: {} }) });
    await render(5173);
    await act(async () => menuBtn().click());
    expect(items().some((i) => i.textContent?.includes(t("portopen.expose")))).toBe(false);
  });

  it("for a listed port the entry reads Preview subdomain settings and prefills nothing", async () => {
    await render(3000);
    await act(async () => menuBtn().click());
    const entry = items().find((i) => i.textContent?.includes(t("portopen.settings")))!;
    await act(async () => entry.click());
    expect(useSettingsUI.getState().previewAddPort).toBeNull();
  });

  it("a right-click opens the menu", async () => {
    await render(3000);
    await act(async () => {
      chip().dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true, clientX: 10, clientY: 10 }));
    });
    expect(menu()).not.toBeNull();
  });
});

describe("PortOpen buttons (workspace bar)", () => {
  const renderButtons = async (port: number) => {
    await act(async () => {
      root!.render(<PortOpen port={port} running variant="buttons" />);
    });
  };
  const btns = () => [...host.querySelectorAll<HTMLButtonElement>("button")];

  it("makes the default target the primary button and puts the other two beside it", async () => {
    await renderButtons(5173);
    const [primary, ...rest] = btns();
    expect(primary.className).toContain("ui-btn-primary");
    expect(primary.textContent).toBe(t("portopen.btn.subdomain"));
    expect(rest.slice(0, 2).map((b) => b.textContent)).toEqual([t("wsbar.preview.open_pane"), t("wsbar.preview.open_light")]);
    await act(async () => rest[1].click()); // lightweight
    expect(open).toHaveBeenCalledWith(expect.stringMatching(/preview\/5173\/$/), "_blank", "noopener");
  });

  it("disables the targets that cannot work, with the reason, and hides Expose without a domain", async () => {
    usePreviewStore.setState({ info: info({ domain: "", urls: {} }) });
    useWorkspaceStore.setState({ browserUnavailable: "kubernetes" });
    await renderButtons(5173);
    const byText = (s: string) => btns().find((b) => b.textContent === s)!;
    expect(byText(t("portopen.btn.subdomain")).disabled).toBe(true);
    expect(byText(t("portopen.btn.subdomain")).title).toBe(t("portopen.unavailable.no_domain"));
    expect(byText(t("wsbar.preview.open_pane")).title).toBe(t("browser.unavailable.short", { runtime: "kubernetes" }));
    expect(btns().some((b) => b.textContent === t("portopen.expose"))).toBe(false);
  });

  it("offers Expose this port… with a domain", async () => {
    await renderButtons(5173);
    expect(btns().some((b) => b.textContent === t("portopen.expose"))).toBe(true);
  });
});
