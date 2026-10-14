// A band above the transcript (the title suggestion) changes the body's own height. jsdom has no
// layout, so the body's geometry is a set of mutable numbers, scrollTop clamps like a browser's, and
// the ResizeObserver is driven by hand.
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { act, useEffect } from "react";
import { createRoot, type Root } from "react-dom/client";
import { useMirrorScroll } from "./useMirrorScroll.ts";
import { clearMarks } from "../scrollMark.ts";

let api: ReturnType<typeof useMirrorScroll> | null = null;
let height = 500;
let top = 0;
let scroll = 0;
const SCROLL_HEIGHT = 2000;
const clampScroll = (v: number) => Math.max(0, Math.min(v, SCROLL_HEIGHT - height));
let observers: (() => void)[] = [];

function Harness() {
  const s = useMirrorScroll();
  api = s;
  return <div ref={s.mirrorRef}><Body s={s} /></div>;
}

function Body({ s }: { s: ReturnType<typeof useMirrorScroll> }) {
  useEffect(() => {
    s.resetForSession("shown");
    s.applyFollow({ groups: [], loaded: true, busy: false, pending: null, pendingPlan: null, pendingPerm: null });
  }, []);
  return (
    <div
      ref={(el) => {
        if (!el || s.bodyRef.current === el) return;
        Object.defineProperty(el, "clientHeight", { get: () => height, configurable: true });
        Object.defineProperty(el, "scrollHeight", { value: SCROLL_HEIGHT, configurable: true });
        Object.defineProperty(el, "scrollTop", {
          get: () => scroll,
          set: (v: number) => (scroll = clampScroll(v)),
          configurable: true,
        });
        el.getBoundingClientRect = () => new DOMRect(0, top, 200, height);
        s.bodyRef.current = el;
      }}
    />
  );
}

let root: Root | null = null;
let host: HTMLDivElement | null = null;

// A band above moves the body's top edge by what it takes; a resize below leaves the top alone.
// Like a browser, a body that grows past the end clamps the offset (with its scroll event) first.
const resize = (h: number, fromTop: boolean) => {
  if (fromTop) top += height - h;
  height = h;
  const clamped = clampScroll(scroll);
  act(() => {
    if (clamped !== scroll) {
      scroll = clamped;
      api!.onBodyScroll();
    }
    observers.forEach((cb) => cb());
  });
};

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor(cb: () => void) {
        observers.push(cb);
      }
      observe() {}
      disconnect() {}
    },
  );
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  act(() => root!.render(<Harness />));
});

afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  vi.unstubAllGlobals();
  clearMarks();
  api = null;
  height = 500;
  top = 0;
  scroll = 0;
  observers = [];
});

describe("useMirrorScroll body resize", () => {
  it("keeps the bottom edge in place off follow when a band above appears and goes", () => {
    const el = api!.bodyRef.current!;
    el.scrollTop = 300; // reading mid-transcript: bottom edge at 800
    api!.atBottomRef.current = false;
    resize(464, true);
    expect(el.scrollTop).toBe(336);
    resize(500, true);
    expect(el.scrollTop).toBe(300);
    expect(api!.atBottomRef.current).toBe(false); // holding the edge is not following
  });

  it("does not add the move to the browser's clamp when a band above goes near the end", () => {
    const el = api!.bodyRef.current!;
    api!.atBottomRef.current = false;
    resize(380, true);
    el.scrollTop = 1530; // 90px above the end, outside the follow zone: bottom edge at 1910
    api!.onBodyScroll();
    resize(500, true); // the browser clamps to 1500 (and scrolls) before the observer runs
    expect(el.scrollTop).toBe(1410);
    expect(api!.atBottomRef.current).toBe(false); // the clamp is not the reader coming back down
  });

  it("keeps the top in place when the body shrinks from below (the composer growing)", () => {
    const el = api!.bodyRef.current!;
    el.scrollTop = 300;
    api!.atBottomRef.current = false;
    resize(380, false);
    expect(el.scrollTop).toBe(300);
  });

  it("still pins to the end while following", () => {
    const el = api!.bodyRef.current!;
    el.scrollTop = 1500;
    api!.atBottomRef.current = true;
    resize(380, true);
    expect(el.scrollTop).toBe(1620);
  });

  it("does not shift on a hidden pane coming back", () => {
    const el = api!.bodyRef.current!;
    api!.atBottomRef.current = false;
    resize(0, false);
    el.scrollTop = 300; // placed while hidden (a position restore)
    top = 40; // and the layout above changed meanwhile
    resize(500, false);
    expect(el.scrollTop).toBe(300);
  });
});
