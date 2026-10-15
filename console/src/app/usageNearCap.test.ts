import { describe, expect, it } from "vitest";
import { pickNearCap } from "./usageNearCap.ts";

const NOW = Date.parse("2026-10-15T14:00:00Z");
const at = (h: number) => new Date(NOW + h * 3600_000).toISOString();

describe("pickNearCap", () => {
  it("skips a stale 5-hour window and shows the capped week", () => {
    const got = pickNearCap(
      [
        { label: "5h", pct: 98, resetsAt: at(4), stale: true },
        { label: "week", pct: 99, resetsAt: at(96) },
      ],
      NOW,
    );
    expect(got?.label).toBe("week");
  });

  it("skips a window whose reset is already past even when not flagged", () => {
    const got = pickNearCap(
      [
        { label: "5h", pct: 98, resetsAt: at(-1) },
        { label: "week", pct: 99, resetsAt: at(96) },
      ],
      NOW,
    );
    expect(got?.label).toBe("week");
  });

  it("shows the later reset when several live windows are capped", () => {
    const got = pickNearCap(
      [
        { label: "5h", pct: 98, resetsAt: at(2) },
        { label: "week", pct: 99, resetsAt: at(96) },
      ],
      NOW,
    );
    expect(got?.label).toBe("week");
    // order must not matter
    expect(
      pickNearCap([{ label: "week", pct: 99, resetsAt: at(96) }, { label: "5h", pct: 98, resetsAt: at(2) }], NOW)?.label,
    ).toBe("week");
  });

  it("ignores windows below the threshold, without a reset, or with an unparseable one", () => {
    expect(pickNearCap([{ label: "a", pct: 94, resetsAt: at(1) }], NOW)).toBeNull();
    expect(pickNearCap([{ label: "a", pct: 99 }], NOW)).toBeNull();
    expect(pickNearCap([{ label: "a", pct: 99, resetsAt: "garbage" }], NOW)).toBeNull();
    expect(pickNearCap([], NOW)).toBeNull();
  });

  it("returns a lone capped window", () => {
    expect(pickNearCap([{ label: "a", pct: 95, resetsAt: at(1) }], NOW)?.label).toBe("a");
  });
});
