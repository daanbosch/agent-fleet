// Inbox attachments as a reader sees them (ADR 0111 decisions 4 and 5).
//
// What is held: a session the server marked unavailable is drawn as a placeholder with
// nothing to open, a recipe shows its repository apart from the prompt and offers take-in
// only where the server says so, and a share is requested only for the sender's own session
// with the box ticked.
import { describe, it, expect, afterEach, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

const takeIn = vi.fn(async () => ({ ok: true }));
vi.mock("./store.ts", () => ({ useInboxStore: { getState: () => ({ takeIn }) } }));
vi.mock("../sessions/open.ts", () => ({ openSessionChat: vi.fn() }));
vi.mock("../sharing/open.ts", () => ({ openSharedSession: vi.fn() }));
vi.mock("../sessions/store.ts", () => ({ useSessionsStore: (f: (s: { sessions: unknown[] }) => unknown) => f({ sessions: [] }) }));
vi.mock("../sharing/store.ts", () => ({ useSharedSessionsStore: (f: (s: { sessions: unknown[] }) => unknown) => f({ sessions: [] }) }));

import { AttachmentView, buildAttachments } from "./Attachments.tsx";
import type { InboxAttachment, InboxItem } from "./store.ts";
const { ToastProvider } = await import("../../ui/ToastProvider.tsx");

let root: Root | null = null;
let host: HTMLDivElement | null = null;

const item = { id: "item-1", role: "recipient", kind: "request" } as InboxItem;

async function render(att: InboxAttachment) {
  host = document.createElement("div");
  document.body.appendChild(host);
  await act(async () => {
    root = createRoot(host!);
    root.render(
      <ToastProvider>
        <AttachmentView item={item} att={att} />
      </ToastProvider>,
    );
  });
}

afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  takeIn.mockClear();
});

describe("AttachmentView", () => {
  it("draws an unavailable session as a placeholder with nothing to open", async () => {
    await render({ type: "session", available: false });
    expect(document.querySelector(".inbox-placeholder")).toBeTruthy();
    expect(document.querySelectorAll("button").length).toBe(0);
  });

  it("shows a recipe's repository apart from its prompt and takes it in", async () => {
    await render({
      type: "recipe",
      available: true,
      recipe: { prompt: "fix the race", kind: "claude", model: "", effort: "", repo: "github.com/k-k1/agent-fleet", canTakeIn: true, memoId: "" },
    });
    expect(document.querySelector(".inbox-recipe-repo code")?.textContent).toBe("github.com/k-k1/agent-fleet");
    expect(document.querySelector(".inbox-recipe .inbox-body")?.textContent).toBe("fix the race");
    const btn = [...document.querySelectorAll("button")].find((b) => /取り込む|Take into/.test(b.textContent || ""));
    await act(async () => btn!.click());
    expect(takeIn).toHaveBeenCalledWith("item-1");
  });

  it("offers no take-in where the server says the reader may not", async () => {
    await render({
      type: "recipe",
      available: true,
      recipe: { prompt: "x", kind: "", model: "", effort: "", repo: "", canTakeIn: false, memoId: "" },
    });
    expect(document.querySelectorAll("button").length).toBe(0);
  });
});

describe("buildAttachments", () => {
  const to = [
    { userKey: "b", email: "b@x", removed: false },
    { userKey: "c", email: "c@x", removed: false },
  ];
  it("asks for a share only for the sender's own session with the box ticked, naming every recipient", () => {
    expect(buildAttachments(null, { own: true, name: "s1", label: "s1" }, true, to)).toEqual([
      { type: "session", sessionName: "s1", share: { recipients: ["b", "c"], scope: "session" } },
    ]);
    expect(buildAttachments(null, { own: true, name: "s1", label: "s1" }, false, to)).toEqual([{ type: "session", sessionName: "s1" }]);
    expect(buildAttachments(null, { own: false, catalogId: "cat", label: "x" }, true, to)).toEqual([{ type: "session", catalogId: "cat" }]);
  });
  it("drops an empty recipe", () => {
    expect(buildAttachments({ prompt: "  ", kind: "", model: "", effort: "", repo: "" }, null, false, to)).toEqual([]);
  });
});
