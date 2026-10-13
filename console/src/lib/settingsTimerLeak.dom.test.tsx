// Guard for src/test/prefsTimersSetup.ts: a ui-prefs timer scheduled by one test must not
// outlive it (#2015). The pair below reproduces the leak with real timers, as the full suite
// meets it: test 1 leaves the 600 ms debounced save pending and ends, test 2 waits past it.
// Without the shared afterEach, test 1's timer fires during test 2 and calls the mock.
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

const apiMock = vi.fn();
const apiJSONMock = vi.fn();
vi.mock("../core/api/client.ts", () => ({
  api: (...a: unknown[]) => apiMock(...a),
  apiJSON: (...a: unknown[]) => apiJSONMock(...a),
}));

beforeEach(() => {
  apiMock.mockReset().mockResolvedValue({});
  apiJSONMock.mockReset().mockResolvedValue({});
  localStorage.clear();
});

describe("a pending ui-prefs save does not leak past its test", () => {
  it("schedules a save and ends without waiting for it", async () => {
    const s = await import("./settings.ts");
    expect(await s.hydrateUIPrefs()).toBe(true);
    s.setSetting("iconSet", "seti");
    expect(apiJSONMock).not.toHaveBeenCalled(); // still inside the debounce
  });

  it("sees no save from the previous test", async () => {
    await new Promise((r) => setTimeout(r, 900));
    expect(apiJSONMock).not.toHaveBeenCalled();
  });
});

describe("cancelPrefsTimersForTest", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("cancels the save and the hydrate retry and reports what was pending", async () => {
    vi.resetModules();
    const s = await import("./settings.ts");
    apiMock.mockResolvedValue({ error: { code: "http_502", message: "down" } });
    expect(await s.hydrateUIPrefs()).toBe(false);
    s.setSetting("iconSet", "material"); // not loaded: queues a change and arms the retry
    expect(s.cancelPrefsTimersForTest()).toEqual({ save: false, hydrateRetry: true });

    apiMock.mockResolvedValue({});
    await vi.advanceTimersByTimeAsync(70_000);
    expect(apiMock).toHaveBeenCalledTimes(1); // only the failed hydrate above
    expect(apiJSONMock).not.toHaveBeenCalled();
  });
});
