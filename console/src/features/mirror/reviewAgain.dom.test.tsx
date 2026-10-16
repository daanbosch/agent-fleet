// "Review again": the memory is keyed by planner session AND plan path, the offered reviewer is
// read from the live session list (so a deleted one is not offered), a stopped reviewer is
// resumed before anything else happens, and the message is a `question` that carries the path.
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  forgetReviewer, hasRememberedReviewer, liveReviewer, prepareReviewer, rememberReviewer, rememberedReviewer,
  reloadReviewersForTest, reviewerLabel, runRereview, sendRereview,
} from "./reviewAgain.ts";
import { rereviewPrompt } from "./planReview.ts";
import { getLocale, setLocale } from "../../lib/i18n/index.ts";
import type { Session } from "../../types/session.ts";

beforeEach(() => {
  localStorage.clear();
  reloadReviewersForTest();
});

const sess = (o: Partial<Session> & { name: string }) => ({ kind: "codex", alive: true, resumable: true, ...o }) as Session;

describe("reviewer memory", () => {
  it("keys by plan path: two plans in one planner session keep their own reviewer", () => {
    rememberReviewer("planner", "/plans/a.md", "rev-a");
    rememberReviewer("planner", "/plans/b.md", "rev-b");
    expect(rememberedReviewer("planner", "/plans/a.md")).toBe("rev-a");
    expect(rememberedReviewer("planner", "/plans/b.md")).toBe("rev-b");
    expect(rememberedReviewer("other", "/plans/a.md")).toBe("");
    expect(hasRememberedReviewer("planner")).toBe(true);
    expect(hasRememberedReviewer("plan")).toBe(false); // prefix of a name is not the name
  });

  it("survives a reload and is replaced by the newest reviewer", () => {
    rememberReviewer("planner", "/plans/a.md", "rev-1");
    rememberReviewer("planner", "/plans/a.md", "rev-2");
    reloadReviewersForTest();
    expect(rememberedReviewer("planner", "/plans/a.md")).toBe("rev-2");
  });

  it("forgets", () => {
    rememberReviewer("planner", "/plans/a.md", "rev");
    forgetReviewer("planner", "/plans/a.md");
    expect(rememberedReviewer("planner", "/plans/a.md")).toBe("");
  });
});

describe("liveReviewer", () => {
  const list = [sess({ name: "rev", title: "Review: X" }), sess({ name: "old", alive: false, resumable: false })];
  it("offers a remembered reviewer that is still in the list", () => {
    expect(liveReviewer("p", "/a.md", list, "rev")?.name).toBe("rev");
  });
  it("offers nothing for a deleted reviewer, an archive-only one, the planner itself or no path", () => {
    expect(liveReviewer("p", "/a.md", list, "gone")).toBeNull();
    expect(liveReviewer("p", "/a.md", list, "old")).toBeNull();
    expect(liveReviewer("rev", "/a.md", list, "rev")).toBeNull();
    expect(liveReviewer("p", "", list, "rev")).toBeNull();
  });
  it("labels by the CURRENT title, so a rename shows", () => {
    expect(reviewerLabel(sess({ name: "rev", title: "Renamed" }))).toBe("Renamed");
    expect(reviewerLabel(sess({ name: "rev" }))).toBe("rev");
  });
});

describe("prepareReviewer", () => {
  const sleep = async () => {};
  it("does nothing for a running reviewer", async () => {
    const start = vi.fn();
    expect(await prepareReviewer(sess({ name: "r" }), { start, ready: vi.fn(), sleep })).toEqual({ ok: true });
    expect(start).not.toHaveBeenCalled();
  });
  it("resumes a stopped reviewer and waits until it is ready", async () => {
    const start = vi.fn().mockResolvedValue(true);
    const ready = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true);
    expect(await prepareReviewer(sess({ name: "r", alive: false }), { start, ready, sleep })).toEqual({ ok: true });
    expect(start).toHaveBeenCalledWith("r");
    expect(ready).toHaveBeenCalledTimes(2);
  });
  it("is silent when start() already told the user why it failed", async () => {
    const r = await prepareReviewer(sess({ name: "r", alive: false }), { start: async () => false, ready: vi.fn(), sleep });
    expect(r).toEqual({ ok: false, message: "", silent: true });
  });
  it("gives up when the reviewer never becomes ready", async () => {
    const r = await prepareReviewer(sess({ name: "r", alive: false }), { start: async () => true, ready: async () => false, sleep });
    expect(r.ok).toBe(false);
  });
});

