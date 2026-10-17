// The retry band: a provider error the agent keeps retrying must not read as plain "working".
import { describe, it, expect, afterEach } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MirrorBanners } from "./MirrorBanners.tsx";
import type { RetryNote } from "./retryNote.ts";

let root: Root | null = null;
let host: HTMLDivElement | null = null;

function mount(retry: RetryNote | null) {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  act(() => {
    root!.render(
      <MirrorBanners
        isPlan={false}
        termState=""
        compactProg={null}
        retry={retry}
        suggestedTitle=""
        titleActing={false}
        onOpenTerminal={() => {}}
        onSkipUpdate={() => {}}
        onAcceptTitle={() => {}}
        onDismissTitle={() => {}}
      />,
    );
  });
  return host;
}

afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = host = null;
});

describe("MirrorBanners retry band", () => {
  it("shows the attempt and the upstream message while retrying", () => {
    const el = mount({ attempt: 5, message: "This Go model requires Global regions.", failed: false });
    const text = el.textContent ?? "";
    expect(text).toContain("5");
    expect(text).toContain("This Go model requires Global regions.");
  });
  it("says the turn was stopped once the driver gave up", () => {
    const el = mount({ attempt: 2, message: "quota", failed: true });
    expect(el.textContent).toContain("quota");
    expect(el.querySelector(".mirror-attention")).not.toBeNull();
  });
  it("renders nothing without a retry", () => {
    expect(mount(null).textContent).toBe("");
  });
});
