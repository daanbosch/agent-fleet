// What Home lists (#2028) — pure functions only (no store, no clock), so the node vitest project
// pins them. The order itself is the command palette's attention order (sessions/order.ts);
// Home only cuts it into two lists.
import { TIER_WAITING, sessionTier, sortSessionsByAttention } from "../sessions/order.ts";
import type { Session } from "../../types/session.ts";

/** How many sessions "continue" shows. Home is a starting point, not a second overview. */
export const HOME_CONTINUE_MAX = 6;

/** homeLists cuts the attention order into "waiting for you" (every live session waiting for a
 *  person) and "continue" (the next few that can still be opened). A session whose folder is
 *  gone cannot be resumed, so it is never offered as somewhere to continue. */
export function homeLists(sessions: Session[], waitingAt: (name: string) => number): { waiting: Session[]; next: Session[] } {
  const ordered = sortSessionsByAttention(sessions, waitingAt);
  const waiting = ordered.filter((s) => sessionTier(s) === TIER_WAITING);
  const next = ordered
    .filter((s) => sessionTier(s) !== TIER_WAITING && (s.alive || s.resumable !== false))
    .slice(0, HOME_CONTINUE_MAX);
  return { waiting, next };
}
