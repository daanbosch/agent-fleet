// The move dialog's pure rules (ADR 0115): which phase a preview is in, which kinds the picker
// offers, and how a usage answer reads. The Agent decides eligibility; these only must not widen
// it (a kind it refused never becomes a button) or invent a quota it did not observe.
import { describe, expect, it } from "vitest";
import { fmtSize, movePhase, moveTargetOf, offeredTargets, orderByQuota, quotaState, readMoveUsage, type MovePreview } from "./move.ts";

const preview = (over: Partial<MovePreview> = {}): MovePreview => ({
  source: "s1",
  kind: "claude",
  state: "idle",
  targets: [
    { kind: "claude", eligible: false, reason: "same_kind" },
    { kind: "codex", eligible: true },
    { kind: "opencode", eligible: true },
    { kind: "cursor", eligible: false, reason: "not_measured" },
  ],
  ...over,
});

describe("movePhase", () => {
  it("reads a fresh source as a move, a moving one as pending and a moved one as moved", () => {
    expect(movePhase(preview())).toBe("move");
    expect(movePhase(preview({ moving: "t1" }))).toBe("pending");
    expect(movePhase(preview({ pending: { target: "t1", targetExists: false } }))).toBe("pending");
    // movedTo wins: a committed move whose record is still there is done, not pending.
    expect(movePhase(preview({ movedTo: "t1", pending: { target: "t1", targetExists: true } }))).toBe("moved");
  });
  it("points a row at the target either way", () => {
    expect(moveTargetOf({ moving: "t1" })).toBe("t1");
    expect(moveTargetOf({ movedTo: "t2" })).toBe("t2");
    expect(moveTargetOf({})).toBe("");
  });
});

describe("offeredTargets", () => {
  it("offers only the kinds the Agent called eligible", () => {
    expect(offeredTargets(preview(), null)).toEqual(["codex", "opencode"]);
  });
  it("drops an eligible kind that is not connected, once the check has answered", () => {
    expect(offeredTargets(preview(), (k) => k !== "opencode")).toEqual(["codex"]);
  });
  it("never turns a refused kind into a button, even when it is connected", () => {
    expect(offeredTargets(preview(), () => true)).not.toContain("cursor");
    expect(offeredTargets(preview(), () => true)).not.toContain("claude");
  });
});

describe("readMoveUsage", () => {
  it("reads the two windows the usage chips read", () => {
    expect(readMoveUsage({ ok: true, fiveHour: { pct: 12.4 }, sevenDay: { pct: 80 } })).toEqual({ fiveHour: 12, sevenDay: 80 });
  });
  it("says nothing for a failed read or a window that rolled over after its capture", () => {
    expect(readMoveUsage({ ok: false, fiveHour: { pct: 5 } })).toBeNull();
    expect(readMoveUsage(null)).toBeNull();
    expect(readMoveUsage({ ok: true, fiveHour: { pct: 5, stale: true } })).toBeNull();
    expect(readMoveUsage({ ok: true, fiveHour: { pct: 5, stale: true }, sevenDay: { pct: 40 } })).toEqual({ sevenDay: 40 });
  });
});

describe("fmtSize", () => {
  it("states bytes, KiB and MiB", () => {
    expect(fmtSize(900)).toBe("900 B");
    expect(fmtSize(300 * 1024)).toBe("300.0 KiB");
    expect(fmtSize(3 * 1024 * 1024)).toBe("3.0 MiB");
  });
});

describe("quotaState and orderByQuota", () => {
  it("judges only a kind with a reading, and calls a window at 100% full", () => {
    expect(quotaState(undefined)).toBe("unknown");
    expect(quotaState({})).toBe("unknown");
    expect(quotaState({ fiveHour: 40, sevenDay: 99 })).toBe("left");
    expect(quotaState({ fiveHour: 10, sevenDay: 100 })).toBe("full");
  });
  it("puts quota left first, least used first, then unjudged kinds, then those at the cap", () => {
    const usage = { claude: { fiveHour: 100 }, codex: { fiveHour: 70 }, muse: { sevenDay: 20 } };
    expect(orderByQuota(["claude", "opencode", "codex", "muse"], usage)).toEqual(["muse", "codex", "opencode", "claude"]);
  });
  it("keeps the Agent's order among kinds it cannot tell apart, and drops none", () => {
    expect(orderByQuota(["opencode", "kiro", "lcpp"], {})).toEqual(["opencode", "kiro", "lcpp"]);
  });
});
