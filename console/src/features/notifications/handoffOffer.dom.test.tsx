// Where a handoff-offer notification leads (docs/log/77 §77.10, run-sheet step H-16 on #1960).
//
// Recorded as it behaves: the click always opens the shared view for the offer's catalog id,
// without asking whether the offer is still pending. After the owner unshared the session the
// offer is expired and the shared view answers that there is no access; §77.10 would rather
// say the handoff can no longer be viewed. Change this test together with that behaviour.
//
// In the dom project because store.ts pulls in core/api/client.ts, which touches localStorage at
// import time.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { openNotificationTarget, type FleetNotification } from "./store.ts";
import { openSharedSession } from "../sharing/open.ts";
import { apiJSON } from "../../core/api/client.ts";

vi.mock("../sharing/open.ts", () => ({ openSharedSession: vi.fn() }));
vi.mock("../../core/api/client.ts", async (orig) => ({
  ...(await orig<Record<string, unknown>>()),
  apiJSON: vi.fn(),
  api: vi.fn(async () => ({})),
}));

const offerNotification = (): FleetNotification => ({
  seq: 1, id: "handoff-offer-o1-m2", kind: "handoff-offer",
  target: { type: "shared-session", id: "cat-1" },
  displayName: "Carry on",
  payload: { offerId: "o1", catalogId: "cat-1", sessionName: "session-1" },
  createdAt: "2026-10-13T00:00:00Z", seen: false,
});

describe("a handoff-offer notification", () => {
  beforeEach(() => vi.clearAllMocks());

  it("opens the shared view of the offer's session, even after the offer expired with the unshare (H-16)", async () => {
    const r = await openNotificationTarget(offerNotification(), false);
    expect(r.opened).toBe(true);
    expect(openSharedSession).toHaveBeenCalledWith("cat-1", false);
    // Nothing asks the CP about the offer's state first: that is the recorded gap.
    expect(apiJSON).not.toHaveBeenCalled();
  });
});
