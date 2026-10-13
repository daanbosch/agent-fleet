// The share dialogs against a slow or failing Control Plane (issue #2074).
//
// A share change waits on the owner's Workspace, so it can take seconds and it can fail
// (409 share_operation_in_progress while an approved operation runs). What is pinned here:
// the create dialog can always be closed and gives up on its own after a deadline, and an
// unshare or a permission change that fails says so instead of leaving the row as if it
// had worked.
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";

type Handler = (path: string, opts?: RequestInit) => Promise<unknown>;
let handler: Handler = async () => ({});
const calls: { path: string; opts?: RequestInit }[] = [];
vi.mock("../../core/api/client.ts", () => ({
  api: (path: string, opts?: RequestInit) => {
    calls.push({ path, opts });
    return handler(path, opts);
  },
  apiJSON: async () => ({}),
  errText: (e: unknown) => String((e as { message?: string })?.message ?? e),
  getTenant: () => "",
}));
vi.mock("../../ui/ConfirmProvider.tsx", () => ({ useConfirm: () => async () => true }));

import { ShareCreateModal } from "./ShareCreateModal.tsx";
import { ShareListModal } from "./ShareListModal.tsx";
import { HandoffOfferModal } from "./HandoffOfferModal.tsx";
import { SHARE_REQUEST_TIMEOUT_MS } from "./boundedRequest.ts";
const { ToastProvider } = await import("../../ui/ToastProvider.tsx");

let root: Root | null = null;
let host: HTMLDivElement | null = null;

/** A request that never answers unless it is aborted, like a CP stuck behind a sync. */
function hang(opts?: RequestInit): Promise<unknown> {
  return new Promise((_, reject) => {
    opts?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
  });
}

async function mount(node: React.ReactNode) {
  host = document.createElement("div");
  document.body.appendChild(host);
  await act(async () => {
    root = createRoot(host!);
    root.render(<ToastProvider>{node}</ToastProvider>);
  });
  await flush();
}

