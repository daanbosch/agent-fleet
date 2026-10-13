// Where a handoff-offer notification leads (docs/log/77 §77.10, run-sheet step H-16 on #1960).
//
// A pending offer opens the shared view, whose banner accepts it. An offer that is no longer
// pending (unshared, taken over, declined, withdrawn, expired) has nowhere to go: the click says
// so instead of landing on the shared view's "no access" (#2041), and the caller marks it read.
//
// In the dom project because store.ts pulls in core/api/client.ts, which touches localStorage at
// import time.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { openNotificationTarget, type FleetNotification } from "./store.ts";
import { openSharedSession } from "../sharing/open.ts";
import { useHandoffStore, type HandoffOffer } from "../sharing/handoffStore.ts";
import { api } from "../../core/api/client.ts";
import { toast } from "../../ui/toast.ts";
import { t } from "../../lib/i18n/index.ts";

vi.mock("../sharing/open.ts", () => ({ openSharedSession: vi.fn() }));
vi.mock("../../ui/toast.ts", () => ({ toast: vi.fn() }));
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

const offer = (id: string): HandoffOffer => ({
  id, sessionId: "cat-1", sessionName: "session-1", recipientUserKey: "b", title: "Carry on", status: "pending",
});

describe("a handoff-offer notification", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(api).mockImplementation(async () => ({}));
    useHandoffStore.setState({ owned: [], received: [] });
  });

  it("opens the shared view of a pending offer the inbox already holds, without asking the CP", async () => {
    useHandoffStore.setState({ received: [offer("o1")] });
    const r = await openNotificationTarget(offerNotification(), false);
    expect(r.opened).toBe(true);
    expect(openSharedSession).toHaveBeenCalledWith("cat-1", false);
    expect(api).not.toHaveBeenCalled();
  });

  it("re-reads the inbox once when the store predates the offer, and opens it when it is pending", async () => {
    vi.mocked(api).mockResolvedValue({ offers: [offer("o1")] });
    const r = await openNotificationTarget(offerNotification(), true);
    expect(api).toHaveBeenCalledTimes(1);
    expect(api).toHaveBeenCalledWith("api/session-handoff-offers/received");
    expect(r.opened).toBe(true);
    expect(openSharedSession).toHaveBeenCalledWith("cat-1", true);
    expect(useHandoffStore.getState().received.map((o) => o.id)).toEqual(["o1"]);
  });

  it("says the handoff is closed instead of opening a no-access view once the inbox no longer lists it (H-16)", async () => {
    // The inbox drops every offer that is not pending or whose share was revoked.
    vi.mocked(api).mockResolvedValue({ offers: [offer("o2")] });
    const r = await openNotificationTarget(offerNotification(), false);
    expect(r).toEqual({ opened: false, handoffClosed: true });
    expect(openSharedSession).not.toHaveBeenCalled();
    expect(toast).toHaveBeenCalledWith(t("noti.handoff_closed"), { kind: "warn" });
  });

  it("falls back to the shared view when the inbox cannot be read, rather than calling it closed", async () => {
    for (const fail of [() => Promise.resolve({ error: { code: "internal" } }), () => Promise.reject(new Error("offline"))]) {
      vi.clearAllMocks();
      vi.mocked(api).mockImplementation(fail);
      const r = await openNotificationTarget(offerNotification(), false);
      expect(r.opened).toBe(true);
      expect(openSharedSession).toHaveBeenCalledWith("cat-1", false);
      expect(toast).not.toHaveBeenCalled();
    }
  });
});
