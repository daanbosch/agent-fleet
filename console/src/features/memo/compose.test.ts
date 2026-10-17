// composeMemoMessage builds the text the send modal opens with. It mirrors the server's
// buildFlushMessage (control-plane/memo.go), so the two must stay in step — including the
// single-memo case, where the list scaffolding is dropped entirely.
import { describe, expect, it } from "vitest";
import { composeMemoMessage, splitForSend } from "./compose.ts";
import type { Memo } from "../../types/memo.ts";

const aMemo = (over: Partial<Memo>): Memo => ({
  id: "m1",
  repo: "",
  category: "",
  kind: "text",
  body: "note",
  refPath: "",
  position: 0,
  createdAt: "2026-09-09T00:00:00Z",
  sentAt: "",
  ...over,
});

describe("composeMemoMessage", () => {
  it("groups by category and numbers within it", () => {
    const got = composeMemoMessage([
      aMemo({ id: "m1", category: "frontend", body: "余白を詰めて" }),
      aMemo({ id: "m2", category: "frontend", body: "色も直して" }),
      aMemo({ id: "m3", category: "api", body: "エラーハンドリング追加" }),
    ]);
    expect(got).toContain("## frontend");
    expect(got).toContain("1. 余白を詰めて");
    expect(got).toContain("2. 色も直して");
    expect(got).toContain("## api");
    // Numbering restarts per category.
    expect(got).toContain("1. エラーハンドリング追加");
  });

  it("sends a lone memo bare — no heading, no number, no indent", () => {
    const got = composeMemoMessage([aMemo({ category: "frontend", body: "余白を詰めて\n色も直して" })]);
    expect(got).toBe("余白を詰めて\n色も直して");
  });

  it("keeps a lone file memo's ref line and attachments, without the scaffolding", () => {
    const got = composeMemoMessage([
      aMemo({
        kind: "file",
        category: "frontend",
        refPath: "~/repos/a/Button.tsx",
        body: "余白を詰めて",
        attachments: [{ path: "/img/paste-1.png", name: "paste-1.png" }],
      }),
    ]);
    expect(got).toBe("対象ファイル: ~/repos/a/Button.tsx\n余白を詰めて\n添付画像: paste-1.png");
  });
});

// ADR 0111 decision 3: a memo taken in from the inbox is sent on its own.
describe("splitForSend", () => {
  it("keeps taken-in memos apart from the member's own", () => {
    const own = aMemo({ id: "own" });
    const taken = aMemo({ id: "taken", origin: { itemId: "i1", sender: "a" } });
    const { own: o, origins } = splitForSend([own, taken]);
    expect(o.map((m) => m.id)).toEqual(["own"]);
    expect(origins.map((m) => m.id)).toEqual(["taken"]);
    // The editable text is composed from the member's own memos only.
    expect(composeMemoMessage(o)).not.toContain("taken");
  });
});
