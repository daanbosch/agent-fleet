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
    expect(await sendRereview("planner", "/plans/a.md", sess({ name: "rev" }), "plan rev 2", send)).toEqual({ ok: true });
    const [to, from, intent, msg] = send.mock.calls[0];
    expect([to, from, intent]).toEqual(["rev", "planner", "question"]);
    expect(msg).toContain("/plans/a.md");
    expect(msg).toContain("planner"); // where the findings go back to
  });
  it("reports the failure, and forgets a reviewer the Agent no longer knows", async () => {
    rememberReviewer("planner", "/plans/a.md", "rev");
    const send = vi.fn().mockResolvedValue({ ok: false, message: "rate limited", code: "peer_rate_limited" });
    expect(await sendRereview("planner", "/plans/a.md", sess({ name: "rev" }), "plan rev 2", send)).toEqual({ ok: false, message: "rate limited" });
    expect(rememberedReviewer("planner", "/plans/a.md")).toBe("rev");
    send.mockResolvedValue({ ok: false, message: "gone", code: "peer_target_unknown" });
    await sendRereview("planner", "/plans/a.md", sess({ name: "rev" }), "plan rev 2", send);
    expect(rememberedReviewer("planner", "/plans/a.md")).toBe("");
  });
});

describe("rereviewPrompt", () => {
  const locale = getLocale();
  it("is fully translated in en and ja, and never carries a plan body", () => {
    setLocale("en");
    const en = rereviewPrompt({ parent: "planner", path: "/p/a.md", revision: "ab12" });
    expect(en).not.toMatch(/[぀-ヿ一-鿿]/);
    expect(en).toContain("Do not re-raise");
    setLocale("ja");
    const ja = rereviewPrompt({ parent: "planner", path: "/p/a.md", revision: "ab12" });
    expect(ja).toMatch(/[぀-ヿ一-鿿]/);
    expect(ja).toContain("/p/a.md");
    setLocale(locale);
  });
});

// The Agent drops an identical (to, body) peer message for 2 minutes (peer_duplicate), so two
// rounds must not render the same body.
const dedupingAgent = () => {
  const seen = new Set<string>();
  return vi.fn(async (to: string, _from: string, _intent: string, body: string) => {
    const k = to + "\0" + body;
    if (seen.has(k)) return { ok: false, message: "duplicate", code: "peer_duplicate" };
    seen.add(k);
    return { ok: true };
  });
};

describe("revision id keeps successive rounds apart", () => {
  it("rev2 then rev3 within the dedupe window are both delivered; the same text twice is not", async () => {
    const send = dedupingAgent();
    const rev = sess({ name: "rev" });
    expect((await sendRereview("p", "/a.md", rev, "plan rev 2", send)).ok).toBe(true);
    expect((await sendRereview("p", "/a.md", rev, "plan rev 3", send)).ok).toBe(true);
    expect((await sendRereview("p", "/a.md", rev, "plan rev 3", send)).ok).toBe(false);
  });
});

describe("runRereview", () => {
  const base = () => {
    const log: string[] = [];
    return {
      log,
      o: {
        planner: "planner",
        path: "/a.md",
        plan: "plan",
        reviewer: sess({ name: "rev" }),
        lock: { held: false },
        beforeSend: async () => (log.push("reject"), true),
        onSent: () => log.push("sent"),
        onFail: (m: string, retry?: any): void => void log.push("fail:" + m + (retry ? "+retry" : "")),
        prepare: (async () => (log.push("prepare"), { ok: true })) as any,
        send: (async () => (log.push("send"), { ok: true })) as any,
      },
    };
  };
  it("readies the reviewer, then rejects, then sends, then reports sent", async () => {
    const { o, log } = base();
    expect(await runRereview(o)).toBe(true);
    expect(log).toEqual(["prepare", "reject", "send", "sent"]);
  });
  it("leaves the plan alone when the reviewer cannot be readied", async () => {
    const { o, log } = base();
    o.prepare = async () => ({ ok: false, message: "timeout" });
    expect(await runRereview(o)).toBe(false);
    expect(log).toEqual(["fail:timeout"]);
  });
  it("sends nothing when the interrupt did not go through", async () => {
    const { o, log } = base();
    o.beforeSend = async () => (log.push("reject"), false);
    expect(await runRereview(o)).toBe(false);
    expect(log).toEqual(["prepare", "reject"]);
  });
  it("does not report sent when the send fails, and hands over a retry that repeats only the send", async () => {
    const { o, log } = base();
    let ok = false;
    let retry: (() => Promise<boolean>) | undefined;
    o.send = async () => (log.push("send"), ok ? { ok: true } : { ok: false, message: "rate" });
    o.onFail = (m: string, r?: () => Promise<boolean>) => (log.push("fail:" + m), (retry = r));
    expect(await runRereview(o)).toBe(false);
    expect(log).toEqual(["prepare", "reject", "send", "fail:rate"]);
    ok = true;
    expect(await retry!()).toBe(true);
    expect(log.slice(4)).toEqual(["send", "sent"]); // no second prepare / reject
  });
  it("does not offer a retry when nothing was rejected (the card is still there)", async () => {
    const { o, log } = base();
    delete (o as any).beforeSend;
    o.send = async () => ({ ok: false, message: "rate" });
    await runRereview(o);
    expect(log.at(-1)).toBe("fail:rate");
  });
  it("drops a press whose plan is no longer on screen after the resume wait", async () => {
    const { o, log } = base();
    (o as any).stillCurrent = () => false;
    expect(await runRereview(o)).toBe(false);
    expect(log).toEqual(["prepare"]); // no reject, no send
  });
  it("is a no-op while another press holds the lock", async () => {
    const { o, log } = base();
    let release!: () => void;
    o.prepare = (() => new Promise((r) => (release = () => r({ ok: true })))) as any;
    const first = runRereview(o);
    expect(await runRereview(o)).toBe(false); // second press while the first is resuming
    release();
    expect(await first).toBe(true);
    expect(log).toEqual(["reject", "send", "sent"]); // one reject, one send
    expect(o.lock.held).toBe(false);
  });
});

describe("storage sharing between tabs", () => {
  it("picks up another tab's write on a storage event, and a later write keeps both", () => {
    rememberReviewer("p", "/a.md", "r1");
    // another tab launched r2 for the same plan and r9 for another plan
    localStorage.setItem("af.plan-reviewers", JSON.stringify({ "p\u0000/a.md": "r2", "p\u0000/b.md": "r9" }));
    window.dispatchEvent(new StorageEvent("storage", { key: "af.plan-reviewers" }));
    expect(rememberedReviewer("p", "/a.md")).toBe("r2");
    // a write from a tab that has not seen the event yet must still merge onto storage
    localStorage.setItem("af.plan-reviewers", JSON.stringify({ "p\u0000/a.md": "r2", "p\u0000/b.md": "r9", "p\u0000/c.md": "r7" }));
    rememberReviewer("p", "/d.md", "r5");
    expect(JSON.parse(localStorage.getItem("af.plan-reviewers")!)).toMatchObject({
      "p\u0000/a.md": "r2", "p\u0000/b.md": "r9", "p\u0000/c.md": "r7", "p\u0000/d.md": "r5",
    });
  });
});
