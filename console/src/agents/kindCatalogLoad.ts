// Fetches the kind catalog into kindCatalog.ts. Kept apart so registry.ts, which every screen
// imports, does not pull the API client in with it.
import { api } from "../core/api/client.ts";
import { hasKindCatalog, parseKindCatalog, setKindCatalog } from "./kindCatalog.ts";

// ── loading ──────────────────────────────────────────────────────────────────────────
// The answer is fixed per Agent build, so a success lives for the page. It is dropped on a
// tenant switch (a different workspace may run a different Agent build). A failure is not
// cached: the fallback answers meanwhile and the request is retried with a growing delay,
// because the usual cause is a workspace that is still starting.
const RETRY_MS = [3_000, 10_000, 30_000, 60_000];

let inflight: Promise<void> | null = null;
let retryTimer: ReturnType<typeof setTimeout> | null = null;
let attempt = 0;
let epoch = 0;

export function loadKindCatalog(): Promise<void> {
  if (hasKindCatalog()) return Promise.resolve();
  if (inflight) return inflight;
  const mine = epoch;
  const p: Promise<void> = Promise.resolve()
    .then(() => api("/api/agents/kinds"))
    .then(parseKindCatalog)
    .catch(() => null)
    .then((cat) => {
      if (mine !== epoch) return; // reset while in flight: this answer belongs to the old tenant
      inflight = null;
      if (cat) {
        attempt = 0;
        setKindCatalog(cat);
        return;
      }
      const delay = RETRY_MS[Math.min(attempt++, RETRY_MS.length - 1)];
      retryTimer = setTimeout(() => {
        retryTimer = null;
        void loadKindCatalog();
      }, delay);
    });
  inflight = p;
  return p;
}

/** Forgets the catalog and any pending retry; the next loadKindCatalog() asks again. */
export function resetKindCatalog(): void {
  epoch++;
  inflight = null;
  attempt = 0;
  if (retryTimer) clearTimeout(retryTimer);
  retryTimer = null;
  setKindCatalog(null);
}
