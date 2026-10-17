// Package usagewin is the one rule every agent's usage chip applies to a quota window whose
// reset has passed since it was read (claude, codex, muse, agy). The Console's near-cap pick
// (console/src/app/usageNearCap.ts) relies on the flag this produces: a window that is past
// its reset must never be offered as the one that lifts the block.
package usagewin

import "time"

// Adjust corrects one recorded window for staleness at `now`. A window whose reset is still
// in the future passes through unchanged. One at or past its reset no longer describes the
// quota: the percent is zeroed, stale is true, and the reset is rolled forward by whole
// windows to the next boundary (not rolled when windowMin <= 0, since the length is
// unknown — the instant then stays in the past and the flag is all the reader has).
//
// stale means the 0 is an assumption, not a reading: it holds only if nothing was spent
// since the capture, and the quota is shared with everything else on the account.
func Adjust(pct float64, windowMin int, reset, now time.Time) (adjPct float64, adjReset time.Time, stale bool) {
	if reset.After(now) {
		return pct, reset, false
	}
	if windowMin > 0 {
		step := time.Duration(windowMin) * time.Minute
		// Advance directly (not a loop) so a very old reading cannot spin.
		n := now.Sub(reset)/step + 1
		reset = reset.Add(step * n)
	}
	return 0, reset, true
}
