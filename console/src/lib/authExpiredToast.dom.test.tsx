// An `auth_expired` send refusal must hand the member a way into the Claude login, because a
// member with no session has no transcript error card to click. Every other failure stays a
// plain toast.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, type ReactNode } from "react";
import { createRoot, type Root } from "react-dom/client";

const { toastSendFailure } = await import("./authExpiredToast.tsx");
const { useSettingsUI } = await import("../features/settings/store.ts");

let root: Root | null = null;
let host: HTMLDivElement;

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  act(() => useSettingsUI.getState().closeSettings());
});

afterEach(async () => {
  await act(async () => root?.unmount());
  host.remove();
  root = null;
});

describe("toastSendFailure", () => {
  it("renders a sticky Re-authenticate action for auth_expired that opens Settings > Agents", async () => {
    const sink = vi.fn();
    toastSendFailure(sink, { code: "auth_expired", message: "ignored" }, "fallback");
    expect(sink).toHaveBeenCalledTimes(1);
    const [node, opts] = sink.mock.calls[0] as [ReactNode, { duration?: number }];
    expect(opts.duration).toBe(0);

    await act(async () => root!.render(<>{node}</>));
    const btn = host.querySelector("button");
    expect(btn?.textContent).toBe("再認証");
    await act(async () => btn!.click());

    const s = useSettingsUI.getState();
    expect(s.settingsOpen).toBe(true);
    expect(s.settingsSection).toBe("agents");
    expect(s.settingsTarget).toBe("claude");
  });

  it("keeps any other failure a plain toast", () => {
    const sink = vi.fn();
    toastSendFailure(sink, { code: "question_pending", message: "answer it first" }, "fallback");
    expect(sink).toHaveBeenCalledWith("answer it first");
    toastSendFailure(sink, {}, "fallback");
    expect(sink).toHaveBeenLastCalledWith("fallback");
  });
});
