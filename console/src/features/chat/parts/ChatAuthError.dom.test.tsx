import { describe, it, expect, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { ChatAuthError } from "./ChatAuthError.tsx";

const RAW = "claude returned an error: Failed to authenticate: OAuth session expired and could not be refreshed";

function mount(props: Partial<Parameters<typeof ChatAuthError>[0]> = {}) {
  const host = document.createElement("div");
  document.body.appendChild(host);
  const root = createRoot(host);
  const onReauth = vi.fn();
  const onRetry = vi.fn();
  act(() => root.render(<ChatAuthError text={RAW} onReauth={onReauth} onRetry={onRetry} {...props} />));
  return { host, onReauth, onRetry, done: () => act(() => root.unmount()) };
}

describe("ChatAuthError", () => {
  it("shows the message, keeps the raw text in details, and wires both buttons", () => {
    const { host, onReauth, onRetry, done } = mount();
    expect(host.querySelector(".cae-msg")?.textContent).toBeTruthy();
    expect(host.querySelector("details .cae-raw")?.textContent).toBe(RAW);
    const [reauth, retry] = Array.from(host.querySelectorAll("button"));
    act(() => reauth.click());
    act(() => retry.click());
    expect(onReauth).toHaveBeenCalledTimes(1);
    expect(onRetry).toHaveBeenCalledTimes(1);
    done();
  });
  it("omits Retry when there is nothing to re-send", () => {
    const { host, done } = mount({ onRetry: undefined });
    expect(host.querySelectorAll("button")).toHaveLength(1);
    done();
  });
});
