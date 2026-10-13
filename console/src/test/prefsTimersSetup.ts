// Shared afterEach for both vitest projects (see vite.config.js).
//
// settings.ts debounces its ui-prefs save (600 ms) and retries its hydrate on a timer. A test that
// changes a setting leaves that timer behind; it then fires inside whichever test file runs next
// in the same worker, against that file's `api` mock (`missing apiJSON export`, or
// `Cannot read properties of undefined (reading 'then')`). Cancel them when each test ends.
//
// Cancelling silently, not failing: a change to any setting schedules a save, so a test that
// merely exercises a settings-backed component leaves one pending and is not at fault.
import { afterEach } from "vitest";

afterEach(() => {
  const hooks = (globalThis as any)[Symbol.for("af.prefsTimerHooks")] as
    | Set<() => unknown>
    | undefined;
  hooks?.forEach((cancel) => cancel());
});
