import { describe, it, expect, vi } from "vitest";
import { useLaunchSeed } from "./store.ts";

describe("useLaunchSeed.onLaunched", () => {
  it("is dropped by set() and clear(), so a cancelled dialog leaves no stale callback", () => {
    const fn = vi.fn();
    useLaunchSeed.getState().setOnLaunched(fn);
    useLaunchSeed.getState().set("p");
    expect(useLaunchSeed.getState().onLaunched).toBeNull();
    useLaunchSeed.getState().setOnLaunched(fn);
    useLaunchSeed.getState().clear();
    expect(useLaunchSeed.getState().onLaunched).toBeNull();
    expect(fn).not.toHaveBeenCalled();
  });
});
