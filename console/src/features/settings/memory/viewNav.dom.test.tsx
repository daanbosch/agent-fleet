// The view-nav is always a tablist (no select at any width): roving tabindex, arrow/Home/End
// keys that move the selection and the focus, and the active tab kept in view in a scrolling row.
import { describe, it, expect, afterEach, vi } from "vitest";
import { act, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import { ViewNav } from "./viewNav.tsx";

const views = [
  { key: "a", label: "Alpha" },
  { key: "b", label: "Beta" },
  { key: "c", label: "Gamma" },
];

let root: Root | null = null;
let host: HTMLDivElement | null = null;

function Harness({ onPick }: { onPick?: (v: string) => void }) {
  const [v, setV] = useState("a");
  return (
    <ViewNav
      label="Views"
      views={views}
      value={v}
      onChange={(k) => {
        onPick?.(k);
        setV(k);
      }}
      prefix="t"
    />
  );
}
async function mount(onPick?: (v: string) => void) {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  await act(async () => root!.render(<Harness onPick={onPick} />));
}
afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = host = null;
});

const tabs = () => [...host!.querySelectorAll<HTMLButtonElement>("[role=tab]")];
const press = (el: Element, key: string) =>
  act(async () => {
    el.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
  });

describe("ViewNav", () => {
  it("renders only a tablist, never a select", async () => {
    await mount();
    expect(host!.querySelector("select")).toBeNull();
    expect(host!.querySelector("[role=combobox]")).toBeNull();
    expect(host!.querySelector("[role=tablist]")).not.toBeNull();
    expect(tabs().map((b) => b.textContent)).toEqual(["Alpha", "Beta", "Gamma"]);
  });

  it("keeps aria-controls and a roving tabindex", async () => {
    await mount();
    expect(tabs().map((b) => b.tabIndex)).toEqual([0, -1, -1]);
    expect(tabs().every((b) => b.getAttribute("aria-controls") === "t-panel")).toBe(true);
    expect(tabs().map((b) => b.getAttribute("aria-selected"))).toEqual(["true", "false", "false"]);
  });

  it("moves selection and focus with the arrow keys, wrapping at the ends", async () => {
    await mount();
    await press(tabs()[0], "ArrowRight");
    expect(tabs()[1].getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(tabs()[1]);
    expect(tabs().map((b) => b.tabIndex)).toEqual([-1, 0, -1]);
    await press(tabs()[1], "ArrowLeft");
    expect(document.activeElement).toBe(tabs()[0]);
    await press(tabs()[0], "ArrowLeft");
    expect(tabs()[2].getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(tabs()[2]);
    await press(tabs()[2], "ArrowRight");
    expect(tabs()[0].getAttribute("aria-selected")).toBe("true");
  });

  it("jumps with Home and End", async () => {
    await mount();
    await press(tabs()[0], "End");
    expect(tabs()[2].getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(tabs()[2]);
    await press(tabs()[2], "Home");
    expect(tabs()[0].getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(tabs()[0]);
  });

  it("ignores other keys", async () => {
    const onPick = vi.fn();
    await mount(onPick);
    await press(tabs()[0], "ArrowDown");
    await press(tabs()[0], "a");
    expect(onPick).not.toHaveBeenCalled();
  });

  it("scrolls the selected tab into view on mount and on change when scrollIntoView exists", async () => {
    const spy = vi.fn();
    Element.prototype.scrollIntoView = spy;
    try {
      await mount();
      expect(spy).toHaveBeenCalledWith({ block: "nearest", inline: "nearest" });
      spy.mockClear();
      await press(tabs()[0], "ArrowRight");
      expect(spy).toHaveBeenCalledTimes(1);
      expect(spy.mock.contexts[0]).toBe(tabs()[1]);
    } finally {
      delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
    }
  });

  it("does not throw where scrollIntoView is missing (jsdom)", async () => {
    await expect(mount()).resolves.toBeUndefined();
  });
});
