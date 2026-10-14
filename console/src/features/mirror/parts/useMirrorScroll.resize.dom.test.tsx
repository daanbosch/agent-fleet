// A band above the transcript (the title suggestion) changes the body's own height. jsdom has
// no layout, so the body's geometry is a set of mutable numbers and the ResizeObserver is driven by hand.
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { act, useEffect } from "react";
import { createRoot, type Root } from "react-dom/client";
import { useMirrorScroll } from "./useMirrorScroll.ts";
import { clearMarks } from "../scrollMark.ts";

let api: ReturnType<typeof useMirrorScroll> | null = null;
let height = 100;
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
        Object.defineProperty(el, "scrollHeight", { value: 1000, configurable: true });
        Object.defineProperty(el, "scrollTop", { value: 0, writable: true, configurable: true });
        el.getBoundingClientRect = () => new DOMRect(0, 0, 200, height);
        s.bodyRef.current = el;
      }}
    />
  );
}

let root: Root | null = null;
let host: HTMLDivElement | null = null;

const resize = (h: number) => {
  height = h;
  act(() => observers.forEach((cb) => cb()));
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
  height = 100;
  observers = [];
});

describe("useMirrorScroll body resize", () => {
  it("keeps the bottom edge in place off follow when a band above appears and goes", () => {
    const el = api!.bodyRef.current!;
    el.scrollTop = 300; // reading mid-transcript: bottom edge at 400
    api!.atBottomRef.current = false;
    resize(64);
    expect(el.scrollTop).toBe(336);
    resize(100);
    expect(el.scrollTop).toBe(300);
    expect(api!.atBottomRef.current).toBe(false); // holding the edge is not following
  });

  it("still pins to the end while following", () => {
    const el = api!.bodyRef.current!;
    el.scrollTop = 900;
    api!.atBottomRef.current = true;
    resize(64);
    expect(el.scrollTop).toBe(1000);
  });

  it("does not shift on a hidden pane coming back", () => {
    const el = api!.bodyRef.current!;
    api!.atBottomRef.current = false;
    resize(0);
    el.scrollTop = 300; // placed while hidden (a position restore)
    resize(100);
    expect(el.scrollTop).toBe(300);
  });
});
