// Which window a usage chip shows when the account is near its cap. Shared by every agent's
// chip (claude, codex, muse, agy, copilot) so they cannot disagree.

// Near-cap threshold: at or above this utilization the chip switches from "N% / M%" to a
// reset time, because once you are about to be blocked (or already are) "when does it clear"
// is the more useful answer. Kept in step with the dropdown's crit coloring boundary (95%).
export const NEAR_MAX_PCT = 95;

export interface CapWindow {
  label: string;
  pct: number;
  resetsAt?: string;
  stale?: boolean;
}

// pickNearCap returns the window whose reset is when the block actually lifts, or null.
//
// Candidates are the windows at/above NEAR_MAX_PCT that are a live reading (not stale) and
// whose reset is still ahead. A stale window's percent and reset are both extrapolated, and a
// past instant is "already over", so neither can be what the user waits for. Among the
// candidates the LATEST reset wins: with several windows at the cap, the block lifts only when
// all of them have reset, so the earliest one (the 5-hour window under a capped week) brings
// no relief.
export function pickNearCap<T extends CapWindow>(wins: readonly T[], now: number = Date.now()): T | null {
  let bind: T | null = null;
  let bindAt = -Infinity;
  for (const w of wins) {
    if (w.stale || !w.resetsAt || !(w.pct >= NEAR_MAX_PCT)) continue;
    const at = new Date(w.resetsAt).getTime();
    if (isNaN(at) || at <= now) continue;
    if (at > bindAt) {
      bind = w;
      bindAt = at;
    }
  }
  return bind;
}