describe("sendRereview", () => {
  it("sends a question that carries the unchanged plan path, from the planner", async () => {
    const send = vi.fn().mockResolvedValue({ ok: true });
    expect(await sendRereview("planner", "/plans/a.md", sess({ name: "rev" }), send)).toEqual({ ok: true });
    const [to, from, intent, msg] = send.mock.calls[0];
    expect([to, from, intent]).toEqual(["rev", "planner", "question"]);
    expect(msg).toContain("/plans/a.md");
    expect(msg).toContain("planner"); // where the findings go back to
  });
  it("reports the failure, and forgets a reviewer the Agent no longer knows", async () => {
    rememberReviewer("planner", "/plans/a.md", "rev");
    const send = vi.fn().mockResolvedValue({ ok: false, message: "rate limited", code: "peer_rate_limited" });
    expect(await sendRereview("planner", "/plans/a.md", sess({ name: "rev" }), send)).toEqual({ ok: false, message: "rate limited" });
    expect(rememberedReviewer("planner", "/plans/a.md")).toBe("rev");
    send.mockResolvedValue({ ok: false, message: "gone", code: "peer_target_unknown" });
    await sendRereview("planner", "/plans/a.md", sess({ name: "rev" }), send);
    expect(rememberedReviewer("planner", "/plans/a.md")).toBe("");
  });
});

describe("rereviewPrompt", () => {
  const locale = getLocale();
  it("is fully translated in en and ja, and never carries a plan body", () => {
    setLocale("en");
    const en = rereviewPrompt({ parent: "planner", path: "/p/a.md" });
    expect(en).not.toMatch(/[぀-ヿ一-鿿]/);
    expect(en).toContain("Do not re-raise");
    setLocale("ja");
    const ja = rereviewPrompt({ parent: "planner", path: "/p/a.md" });
    expect(ja).toMatch(/[぀-ヿ一-鿿]/);
    expect(ja).toContain("/p/a.md");
    setLocale(locale);
  });
});

describe("runRereview ordering", () => {
  const run = (o: { prepare?: any; send?: any }) => {
    const log: string[] = [];
    const p = runRereview({
      planner: "planner",
      path: "/a.md",
      reviewer: sess({ name: "rev" }),
      beforeSend: async () => void log.push("reject"),
      onSent: () => log.push("sent"),
      onFail: (m) => log.push("fail:" + m),
      prepare: o.prepare ?? (async () => (log.push("prepare"), { ok: true })),
      send: o.send ?? (async () => (log.push("send"), { ok: true })),
    });
    return p.then((ok) => ({ ok, log }));
  };
  it("readies the reviewer, then rejects, then sends, then reports sent", async () => {
    expect(await run({})).toEqual({ ok: true, log: ["prepare", "reject", "send", "sent"] });
  });
  it("leaves the plan alone (no reject, no send) when the reviewer cannot be readied", async () => {
    const r = await run({ prepare: async () => ({ ok: false, message: "timeout" }) });
    expect(r).toEqual({ ok: false, log: ["fail:timeout"] });
  });
  it("does not report sent (the carried card stays) when the send fails", async () => {
    const r = await run({ send: async () => ({ ok: false, message: "rate" }) });
    expect(r.ok).toBe(false);
    expect(r.log).toEqual(["prepare", "reject", "fail:rate"]);
  });
});
