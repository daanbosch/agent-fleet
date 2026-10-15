import { beforeEach, describe, expect, it, vi } from "vitest";

const raw = vi.fn();
vi.mock("../../core/api/client.ts", () => ({
  raw: (...a: unknown[]) => raw(...a),
  errDetail: (e: { message?: string }) => e.message ?? "",
}));

import { exportSessionMarkdown } from "./exportMarkdown.ts";
import { t } from "../../lib/i18n/index.ts";

describe("exportSessionMarkdown", () => {
  beforeEach(() => raw.mockReset());

  it("reports a body that fails after a 200 instead of dropping it", async () => {
    raw.mockResolvedValue({
      ok: true,
      headers: new Headers(),
      blob: () => Promise.reject(new TypeError("network")),
    });
    const notify = vi.fn();
    await exportSessionMarkdown("s1", "names", notify);
    expect(notify).toHaveBeenCalledTimes(1);
    expect(notify.mock.calls[0][0]).toBe(t("srow.export_failed"));
  });

  it("shows the Agent's error and asks for the browser's zone", async () => {
    raw.mockResolvedValue({
      ok: false,
      json: () => Promise.resolve({ error: { code: "unsupported_kind", message: "nope" } }),
    });
    const notify = vi.fn();
    await exportSessionMarkdown("s1", "full", notify);
    expect(String(raw.mock.calls[0][0])).toMatch(/^api\/sessions\/s1\/export\?tools=full&tz=/);
    expect(notify.mock.calls[0][0]).toBe(t("srow.export_failed") + ": nope");
  });
});
