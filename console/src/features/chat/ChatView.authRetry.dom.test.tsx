// Expired-login Retry in the real ChatView: the failed turn is re-run as sent (same prompt,
// retry flag), the user's bubble stays a single row, and a draft typed meanwhile is untouched.
import { describe, it, expect, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Conversation } from "../../types/chat.ts";

const RAW = "claude returned an error: Failed to authenticate: OAuth session expired and could not be refreshed";

const CONV: Conversation = {
  id: "c1",
  agent: "claude",
  active_agent: "claude",
  assistant_id: "a1",
  title: "t",
  model: "claude-opus-5",
  created_at: 1,
  updated_at: 2,
  messages: [],
} as Conversation;

const calls: unknown[][] = [];
const stream = vi.fn(async (_id: string, content: string, h: { onError?: (m: string) => void; onDone?: (c: Conversation) => void }, _s?: AbortSignal, _src?: string, retry?: boolean) => {
  calls.push([content, retry]);
  if (calls.length === 1) return h.onError?.(RAW);
  // The server keeps ONE user message for the retried turn.
  h.onDone?.({ ...CONV, messages: [{ role: "user", content, ts: 1 }, { role: "assistant", content: "ok", ts: 2, agent: "claude" }] });
});

vi.mock("../../core/api/client.ts", async (orig) => ({
  ...((await orig()) as Record<string, unknown>),
  api: () => Promise.resolve({}),
  apiJSON: () => Promise.resolve({}),
  chatGet: () => Promise.resolve(CONV),
  chatList: () => Promise.resolve([]),
  chatStream: (...a: Parameters<typeof stream>) => stream(...a),
  assistantGet: () => Promise.resolve({ id: "a1", name: "A", agent: "claude", icon: "beaker", voice: "" }),
  chatSuggestReplies: () => Promise.resolve({ suggestions: [] }),
  raw: () => Promise.resolve(new Response("")),
  rel: (p: string) => p,
  errText: (e: { message?: string }) => e?.message || "",
  isTransientErr: () => false,
}));
vi.mock("../../ui/ToastProvider.tsx", () => ({ useToast: () => () => {} }));
vi.mock("../../ui/ConfirmProvider.tsx", () => ({ useConfirm: () => () => Promise.resolve(true) }));

import { ChatView } from "./ChatView.tsx";
import { useWorkspaceStore } from "../../core/store/workspace.ts";

const settle = () => act(async () => { await new Promise((r) => setTimeout(r, 80)); });

function typeInto(ta: HTMLTextAreaElement, v: string) {
  const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
  set.call(ta, v);
  ta.dispatchEvent(new Event("input", { bubbles: true }));
}

describe("ChatView expired-login Retry", () => {
  it("re-runs the failed prompt once, keeps the new draft, and shows one user row", async () => {
    useWorkspaceStore.setState({ state: "running" });
    const host = document.createElement("div");
    document.body.appendChild(host);
    const root = createRoot(host);
    await act(async () => root.render(<ChatView conversationId="c1" paneId="p0" active />));
    await settle();

    const ta = host.querySelector("textarea") as HTMLTextAreaElement;
    await act(async () => typeInto(ta, "do the thing"));
    await act(async () => (host.querySelector(".chat-composer-row button.primary, .chat-composer-row .chat-send") as HTMLElement | null)?.click());
    await settle();
    expect(host.querySelector(".chat-auth-error")).not.toBeNull();

    // A new draft is typed while the notice is up.
    await act(async () => typeInto(ta, "next draft"));
    const retry = Array.from(host.querySelectorAll(".chat-auth-error button")).find((b) => b.className.includes("ghost")) as HTMLElement;
    await act(async () => retry.click());
    await settle();

    expect(calls).toEqual([["do the thing", false], ["do the thing", true]]);
    expect((host.querySelector("textarea") as HTMLTextAreaElement).value).toBe("next draft");
    expect(host.querySelector(".chat-auth-error")).toBeNull();
    expect(host.innerHTML.split("do the thing").length - 1).toBe(1);

    await act(async () => root.unmount());
    host.remove();
  });
});