async function flush(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function buttons(): HTMLButtonElement[] {
  return [...document.querySelectorAll("button")] as HTMLButtonElement[];
}

beforeEach(() => {
  vi.useFakeTimers();
  calls.length = 0;
});

afterEach(() => {
  act(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.useRealTimers();
});

describe("ShareCreateModal", () => {
  async function openAndSubmit(onClose: () => void) {
    handler = async (path, opts) => {
      if (path.startsWith("api/session-share-recipients")) return { members: [{ userKey: "b-example-com", email: "b@example.com" }] };
      if (path === "api/session-shares" && opts?.method === "POST") return hang(opts);
      return { shares: [] };
    };
    await mount(<ShareCreateModal initialTarget="session:s1" onClose={onClose} />);
    await flush(300); // the recipient search is debounced
    const pick = buttons().find((b) => b.textContent?.includes("b@example.com"));
    expect(pick).toBeTruthy();
    await act(async () => pick!.click());
    const submit = buttons().find((b) => b.type === "submit")!;
    await act(async () => submit.click());
    await flush();
    const post = calls.find((c) => c.opts?.method === "POST");
    expect(post).toBeTruthy();
    return { submit, post: post! };
  }

  it("can be closed while the share request is still running, and abandons it", async () => {
    const onClose = vi.fn();
    const { submit, post } = await openAndSubmit(onClose);
    expect(submit.disabled).toBe(true);
    expect(submit.getAttribute("aria-busy")).toBe("true");
    const cancel = [...document.querySelectorAll(".ui-modal-foot button")].find((b) => (b as HTMLButtonElement).type !== "submit") as HTMLButtonElement;
    expect(cancel.disabled).toBe(false);
    await act(async () => cancel.click());
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(post.opts?.signal?.aborted).toBe(true);
  });

  // The parent can unmount the dialog mid-request (HandoffOfferModal closing, a pane
  // switch). The aborted request is not a created share: onCreated would make the parent
  // reload recipients from a screen that is gone.
  it("does not report an abandoned request as created when the parent unmounts it", async () => {
    const onCreated = vi.fn();
    const onClose = vi.fn();
    handler = async (path, opts) => {
      if (path.startsWith("api/session-share-recipients")) return { members: [{ userKey: "b-example-com", email: "b@example.com" }] };
      if (path === "api/session-shares" && opts?.method === "POST") return hang(opts);
      return { shares: [] };
    };
    await mount(<ShareCreateModal initialTarget="session:s1" onClose={onClose} onCreated={onCreated} />);
    await flush(300);
    const pick = buttons().find((b) => b.textContent?.includes("b@example.com"))!;
    await act(async () => pick.click());
    await act(async () => buttons().find((b) => b.type === "submit")!.click());
    await flush();
    const post = calls.find((c) => c.opts?.method === "POST")!;
    act(() => root?.unmount());
    root = null;
    await flush();
    expect(post.opts?.signal?.aborted).toBe(true);
    expect(onCreated).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("gives up after the deadline, says so, and lets the user try again", async () => {
    const { submit } = await openAndSubmit(() => {});
    await flush(SHARE_REQUEST_TIMEOUT_MS + 10);
    expect(document.body.textContent || "").toContain("共有の応答がありません");
    expect(submit.disabled).toBe(false);
  });
});

describe("ShareListModal", () => {
  const SHARES = { shares: [{ id: "sh1", recipientUserKey: "b-example-com", scope: { type: "session", key: "s1" }, permission: "ro" }] };

  it("shows the server's reason when an unshare is refused", async () => {
    let answer: (v: unknown) => void = () => {};
    handler = async (path, opts) => {
      if (opts?.method === "DELETE") return new Promise((r) => { answer = r; });
      return SHARES;
    };
    await mount(<ShareListModal onClose={() => {}} />);
    const trash = buttons().find((b) => b.getAttribute("aria-label") === "解除")!;
    await act(async () => trash.click());
    await flush();
    expect(document.querySelector(".share-row")?.getAttribute("aria-busy")).toBe("true");
    await act(async () => answer({ error: { code: "share_operation_in_progress", message: "busy right now" } }));
    await flush();
    expect(document.body.textContent || "").toContain("busy right now");
    expect(document.querySelector(".share-row")?.getAttribute("aria-busy")).toBe("false");
  });

  it("says when a permission change could not be sent", async () => {
    handler = async (path, opts) => {
      if (opts?.method === "PATCH") throw new TypeError("network down");
      return SHARES;
    };
    await mount(<ShareListModal onClose={() => {}} />);
    const select = document.querySelector(".share-row select") as HTMLSelectElement;
    const setValue = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, "value")!.set!;
    await act(async () => {
      setValue.call(select, "rw");
      select.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await flush();
    expect(calls.some((c) => c.opts?.method === "PATCH")).toBe(true);
    expect(document.body.textContent || "").toContain("権限を変更できませんでした");
  });

  it("gives up on an unshare that never answers", async () => {
    handler = async (path, opts) => (opts?.method === "DELETE" ? hang(opts) : SHARES);
    await mount(<ShareListModal onClose={() => {}} />);
    const trash = buttons().find((b) => b.getAttribute("aria-label") === "解除")!;
    await act(async () => trash.click());
    await flush(SHARE_REQUEST_TIMEOUT_MS + 10);
    expect(document.body.textContent || "").toContain("応答がありません");
    expect(trash.disabled).toBe(false);
  });
});

describe("HandoffOfferModal", () => {
  // The recipient lookup syncs the owner's catalog before it answers; "loading" must end
  // in the load error rather than last until a reload.
  it("stops loading after the deadline and shows the load error", async () => {
    handler = async (path, opts) => hang(opts);
    await mount(<HandoffOfferModal session="s1" initialPrompt="continue" onClose={() => {}} />);
    expect(document.body.textContent || "").toContain("読み込み中");
    await flush(SHARE_REQUEST_TIMEOUT_MS + 10);
    expect(document.body.textContent || "").not.toContain("読み込み中");
    expect(document.body.textContent || "").toContain("引き継ぎ先を取得できませんでした");
  });

  const MEMBER = { members: [{ userKey: "b-example-com", email: "b@example.com" }] };
  const submitBtn = () => buttons().find((b) => b.type === "submit")!;

  // The git run behind the verdict is the slow part for a member with many working copies:
  // the recipients must not wait for it, and the send must not go out before it.
  it("shows the recipients before the verdict arrives, and keeps send disabled until it passes", async () => {
    let answer: (v: unknown) => void = () => {};
    handler = async (path) => {
      if (path.includes("/handoff-recipients")) return MEMBER;
      if (path.endsWith("/handoff-context")) return new Promise((r) => { answer = r; });
      return {};
    };
    await mount(<HandoffOfferModal session="s1" initialTitle="next" initialPrompt="continue" onClose={() => {}} />);
    expect(document.querySelector(".ui-modal select")?.textContent).toContain("b@example.com");
    expect(document.body.textContent || "").toContain("確認しています");
    expect(submitBtn().disabled).toBe(true);
    await act(async () => answer({ context: { vcs: "git", branch: "main", headSha: "abcdef12", ahead: 0 } }));
    await flush();
    expect(document.body.textContent || "").not.toContain("確認しています");
    expect(submitBtn().disabled).toBe(false);
  });

  it("keeps send disabled when the verdict that arrives is a block", async () => {
    handler = async (path) => {
      if (path.includes("/handoff-recipients")) return MEMBER;
      if (path.endsWith("/handoff-context")) return { context: { vcs: "git", branch: "main", blocked: "detached_head" } };
      return {};
    };
    await mount(<HandoffOfferModal session="s1" initialTitle="next" initialPrompt="continue" onClose={() => {}} />);
    expect(document.querySelector(".handoff-blocked")).toBeTruthy();
    expect(submitBtn().disabled).toBe(true);
  });

  it("never enables send when the verdict cannot be read", async () => {
    handler = async (path) => {
      if (path.includes("/handoff-recipients")) return MEMBER;
      if (path.endsWith("/handoff-context")) return { error: { code: "handoff_no_working_copy", message: "no working copy" } };
      return {};
    };
    await mount(<HandoffOfferModal session="s1" initialTitle="next" initialPrompt="continue" onClose={() => {}} />);
    expect(document.body.textContent || "").toContain("no working copy");
    expect(submitBtn().disabled).toBe(true);
  });

  it("asks for the recipients without the verdict, and for the verdict on its own route", async () => {
    handler = async (path) => (path.includes("/handoff-recipients") ? MEMBER : { context: { vcs: "git", branch: "main" } });
    await mount(<HandoffOfferModal session="s1" initialPrompt="continue" onClose={() => {}} />);
    expect(calls.map((c) => c.path).sort()).toEqual([
      "api/sessions/s1/handoff-context",
      "api/sessions/s1/handoff-recipients?context=skip",
    ]);
  });

  async function openAndSend(onClose: () => void) {
    handler = async (path, opts) => {
      if (path.includes("/handoff-recipients")) return { members: [{ userKey: "b-example-com", email: "b@example.com" }] };
      if (path.endsWith("/handoff-context")) return { context: { vcs: "git", branch: "main", ahead: 0 } };
      if (path === "api/session-handoff-offers" && opts?.method === "POST") return hang(opts);
      return {};
    };
    await mount(<HandoffOfferModal session="s1" initialTitle="next" initialPrompt="continue" onClose={onClose} />);
    await act(async () => buttons().find((b) => b.type === "submit")!.click());
    await flush();
    const post = calls.find((c) => c.path === "api/session-handoff-offers")!;
    expect(post).toBeTruthy();
    return post;
  }

  // The send waits on the owner's Workspace too (catalog sync, handoff-context): it must
  // neither lock the dialog nor spin forever.
  it("can be closed while the send is running, and abandons it", async () => {
    const onClose = vi.fn();
    const post = await openAndSend(onClose);
    const cancel = [...document.querySelectorAll(".ui-modal-foot button")].find((b) => (b as HTMLButtonElement).type !== "submit") as HTMLButtonElement;
    await act(async () => cancel.click());
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(post.opts?.signal?.aborted).toBe(true);
  });

  // "Push, then send" may legitimately run as long as the Agent's push bound; the Console
  // must not abandon it at the ordinary deadline.
  it("waits for the push bound on top of the deadline when the send pushes first", async () => {
    handler = async (path, opts) => {
      if (path.includes("/handoff-recipients")) return { members: [{ userKey: "b-example-com", email: "b@example.com" }] };
      if (path.endsWith("/handoff-context")) {
        return { context: { vcs: "git", branch: "x", headSha: "abc", blocked: "unpushed_commits", pushRemote: "origin", pushCommits: 1 } };
      }
      if (path === "api/session-handoff-offers" && opts?.method === "POST") return hang(opts);
      return {};
    };
    await mount(<HandoffOfferModal session="s1" initialTitle="next" initialPrompt="continue" onClose={() => {}} />);
    const box = document.querySelector("label.handoff-push input") as HTMLInputElement;
    await act(async () => box.click());
    await act(async () => buttons().find((b) => b.type === "submit")!.click());
    await flush();
    const post = calls.find((c) => c.path === "api/session-handoff-offers")!;
    expect(JSON.parse(String(post.opts?.body)).pushFirst).toBe(true);
    await flush(SHARE_REQUEST_TIMEOUT_MS + 10);
    expect(post.opts?.signal?.aborted).toBe(false);
    await flush(62_000);
    expect(post.opts?.signal?.aborted).toBe(true);
    expect(document.body.textContent || "").toContain("引き継ぎの応答がありません");
  });

  it("gives up on a send that never answers and says so", async () => {
    await openAndSend(() => {});
    await flush(SHARE_REQUEST_TIMEOUT_MS + 10);
    expect(document.body.textContent || "").toContain("引き継ぎの応答がありません");
    expect(buttons().find((b) => b.type === "submit")!.disabled).toBe(false);
  });
});
