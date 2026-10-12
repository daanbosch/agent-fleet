// homeLists (#2028): Home cuts the attention order into "waiting for you" and "continue". These
// pin the cut, not the order itself, which sessions/order.ts owns and tests.
import { describe, expect, it } from "vitest";
import { HOME_CONTINUE_MAX, homeLists } from "./home.ts";
import { validateStoredContent } from "../../layout/migrate.ts";
import type { Session } from "../../types/session.ts";

const s = (name: string, over: Partial<Session> = {}): Session => ({ name, kind: "claude", alive: true, state: "", ...over });
const names = (list: Session[]) => list.map((x) => x.name);
const never = () => 0;

describe("homeLists", () => {
  it("puts every live session waiting for a person under waiting, most recent wait first", () => {
    const at: Record<string, number> = { q: 10, p: 30, pl: 20 };
    const { waiting, next } = homeLists(
      [s("idle"), s("q", { state: "question" }), s("p", { state: "permission" }), s("pl", { state: "plan" })],
      (n) => at[n] || 0,
    );
    expect(names(waiting)).toEqual(["p", "pl", "q"]);
    expect(names(next)).toEqual(["idle"]);
  });

  it("never offers a session whose folder is gone, and caps continue", () => {
    const many = Array.from({ length: HOME_CONTINUE_MAX + 3 }, (_, i) => s(`a${i}`, { createdAt: `2026-10-${String(10 + i)}T00:00:00Z` }));
    const { next } = homeLists([...many, s("dead", { alive: false, resumable: false }), s("stopped", { alive: false, resumable: true })], never);
    expect(next).toHaveLength(HOME_CONTINUE_MAX);
    expect(names(next)).not.toContain("dead");
    // Newest live sessions first; stopped ones only after every live one.
    expect(next[0].name).toBe(`a${HOME_CONTINUE_MAX + 2}`);
  });

  it("does not count a stopped session in a question as waiting", () => {
    const { waiting, next } = homeLists([s("old", { alive: false, state: "question" })], never);
    expect(waiting).toEqual([]);
    expect(names(next)).toEqual(["old"]);
  });
});

describe("Home as a stored pane", () => {
  it("survives a reload instead of falling back to a blank terminal", () => {
    expect(validateStoredContent({ kind: "home" })).toEqual({ kind: "home" });
  });
});
